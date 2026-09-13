package main

import (
	"encoding/json"
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
