package main

import (
	"encoding/json"
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

func TestReverseSymbolMap_BuildsCorrectLookup(t *testing.T) {
	symbols := []string{"BTC", "ETH"}
	resolved := []string{"BTC-USD_UM_XPERP-310404", "ETH-USD_UM_XPERP-310404"}

	got := reverseSymbolMap(symbols, resolved)

	if got["BTC-USD_UM_XPERP-310404"] != "BTC" {
		t.Errorf("got[BTC-USD_UM_XPERP-310404] = %q, want BTC", got["BTC-USD_UM_XPERP-310404"])
	}
	if got["ETH-USD_UM_XPERP-310404"] != "ETH" {
		t.Errorf("got[ETH-USD_UM_XPERP-310404] = %q, want ETH", got["ETH-USD_UM_XPERP-310404"])
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2", len(got))
	}
}
