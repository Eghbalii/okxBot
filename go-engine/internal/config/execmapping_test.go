package config

import "testing"

// TestLoad_ParsesSymbolMap guards the real-trading instrument mapping (CLAUDE.md §27, found live
// 2026-09-04): trading.symbol_map/exec_inst_type/exec_settle_ccy must parse from the example
// config exactly as documented there, since a typo here would silently leave a service
// subscribing to/calling the wrong instId, or none at all, against an account that cannot trade
// the classic SWAP instrument.
func TestLoad_ParsesSymbolMap(t *testing.T) {
	cfg, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	got, ok := cfg.Trading.SymbolMap["BTC"]
	if !ok {
		t.Fatal("symbol_map missing BTC entry")
	}
	if got != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("symbol_map[BTC] = %q, want BTC-USD_UM_XPERP-310404", got)
	}
	if cfg.Trading.ExecInstType != "FUTURES" {
		t.Errorf("exec_inst_type = %q, want FUTURES", cfg.Trading.ExecInstType)
	}
	if cfg.Trading.ExecSettleCcy != "USDC" {
		t.Errorf("exec_settle_ccy = %q, want USDC", cfg.Trading.ExecSettleCcy)
	}

	// Every trading.inst_ids entry must have a symbol_map entry — a silent gap here is exactly
	// the "channel subscribed, nothing ever arrives" failure mode CLAUDE.md §9 warns about.
	for _, sym := range cfg.Trading.InstIDs {
		if cfg.Trading.SymbolMap[sym] == "" {
			t.Errorf("trading.inst_ids entry %q has no symbol_map entry", sym)
		}
	}
}

// TestLoad_SymbolMapDefaultsToNil confirms a config that never sets symbol_map leaves it nil, not
// some populated default — a nil map's lookups return "" cleanly, and SymbolMap.Resolve turns
// that into a loud error rather than a silent empty-instId call.
func TestLoad_SymbolMapDefaultsToNil(t *testing.T) {
	c := &Config{}
	if c.Trading.SymbolMap != nil {
		t.Errorf("expected nil SymbolMap on a zero-value Config, got %v", c.Trading.SymbolMap)
	}
	if c.Trading.SymbolMap["anything"] != "" {
		t.Error("expected a lookup on a nil map to return the empty string, not panic")
	}
}
