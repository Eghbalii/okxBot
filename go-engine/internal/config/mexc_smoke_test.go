package config

import (
	"testing"

	"github.com/shopspring/decimal"
)

// Regression for configs/config.mexc.yaml (2026-09-22, multi-exchange paper trading,
// docs/MEXC_TEST_HANDOFF.md) — this file must stay loadable and pass the same validation every
// other deployment's config.yaml does. A YAML typo here would otherwise only be discovered at
// paper-trader-mexc's own startup on the server.
func TestLoad_MexcConfigFile(t *testing.T) {
	cfg, err := Load("../../configs/config.mexc.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.ValidatePaperTradingBars(); err != nil {
		t.Fatalf("validate paper trading bars: %v", err)
	}
	if len(cfg.Trading.InstIDs) == 0 {
		t.Fatal("trading.inst_ids is empty")
	}
	// MEXC symbols are their own exec id (internal/mexc/adapter.go's IdentitySymbolResolver) —
	// symbol_map still needs an entry per inst_ids symbol since usecase.SeedExecIDs requires one
	// regardless of exchange.
	for _, sym := range cfg.Trading.InstIDs {
		if cfg.Trading.SymbolMap[sym] == "" {
			t.Errorf("trading.inst_ids %q has no symbol_map entry", sym)
		}
	}
	if !cfg.Risk.MaxLeverage.Equal(decimal.NewFromInt(100)) {
		t.Errorf("risk.max_leverage = %s, want 100 (the operator's explicit ask for this test)", cfg.Risk.MaxLeverage)
	}
}
