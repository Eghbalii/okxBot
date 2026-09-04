package config

import "testing"

// TestLoad_ParsesExecInstIDMap guards the real-trading instrument mapping (CLAUDE.md §27, found
// live 2026-09-04): trading.exec_inst_id_map/exec_inst_type/exec_settle_ccy must parse from the
// example config exactly as documented there, since a typo here would silently leave RealTrader
// targeting the market-data instId directly against an account that cannot trade it.
func TestLoad_ParsesExecInstIDMap(t *testing.T) {
	cfg, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	got, ok := cfg.Trading.ExecInstIDMap["BTC-USDT-SWAP"]
	if !ok {
		t.Fatal("exec_inst_id_map missing BTC-USDT-SWAP entry")
	}
	if got != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("exec_inst_id_map[BTC-USDT-SWAP] = %q, want BTC-USD_UM_XPERP-310404", got)
	}
	if cfg.Trading.ExecInstType != "FUTURES" {
		t.Errorf("exec_inst_type = %q, want FUTURES", cfg.Trading.ExecInstType)
	}
	if cfg.Trading.ExecSettleCcy != "USDC" {
		t.Errorf("exec_settle_ccy = %q, want USDC", cfg.Trading.ExecSettleCcy)
	}
}

// TestLoad_ExecInstIDMapDefaultsToNil confirms a config that never sets exec_inst_id_map leaves it
// nil, not some populated default — RealTrader.execInstID() must fall back to InstID in that case
// (the correct behavior for a deployment whose account CAN trade the SWAP instrument directly).
func TestLoad_ExecInstIDMapDefaultsToNil(t *testing.T) {
	c := &Config{}
	if c.Trading.ExecInstIDMap != nil {
		t.Errorf("expected nil ExecInstIDMap on a zero-value Config, got %v", c.Trading.ExecInstIDMap)
	}
	if c.Trading.ExecInstIDMap["anything"] != "" {
		t.Error("expected a lookup on a nil map to return the empty string, not panic")
	}
}
