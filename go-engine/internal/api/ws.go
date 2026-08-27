package api

import (
	"log/slog"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// wsHub fans out paper-order open/close events (usecase.PaperOrderEvent, published by
// cmd/paper-trader onto "okx.paper-order-events") to every connected panel WebSocket client
// (CLAUDE.md §11.4's "sound + browser notification on state change" — this replaces polling-based
// diffing on the frontend with a real-time push). Deliberately small/hand-rolled: at this
// project's scale there's no need for a full pub/sub library, just a mutex-guarded client set and
// a broadcast method.
type wsHub struct {
	upgrader websocket.Upgrader
	logger   *slog.Logger

	mu      sync.Mutex
	clients map[*websocket.Conn]chan []byte
}

func newWSHub(logger *slog.Logger) *wsHub {
	return &wsHub{
		// CheckOrigin always allows: the panel is same-origin in production (nginx reverse-proxies
		// /api/ to this service, CLAUDE.md §11) and the only other access path is the OpenVPN
		// tunnel itself, which is the actual access-control boundary (no auth in v1, matching
		// every other cmd/api endpoint).
		upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
		logger:   logger,
		clients:  make(map[*websocket.Conn]chan []byte),
	}
}

// handleWS upgrades the connection and registers it for broadcast until the client disconnects or
// the request context is done (server shutdown).
func (h *wsHub) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("websocket upgrade failed", "error", err)
		}
		return
	}

	send := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[conn] = send
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, conn)
		h.mu.Unlock()
		conn.Close()
	}()

	// A dedicated writer goroutine, since gorilla/websocket forbids concurrent writes to the same
	// connection from multiple goroutines — broadcast() sends here rather than writing directly.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range send {
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()

	// The panel never sends anything meaningful over this connection; ReadMessage's only job here
	// is to detect the client disconnecting (close frame / error) so this handler can clean up.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break
		}
	}
	close(send)
	<-done
}

// broadcast sends msg to every currently-connected client. Non-blocking per client: a slow/stuck
// client's full buffer drops the message rather than stalling every other client's delivery.
func (h *wsHub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for conn, send := range h.clients {
		select {
		case send <- msg:
		default:
			if h.logger != nil {
				h.logger.Warn("dropping websocket event: client send buffer full", "remote", conn.RemoteAddr())
			}
		}
	}
}

// closeAll disconnects every client — called on server shutdown.
func (h *wsHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for conn := range h.clients {
		conn.Close()
	}
}
