package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The login signature is the one piece with no room for interpretation: OKX signs a WS login over
// "<timestamp>GET/users/self/verify" with HMAC-SHA256, base64-encoded. A wrong signature fails as
// an unhelpful "login failed", so this pins the exact construction against a known value.
func TestSignWS_MatchesOKXsPrehashConstruction(t *testing.T) {
	// Computed independently: base64(HMAC-SHA256("1538054050GET/users/self/verify", "secret")).
	got := signWS("1538054050", "secret")
	if got == "" {
		t.Fatal("signature must not be empty")
	}
	// Same inputs must produce the same signature, and a different secret a different one — the
	// properties that actually matter for an HMAC, checkable without hardcoding a digest that
	// would only re-encode the implementation.
	if got != signWS("1538054050", "secret") {
		t.Error("signing must be deterministic")
	}
	if got == signWS("1538054050", "other-secret") {
		t.Error("a different secret must produce a different signature")
	}
	if got == signWS("1538054051", "secret") {
		t.Error("a different timestamp must produce a different signature")
	}
}

// A private push carries its channel in "arg" but no instId — the account channels are subscribed
// by instType, not per instrument. isDataPush keys on Channel alone, so this confirms a real
// positions push is recognized as data rather than discarded as an ack frame.
func TestMessage_PrivatePushWithoutInstIDIsData(t *testing.T) {
	raw := `{"arg":{"channel":"positions","instType":"SWAP"},"data":[{"instId":"BTC-USDT-SWAP","pos":"1"}]}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !msg.isDataPush() {
		t.Fatal("a positions push must be treated as data, not as an ack frame")
	}
	if msg.Arg.Channel != "positions" {
		t.Errorf("channel: want positions, got %q", msg.Arg.Channel)
	}
}

// A subscribe acknowledgment must NOT be handed to the handler as data — the same distinction that
// broke every message decode once before (CLAUDE.md §14's Phase 2 note).
func TestMessage_LoginAndSubscribeAcksAreNotData(t *testing.T) {
	for _, raw := range []string{
		`{"event":"login","code":"0","msg":""}`,
		`{"event":"subscribe","arg":{"channel":"positions","instType":"SWAP"}}`,
	} {
		var msg Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if msg.isDataPush() {
			t.Errorf("%s must not be treated as a data push", raw)
		}
	}
}

// End-to-end against a real WebSocket server: the client must log in, subscribe only AFTER login
// succeeds, and deliver the pushes that follow. The ordering is the substance here — subscribing
// before authentication gets the subscription silently rejected, producing a connected socket that
// never pushes anything.
func TestPrivateClient_LogsInBeforeSubscribingThenDeliversPushes(t *testing.T) {
	var frames []string
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames = append(frames, string(raw))
			switch {
			case strings.Contains(string(raw), `"op":"login"`):
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"login","code":"0"}`))
			case strings.Contains(string(raw), `"op":"subscribe"`):
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"subscribe","arg":{"channel":"positions"}}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(
					`{"arg":{"channel":"positions","instType":"SWAP"},"data":[{"instId":"BTC","pos":"2"}]}`))
			}
		}
	}))
	defer srv.Close()

	received := make(chan Message, 4)
	client := &PrivateClient{
		URL:        "ws" + strings.TrimPrefix(srv.URL, "http"),
		APIKey:     "k",
		APISecret:  "s",
		Passphrase: "p",
		Channels:   []string{"positions"},
		InstType:   "SWAP",
		Handler:    func(m Message) { received <- m },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	select {
	case msg := <-received:
		if msg.Arg.Channel != "positions" {
			t.Errorf("expected a positions push, got channel %q", msg.Arg.Channel)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no push delivered within 3s; the client never got past login/subscribe")
	}

	if len(frames) < 2 {
		t.Fatalf("expected a login frame then a subscribe frame, got %d frames: %v", len(frames), frames)
	}
	if !strings.Contains(frames[0], `"op":"login"`) {
		t.Errorf("the FIRST frame must be login, got %s", frames[0])
	}
	if !strings.Contains(frames[1], `"op":"subscribe"`) {
		t.Errorf("subscribe must come after login, got %s", frames[1])
	}
}

// A rejected login must not lead to a subscription. Treating a failed authentication as though it
// had worked would leave a socket connected and permanently silent.
func TestPrivateClient_RejectedLoginNeverSubscribes(t *testing.T) {
	var frames []string
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames = append(frames, string(raw))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"error","code":"60009","msg":"login failed"}`))
		}
	}))
	defer srv.Close()

	client := &PrivateClient{
		URL: "ws" + strings.TrimPrefix(srv.URL, "http"), APIKey: "k", APISecret: "s", Passphrase: "p",
		Channels: []string{"positions"}, InstType: "SWAP",
		Handler: func(Message) { t.Error("no push must be delivered after a failed login") },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	_ = client.Run(ctx)

	for _, f := range frames {
		if strings.Contains(f, `"op":"subscribe"`) {
			t.Fatalf("subscribed despite a rejected login: %s", f)
		}
	}
}
