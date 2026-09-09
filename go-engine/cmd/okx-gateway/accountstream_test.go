package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/eghbalii/okxBot/go-engine/internal/okx/ws"
)

type capturingPublisher struct {
	keys   []string
	events []accountEvent
	err    error
}

func (p *capturingPublisher) Publish(ctx context.Context, key string, event any) error {
	if p.err != nil {
		return p.err
	}
	raw, _ := json.Marshal(event)
	var decoded accountEvent
	_ = json.Unmarshal(raw, &decoded)
	p.keys = append(p.keys, key)
	p.events = append(p.events, decoded)
	return nil
}

func (p *capturingPublisher) Close() error { return nil }

func testStream(pub *capturingPublisher) *accountStream {
	return &accountStream{
		publisher:      pub,
		instIDToSymbol: map[string]string{"BTC-USD_UM_XPERP-310404": "BTC"},
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// An OKX position push must reach the bus keyed by the SHORT symbol every other service routes on
// (CLAUDE.md §33.4) — publishing the raw execution instId would key the event differently from
// every other topic, and cmd/trader's engine lookup would miss it entirely.
func TestAccountStream_PublishesUnderTheShortSymbol(t *testing.T) {
	pub := &capturingPublisher{}
	stream := testStream(pub)

	stream.handle(context.Background(), ws.Message{
		Arg:  ws.Arg{Channel: "positions"},
		Data: json.RawMessage(`[{"instId":"BTC-USD_UM_XPERP-310404","pos":"2"}]`),
	})

	if len(pub.events) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.events))
	}
	if pub.keys[0] != "BTC" {
		t.Errorf("event must be keyed by the short symbol BTC, got %q", pub.keys[0])
	}
	if pub.events[0].InstID != "BTC" || pub.events[0].Channel != "positions" {
		t.Errorf("unexpected event: %+v", pub.events[0])
	}
}

// OKX batches several rows into one frame. Three updates to the same position are one "go look at
// BTC", not three reconciliation passes.
func TestAccountStream_DeduplicatesRepeatedInstrumentsInOnePush(t *testing.T) {
	pub := &capturingPublisher{}
	stream := testStream(pub)

	stream.handle(context.Background(), ws.Message{
		Arg: ws.Arg{Channel: "orders"},
		Data: json.RawMessage(`[
			{"instId":"BTC-USD_UM_XPERP-310404"},
			{"instId":"BTC-USD_UM_XPERP-310404"},
			{"instId":"BTC-USD_UM_XPERP-310404"}]`),
	})

	if len(pub.events) != 1 {
		t.Fatalf("three rows for one instrument must publish one event, got %d", len(pub.events))
	}
}

// An unmapped instrument is published under its raw instId rather than dropped: an event for an
// instrument this bot does not recognise is something an operator needs to see.
func TestAccountStream_UnmappedInstrumentIsStillPublished(t *testing.T) {
	pub := &capturingPublisher{}
	stream := testStream(pub)

	stream.handle(context.Background(), ws.Message{
		Arg:  ws.Arg{Channel: "positions"},
		Data: json.RawMessage(`[{"instId":"SOMETHING-UNKNOWN"}]`),
	})

	if len(pub.events) != 1 || pub.events[0].InstID != "SOMETHING-UNKNOWN" {
		t.Fatalf("an unmapped instrument must still be published, got %+v", pub.events)
	}
}

// Account-wide rows carry no instrument, so there is nothing to route — publishing an event with an
// empty key would reconcile nothing and only add noise.
func TestAccountStream_SkipsRowsWithNoInstrument(t *testing.T) {
	pub := &capturingPublisher{}
	stream := testStream(pub)

	stream.handle(context.Background(), ws.Message{
		Arg:  ws.Arg{Channel: "account"},
		Data: json.RawMessage(`[{"totalEq":"100"}]`),
	})

	if len(pub.events) != 0 {
		t.Fatalf("an instrument-less row must publish nothing, got %d events", len(pub.events))
	}
}

func TestReverseSymbolMap(t *testing.T) {
	got := reverseSymbolMap(map[string]string{"BTC": "BTC-USD_UM_XPERP-310404", "ETH": "ETH-USD_UM_XPERP-310404"})
	if got["BTC-USD_UM_XPERP-310404"] != "BTC" || got["ETH-USD_UM_XPERP-310404"] != "ETH" {
		t.Fatalf("instId must map back to its symbol, got %v", got)
	}
}
