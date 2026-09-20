package main

import (
	"encoding/json"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
	"testing"
)

// TestRewriteInstID_ReplacesInstIDField guards the ticker path's short-symbol rewrite (CLAUDE.md
// §27, 2026-09-04 design): a downstream consumer reading this Kafka message must see the short
// internal symbol, never OKX's own wire-format instId.
func TestRewriteInstID_ReplacesInstIDField(t *testing.T) {
	raw := json.RawMessage(`{"instId":"BTC-USD_UM_XPERP-310404","last":"81000","askPx":"81001"}`)

	out, err := rewriteInstID(raw, "BTC")
	if err != nil {
		t.Fatalf("rewriteInstID failed: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("decode rewritten payload: %v", err)
	}
	if m["instId"] != "BTC" {
		t.Errorf("instId = %v, want BTC", m["instId"])
	}
	if m["last"] != "81000" {
		t.Errorf("last = %v, want 81000 (other fields must survive the rewrite)", m["last"])
	}
}

func TestRewriteInstID_InvalidJSONErrors(t *testing.T) {
	if _, err := rewriteInstID(json.RawMessage(`not json`), "BTC"); err == nil {
		t.Fatal("expected an error for invalid JSON, got nil")
	}
}

// seedExecIDsFrom is the one-time bridge from config.yaml's symbol_map into the database-backed
// roster (2026-09-13). It replaced reverseSymbolMap, which became dead once the roster's own
// exec_inst_id column took over the lookup — removed rather than left behind with a passing test,
// since a tested function nothing calls reads as load-bearing when it is not.
func TestSeedExecIDsFrom_BuildsTheSeedMap(t *testing.T) {
	m := map[string]string{
		"BTC": "BTC-USD_UM_XPERP-310404",
		"ETH": "ETH-USD_UM_XPERP-310404",
	}
	got, err := usecase.SeedExecIDs(m, []string{"BTC", "ETH"})
	if err != nil {
		t.Fatal(err)
	}
	if got["BTC"] != "BTC-USD_UM_XPERP-310404" || got["ETH"] != "ETH-USD_UM_XPERP-310404" {
		t.Errorf("got %v", got)
	}
}

// A symbol with no map entry must fail the seed rather than write a row with an empty exec id: OKX
// accepts an empty instId as a subscription and then pushes nothing, which is a silent data gap on a
// pipeline that looks perfectly healthy (§9's own reasoning for validating bar-name casing).
func TestSeedExecIDsFrom_FailsOnAMissingEntry(t *testing.T) {
	m := map[string]string{"BTC": "BTC-USD_UM_XPERP-310404"}
	if _, err := usecase.SeedExecIDs(m, []string{"BTC", "MYSTERY"}); err == nil {
		t.Error("a symbol with no symbol_map entry must fail loudly")
	}
}

// toWireLevels converts okx.BookLevel (the merged order book's price/size pair) into cmd/api's
// [4]string wire convention, only populating price/size — the deprecated/numOrders slots have no
// equivalent from a merged book and reshapeBookLevels (cmd/api) never reads them anyway.
func TestToWireLevels_PopulatesOnlyPriceAndSize(t *testing.T) {
	got := toWireLevels([]okx.BookLevel{{Px: "100.5", Sz: "2.3"}, {Px: "101", Sz: "1"}})
	if len(got) != 2 {
		t.Fatalf("expected 2 levels, got %d", len(got))
	}
	if got[0] != (bookLevel{"100.5", "2.3", "", ""}) {
		t.Errorf("level 0 = %v", got[0])
	}
	if got[1] != (bookLevel{"101", "1", "", ""}) {
		t.Errorf("level 1 = %v", got[1])
	}
}

func TestToWireLevels_EmptyInputProducesEmptySlice(t *testing.T) {
	got := toWireLevels(nil)
	if len(got) != 0 {
		t.Fatalf("expected an empty slice, got %v", got)
	}
}

// TestOrderbookEvent_JSONRoundTripsAsAStruct is a regression test for a real bug caught live
// (2026-09-19): the orderbook publish call originally did `raw, _ := json.Marshal(event); pub.
// Publish(ctx, sym, raw)` — passing an already-marshaled []byte to Publish, which itself calls
// json.Marshal internally. A plain []byte has no custom MarshalJSON (unlike json.RawMessage, which
// marshals to itself verbatim and is what the ticker rewrite path correctly uses), so Go's default
// []byte encoding base64-encoded the whole payload into a JSON STRING. cmd/api's consumer then
// failed `json: cannot unmarshal string into Go value of type main.orderbookEvent` on every single
// message — silently, since the handler swallows a decode error — so the Kafka topic filled up and
// the consumer group's offset advanced completely normally while broadcasting nothing to the panel
// at all. Live-verified end to end against the real server before this fix and after.
//
// This test marshals an orderbookEvent exactly once (mirroring the CORRECT call — passing the
// struct itself to Publish, letting it marshal) and confirms the result decodes straight back into
// an orderbookEvent, not a JSON string.
func TestOrderbookEvent_JSONRoundTripsAsAStruct(t *testing.T) {
	event := orderbookEvent{
		InstID: "BTC",
		Asks:   []bookLevel{{"100", "1", "", ""}},
		Bids:   []bookLevel{{"99", "2", "", ""}},
		Ts:     "1789830865405",
	}

	b, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded orderbookEvent
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("a correctly single-marshaled orderbookEvent must decode straight back into one, got: %v", err)
	}
	if decoded.InstID != "BTC" || len(decoded.Asks) != 1 || decoded.Asks[0][0] != "100" {
		t.Fatalf("round-tripped event does not match: %+v", decoded)
	}
}

// TestOrderbookEvent_DoubleMarshalProducesAString reproduces the exact broken shape the bug
// produced, so the failure mode itself is pinned — a future change to Publisher.Publish's own
// signature/behavior that reintroduces this footgun elsewhere should be caught by understanding
// what "broken" looks like here, not just that "correct" works.
func TestOrderbookEvent_DoubleMarshalProducesAString(t *testing.T) {
	event := orderbookEvent{InstID: "BTC"}
	raw, err := json.Marshal(event) // first marshal, as the buggy code did
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	doubleMarshaled, err := json.Marshal(raw) // the bug: Publish marshals this AGAIN
	if err != nil {
		t.Fatalf("marshal raw bytes: %v", err)
	}

	var decoded orderbookEvent
	err = json.Unmarshal(doubleMarshaled, &decoded)
	if err == nil {
		t.Fatal("expected the double-marshaled payload to fail decoding as orderbookEvent (it decodes as a plain string)")
	}
}
