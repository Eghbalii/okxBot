package ws

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/gorilla/websocket"

	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
)

// PrivateClient is a reconnecting client for wss://ws.okx.com:8443/ws/v5/private — the account's
// own positions/orders/balance pushes (2026-09-09 request: "check the positions every 5 seconds,
// or if there is a websocket for that, that would be the best choice for keeping positions synced").
//
// It IS the better choice, and this is why: the REST reconciliation poll can only ever notice a
// change one interval after it happened, and it spends rate-limit budget on every poll whether
// anything changed or not. The exchange pushes these events the moment they occur — a fill, a
// liquidation, a stop triggering, a manual close from OKX's own app — so the trading process
// learns about them in roughly the time it takes the message to arrive, and costs nothing while
// the account is idle.
//
// It does NOT replace the poll. A WebSocket can be connected and silently stale, and the whole
// reason this system now rests its stops on the exchange is that a component being up is not
// evidence it is working. The poll stays as the backup that depends on nothing staying connected;
// this is the fast path layered on top (CLAUDE.md §27.6's own "a push channel plus periodic REST
// reconciliation is a reasonable defense-in-depth pair, not a case for picking only one").
//
// Authentication is the one structural difference from PublicClient: a login frame, signed exactly
// the way REST requests are (HMAC-SHA256 over timestamp+method+path), must succeed before any
// subscription is accepted.
type PrivateClient struct {
	URL        string
	APIKey     string
	APISecret  string
	Passphrase string
	// Channels to subscribe to after login, e.g. "positions", "orders", "account". Subscribed with
	// instType rather than instId so one subscription covers every instrument the account trades —
	// a per-instrument subscription would silently miss a position on anything not in the list,
	// which is exactly the untracked-position case this exists to catch.
	Channels []string
	InstType string
	// Handler receives each data push. Runs on its own goroutine via the same bounded queue
	// PublicClient uses, for the same live-verified reason: a slow handler must never stall the
	// socket read loop.
	Handler func(Message)
	Logger  *slog.Logger
}

// loginArg is one entry of the login frame's args array.
type loginArg struct {
	APIKey     string `json:"apiKey"`
	Passphrase string `json:"passphrase"`
	Timestamp  string `json:"timestamp"`
	Sign       string `json:"sign"`
}

type loginReq struct {
	Op   string     `json:"op"`
	Args []loginArg `json:"args"`
}

// privateArg carries instType, which the account channels key on instead of instId.
type privateArg struct {
	Channel  string `json:"channel"`
	InstType string `json:"instType,omitempty"`
}

type privateSubscribeReq struct {
	Op   string       `json:"op"`
	Args []privateArg `json:"args"`
}

// signWS produces the login frame's signature. OKX signs a WS login exactly as it signs a REST
// request, over the fixed prehash "<timestamp>GET/users/self/verify" — the same HMAC-SHA256 +
// base64 construction rest.sign uses, duplicated here (it is four lines) rather than exporting it
// and coupling the ws package to the REST client.
//
// The timestamp is UNIX SECONDS here, not the ISO8601 milliseconds REST uses — a genuine
// difference in OKX's own API, and one that fails as an unhelpful "login failed" rather than
// anything that names the timestamp.
func signWS(timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "GET" + "/users/self/verify"))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Run connects, authenticates, subscribes, and streams until ctx is cancelled, reconnecting with
// the same exponential backoff PublicClient uses.
func (c *PrivateClient) Run(ctx context.Context) error {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}

	wsConnected := metrics.WSConnected.WithLabelValues(c.URL, "private")
	wsReconnects := metrics.WSReconnectsTotal.WithLabelValues(c.URL, "private")
	wsConnected.Set(0)
	defer wsConnected.Set(0)

	backoff := time.Second
	first := true
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if !first {
			wsReconnects.Inc()
		}
		first = false

		if err := c.connectAndStream(ctx, logger); err != nil {
			logger.Warn("okx private ws connection dropped, reconnecting", "error", err, "backoff", backoff)
		}
		wsConnected.Set(0)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// login sends the signed login frame and waits for OKX's response, returning an error unless the
// account was actually authenticated.
//
// This blocks on a read before the main loop starts, deliberately: subscribing before login
// succeeds gets the subscription silently rejected, which would present as a connected socket that
// simply never pushes anything — the exact failure shape this codebase has been bitten by before
// (a mis-cased bar name subscribing to a channel that pushes nothing, CLAUDE.md §9).
func (c *PrivateClient) login(conn *websocket.Conn) error {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	frame := loginReq{Op: "login", Args: []loginArg{{
		APIKey:     c.APIKey,
		Passphrase: c.Passphrase,
		Timestamp:  timestamp,
		Sign:       signWS(timestamp, c.APISecret),
	}}}
	if err := conn.WriteJSON(frame); err != nil {
		return fmt.Errorf("send login: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read login response: %w", err)
		}
		if string(raw) == "pong" {
			continue
		}
		var resp struct {
			Event string `json:"event"`
			Code  string `json:"code"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("decode login response %q: %w", string(raw), err)
		}
		switch resp.Event {
		case "login":
			if resp.Code != "" && resp.Code != "0" {
				return fmt.Errorf("login rejected: code=%s msg=%s", resp.Code, resp.Msg)
			}
			return nil
		case "error":
			return fmt.Errorf("login error: code=%s msg=%s", resp.Code, resp.Msg)
		default:
			// Anything else arriving before the login result is not ours to interpret; keep reading
			// until the deadline rather than guessing that silence means success.
			continue
		}
	}
}

func (c *PrivateClient) connectAndStream(ctx context.Context, logger *slog.Logger) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.URL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.URL, err)
	}
	defer conn.Close()

	if err := c.login(conn); err != nil {
		return fmt.Errorf("authenticate: %w", err)
	}

	args := make([]privateArg, 0, len(c.Channels))
	for _, ch := range c.Channels {
		args = append(args, privateArg{Channel: ch, InstType: c.InstType})
	}
	if err := conn.WriteJSON(privateSubscribeReq{Op: "subscribe", Args: args}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	metrics.WSConnected.WithLabelValues(c.URL, "private").Set(1)
	logger.Info("okx private ws connected", "url", c.URL, "channels", c.Channels, "instType", c.InstType)

	pingTicker := time.NewTicker(20 * time.Second)
	defer pingTicker.Stop()

	queue := make(chan Message, handlerQueueSize)
	stopDispatch := make(chan struct{})
	defer close(stopDispatch)
	go func() {
		for {
			select {
			case msg := <-queue:
				if c.Handler != nil {
					c.Handler(msg)
				}
			case <-stopDispatch:
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		for {
			_ = conn.SetReadDeadline(time.Now().Add(35 * time.Second))
			_, raw, err := conn.ReadMessage()
			if err != nil {
				done <- err
				return
			}
			if string(raw) == "pong" {
				continue
			}
			var msg Message
			if err := json.Unmarshal(raw, &msg); err != nil {
				logger.Warn("okx private ws: failed to decode message", "error", err, "raw", string(raw))
				continue
			}
			if !msg.isDataPush() {
				if msg.Event == "error" {
					logger.Warn("okx private ws subscribe error", "raw", string(raw))
				}
				continue
			}
			select {
			case queue <- msg:
			default:
				logger.Warn("okx private ws: handler queue full, dropping message",
					"url", c.URL, "channel", msg.Arg.Channel)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			return err
		case <-pingTicker.C:
			if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
				return fmt.Errorf("ping: %w", err)
			}
		}
	}
}
