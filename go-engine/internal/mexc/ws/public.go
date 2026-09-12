// Package ws implements MEXC futures WebSocket clients with auto-reconnect and heartbeat handling.
package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
)

// Message is a decoded MEXC WS push.
//
// MEXC's frame shape differs from OKX's in every part (verified live against
// wss://contract.mexc.com/edge, 2026-09-13):
//
//	subscribe:  {"method":"sub.ticker","param":{"symbol":"BTC_USDT"}}
//	ack:        {"channel":"rs.sub.ticker","data":"success","ts":...}
//	data push:  {"channel":"push.ticker","symbol":"BTC_USDT","data":{...},"ts":...}
//
// So the channel name itself distinguishes an ack ("rs." prefix) from a push ("push." prefix),
// where OKX uses a separate "event" field. That difference is load-bearing: CLAUDE.md §14 records
// that misclassifying OKX's ack frames as data pushes broke every message decode, and it only
// surfaced when the client was first run against the live exchange.
type Message struct {
	Channel string          `json:"channel"`
	Symbol  string          `json:"symbol"`
	Data    json.RawMessage `json:"data"`
	TS      int64           `json:"ts"`
}

// isDataPush reports whether msg carries channel data rather than a subscribe acknowledgement.
//
// Checked by prefix rather than by an exact channel list so a channel added later is handled
// correctly by default. An ack's own `data` field is the literal string "success", which would
// otherwise decode into a caller's struct as an empty value — a silent wrong answer rather than an
// error, which is exactly what this guard prevents.
func (m Message) isDataPush() bool {
	return len(m.Channel) > 5 && m.Channel[:5] == "push."
}

// isAck reports whether msg is a subscribe/unsubscribe acknowledgement.
func (m Message) isAck() bool {
	return len(m.Channel) > 3 && m.Channel[:3] == "rs."
}

// subscribeReq is MEXC's subscription frame. One symbol per frame — unlike OKX, which takes an
// array of args in a single subscribe, MEXC's param object describes exactly one subscription, so
// a client with N instruments sends N frames on connect.
type subscribeReq struct {
	Method string            `json:"method"`
	Param  map[string]string `json:"param"`
}

// handlerQueueSize bounds queued pushes before dispatch blocks. Same value and same reasoning as
// the OKX client's: live-verified there that a synchronous Handler in the read loop stalls
// ReadMessage under downstream publish latency, and the exchange's own send-side buffering then
// silently favours frequent forming-candle pushes over the rarer finalized ones — the connection
// keeps reporting healthy while candle finalization stops for hours.
const handlerQueueSize = 4096

// PublicClient is a reconnecting client for MEXC's public futures WebSocket.
type PublicClient struct {
	URL string
	// Method is the subscription method, e.g. "sub.ticker" or "sub.kline".
	Method string
	// Interval is MEXC's own interval name ("Min5"), required for kline subscriptions and ignored
	// for others. Already translated by the caller via rest.IntervalFor — this client never sees
	// this project's internal bar names.
	Interval string
	Symbols  []string
	Handler  func(Message)
	Logger   *slog.Logger
}

// Run connects and streams until ctx is cancelled, reconnecting with backoff on any error.
func (c *PublicClient) Run(ctx context.Context) error {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}

	wsConnected := metrics.WSConnected.WithLabelValues(c.URL, c.Method)
	wsReconnects := metrics.WSReconnectsTotal.WithLabelValues(c.URL, c.Method)
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

		if err := c.connectAndStream(ctx, logger, wsConnected); err != nil {
			logger.Warn("mexc ws connection dropped, reconnecting", "error", err, "backoff", backoff)
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

func (c *PublicClient) connectAndStream(ctx context.Context, logger *slog.Logger, wsConnected prometheus.Gauge) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.URL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.URL, err)
	}
	defer conn.Close()

	// One frame per symbol (see subscribeReq). A failure part-way through leaves the connection
	// subscribed to a subset, so this returns the error and lets the outer loop reconnect from
	// scratch rather than continuing with partial coverage — a silently short subscription is the
	// data-gap failure this codebase keeps rediscovering (§9, §33.5).
	for _, sym := range c.Symbols {
		param := map[string]string{"symbol": sym}
		if c.Interval != "" {
			param["interval"] = c.Interval
		}
		if err := conn.WriteJSON(subscribeReq{Method: c.Method, Param: param}); err != nil {
			return fmt.Errorf("subscribe %s: %w", sym, err)
		}
	}
	wsConnected.Set(1)
	logger.Info("mexc ws connected", "url", c.URL, "method", c.Method, "interval", c.Interval, "symbols", c.Symbols)

	// MEXC expects a JSON {"method":"ping"} rather than OKX's plain-text "ping", and replies with
	// {"channel":"pong"}. Sending OKX's bare text here does not error — it is simply ignored, so
	// the connection dies at the read deadline instead, which reads as an unstable network rather
	// than a protocol mistake.
	pingTicker := time.NewTicker(15 * time.Second)
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
			_ = conn.SetReadDeadline(time.Now().Add(40 * time.Second))
			_, raw, err := conn.ReadMessage()
			if err != nil {
				done <- err
				return
			}

			var msg Message
			if err := json.Unmarshal(raw, &msg); err != nil {
				logger.Warn("mexc ws: failed to decode message", "error", err, "raw", string(raw))
				continue
			}
			if msg.Channel == "pong" {
				continue
			}
			if !msg.isDataPush() {
				// An ack whose payload is not "success" means that subscription is not live, and
				// this client would otherwise sit connected and silent on that symbol forever.
				if msg.isAck() && string(msg.Data) != `"success"` {
					logger.Warn("mexc ws subscribe not acknowledged", "channel", msg.Channel, "raw", string(raw))
				}
				continue
			}
			select {
			case queue <- msg:
			default:
				logger.Warn("mexc ws: handler queue full, dropping message", "url", c.URL, "method", c.Method, "symbol", msg.Symbol)
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
			if err := conn.WriteJSON(map[string]string{"method": "ping"}); err != nil {
				return fmt.Errorf("ping: %w", err)
			}
		}
	}
}
