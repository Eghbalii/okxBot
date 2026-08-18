// Package ws implements OKX v5 WebSocket clients with auto-reconnect and heartbeat handling.
package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/gorilla/websocket"
)

// Message is a decoded OKX WS push message.
type Message struct {
	Arg  Arg             `json:"arg"`
	Data json.RawMessage `json:"data"`
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

	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := c.connectAndStream(ctx, logger); err != nil {
			logger.Warn("okx ws connection dropped, reconnecting", "error", err, "backoff", backoff)
		}

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

func (c *PublicClient) connectAndStream(ctx context.Context, logger *slog.Logger) error {
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
	logger.Info("okx ws connected", "url", c.URL, "channel", c.Channel, "instIds", c.InstIDs)

	pingTicker := time.NewTicker(20 * time.Second)
	defer pingTicker.Stop()

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
			if msg.Arg.Channel == "" {
				// subscribe ack / event message, not a data push.
				continue
			}
			if c.Handler != nil {
				c.Handler(msg)
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
