package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// dockerJSONSample is a verbatim excerpt of Docker's real /containers/json response, captured from
// the live socket on 2026-09-13.
//
// Using the real bytes is the point. The first version of this decoder typed Health as a string
// because a field LIST showed the name — and Docker actually sends an object. That failed the whole
// response, blanking every service's state at once rather than just that field, and it only
// surfaced on deploy. A fixture written from the same assumption as the code would have agreed with
// it and proved nothing.
const dockerJSONSample = `[
  {"Names":["/okxbot-kafka-1"],"State":"running","Status":"Up About an hour (healthy)",
   "Health":{"Status":"healthy","FailingStreak":0}},
  {"Names":["/okxbot-trader-1"],"State":"restarting","Status":"Restarting (1) 20 seconds ago"},
  {"Names":["/okxbot-panel-1"],"State":"exited","Status":"Exited (0) 5 minutes ago"}
]`

func TestListContainers_DecodesRealDockerShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dockerJSONSample))
	}))
	defer srv.Close()

	// Point the client at the test server rather than a socket; the decode path is identical.
	client := srv.Client()
	states, err := listContainersFrom(context.Background(), client, srv.URL+"/containers/json?all=1")
	if err != nil {
		t.Fatalf("decode failed: %v — a whole-response failure blanks EVERY service, not one field", err)
	}

	if len(states) != 3 {
		t.Fatalf("got %d containers, want 3", len(states))
	}

	kafka, ok := states["okxbot-kafka-1"]
	if !ok {
		t.Fatal("kafka missing — the leading slash in Names is not being stripped")
	}
	if kafka.State != "running" {
		t.Errorf("kafka state = %q, want running", kafka.State)
	}
	if kafka.Health != "healthy" {
		t.Errorf("kafka health = %q, want healthy — Health is an object, its Status is the string",
			kafka.Health)
	}

	// The crash-loop signal must survive verbatim: it is what distinguishes §47's outage (needs a
	// fix) from §48's self-halt (needs a reset), and flattening it loses the whole distinction.
	trader := states["okxbot-trader-1"]
	if trader.State != "restarting" {
		t.Errorf("trader state = %q, want restarting preserved exactly", trader.State)
	}
	if trader.Status == "" {
		t.Error("status dropped — it carries the restart count and uptime")
	}

	// Docker says "none" for a container with no healthcheck; it must normalize to empty so the
	// panel does not render a badge that reads like a failed check.
	if got := states["okxbot-panel-1"]; got.State != "exited" || got.Health != "" {
		t.Errorf("panel = %+v, want exited with empty health", got)
	}
}
