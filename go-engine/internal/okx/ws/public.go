// Package ws implements OKX v5 WebSocket clients with auto-reconnect and heartbeat handling.
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

// Message is a decoded OKX WS push message.
type Message struct {
	Event string          `json:"event"` // "subscribe"/"unsubscribe"/"error" for ack/event frames, "" for data pushes
	Arg   Arg             `json:"arg"`
	Data  json.RawMessage `json:"data"`
}

// isDataPush reports whether msg is an actual channel data push (has a "data" field to decode),
// as opposed to a subscribe/unsubscribe/error acknowledgment frame. Ack frames still echo "arg"
// (so a non-empty Arg.Channel alone isn't a reliable signal) but carry no "data" field at all.
func (m Message) isDataPush() bool {
	return m.Event == "" && m.Arg.Channel != ""
}

// Arg identifies the channel/instrument a push message belongs to.
type Arg struct {
	Channel string `json:"channel"`
	InstID  string `json:"instId"`
}

type subscribeReq struct {
	Op   string `json:"op"`
	Args []Arg  `json:"args"`
}

// handlerQueueSize bounds how many decoded data-push messages can be queued for Handler before
// the dispatch goroutine (not the socket read loop) starts blocking. Sized generously — at 10
// instruments x 5 candle channels this process runs, a slow Handler call (e.g. a Kafka publish
// hitting transient broker latency) must never stall the socket read loop itself, which is what
// this queue exists to prevent; see connectAndStream's doc comment for why that mattered in
// practice, not just in theory.
const handlerQueueSize = 256

// PublicClient is a reconnecting client for wss://ws.okx.com:8443/ws/v5/public.
type PublicClient struct {
	URL     string
	Channel string
	InstIDs []string
	Handler func(Message)
	Logger  *slog.Logger
}

// Run connects and streams messages until ctx is cancelled, reconnecting on any error.
// OKX pings with plain-text "ping"/"pong" every ~25s; we also enforce a read deadline so a
// silently dead connection is detected and replaced instead of hanging forever.
func (c *PublicClient) Run(ctx context.Context) error {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}

	wsConnected := metrics.WSConnected.WithLabelValues(c.URL, c.Channel)
	wsReconnects := metrics.WSReconnectsTotal.WithLabelValues(c.URL, c.Channel)
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
			logger.Warn("okx ws connection dropped, reconnecting", "error", err, "backoff", backoff)
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

	args := make([]Arg, 0, len(c.InstIDs))
	for _, id := range c.InstIDs {
		args = append(args, Arg{Channel: c.Channel, InstID: id})
	}
	if err := conn.WriteJSON(subscribeReq{Op: "subscribe", Args: args}); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	wsConnected.Set(1)
	logger.Info("okx ws connected", "url", c.URL, "channel", c.Channel, "instIds", c.InstIDs)

	pingTicker := time.NewTicker(20 * time.Second)
	defer pingTicker.Stop()

	// Handler (a Kafka publish, in every caller this ships with) runs on its own goroutine via
	// this queue, deliberately never inline in the socket read loop below. Live-verified root
	// cause of a real production incident: with Handler called synchronously from the read loop,
	// any latency in the downstream publish stalls ReadMessage from ever being called again —
	// OKX's own send-side buffering then appears to silently favor/coalesce the more frequent
	// forming-candle (confirm=0) pushes over the less frequent but more important finalized
	// (confirm=1) ones, so the connection kept reporting healthy (pings still succeeded, some
	// messages still arrived) while candle finalization silently stopped for hours. A full queue
	// drops the newest message and logs — never blocks the dispatch goroutine either, since a
	// permanently wedged Handler would just reproduce the exact same failure one level down.
	queue := make(chan Message, handlerQueueSize)
	stopDispatch := make(chan struct{})
	defer close(stopDispatch) // stop the dispatch goroutine below on every return path, incl. reconnect
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
				logger.Warn("okx ws: failed to decode message", "error", err, "raw", string(raw))
				continue
			}
			if !msg.isDataPush() {
				if msg.Event == "error" {
					logger.Warn("okx ws subscribe error", "raw", string(raw))
				}
				continue
			}
			select {
			case queue <- msg:
			default:
				logger.Warn("okx ws: handler queue full, dropping message", "url", c.URL, "channel", c.Channel, "instId", msg.Arg.InstID)
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
