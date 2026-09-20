package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestWSHub_BroadcastDuringDisconnectDoesNotPanic is a regression test for a real production
// crash (2026-09-19: "panic: send on closed channel", found under real orderbook-broadcast volume
// once a client disconnected mid-stream — see handleWS's own doc comment for the exact race). The
// original handleWS closed a client's send channel in one step and removed it from h.clients in a
// LATER, separately-locked deferred block, leaving a window where broadcast() could observe a
// client still present in the map whose channel was already closed.
//
// This drives the real hub through httptest with many concurrent real WebSocket connections
// repeatedly connecting, disconnecting, and racing broadcast() — run under `go test -race`, which
// is what actually catches this class of bug; a normal run can pass by luck even with the bug
// present, since the race window is narrow.
func TestWSHub_BroadcastDuringDisconnectDoesNotPanic(t *testing.T) {
	hub := newWSHub(slog.Default())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.handleWS(w, r)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Connect/disconnect churn: many goroutines repeatedly dial, read a couple of messages (or
	// none), and close — exactly the client lifecycle whose interleaving with broadcast() crashed
	// the process.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
				if err != nil {
					continue
				}
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
				_, _, _ = conn.ReadMessage()
				conn.Close()
			}
		}()
	}

	// Broadcast continuously and concurrently, exactly like the orderbook consumer does in
	// cmd/api/main.go — this is the goroutine that panicked in production.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			hub.broadcast([]byte(`{"type":"orderbook","instId":"BTC"}`))
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	// Reaching here without a panic (especially under -race) is the assertion.
}
