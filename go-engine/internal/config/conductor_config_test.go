package config

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// TestConductorConfigParses guards the signal-lifecycle conductor's config keys (CLAUDE.md
// §15.12). Worth its own test because a mistyped or unparseable key here fails SILENTLY: yaml.v3
// leaves the field zero, the conductor falls back to its defaults, and the engine runs looking
// perfectly healthy while ignoring what the operator configured. The duration field is the real
// risk — "15m" only parses into a time.Duration if the type is right.
func TestConductorConfigParses(t *testing.T) {
	cfg, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}

	if want := decimal.NewFromFloat(0.01); !cfg.PaperTrading.RLUpdatePnLThresholdPct.Equal(want) {
		t.Errorf("rl_update_pnl_threshold_pct = %v, want %v", cfg.PaperTrading.RLUpdatePnLThresholdPct, want)
	}
	if want := 15 * time.Minute; cfg.PaperTrading.RLUpdateMaxInterval != want {
		t.Errorf("rl_update_max_interval = %v, want %v", cfg.PaperTrading.RLUpdateMaxInterval, want)
	}
	// Early close must default off in the shipped example: it destroys the counterfactual.
	if cfg.PaperTrading.RLEarlyClose {
		t.Error("rl_early_close should ship disabled")
	}

	c := cfg.PaperTrading.RLClamps
	if want := decimal.NewFromFloat(0.005); !c.MinSLDistPct.Equal(want) {
		t.Errorf("min_sl_dist_pct = %v, want %v", c.MinSLDistPct, want)
	}
	if want := decimal.NewFromFloat(0.05); !c.MaxSLDistPct.Equal(want) {
		t.Errorf("max_sl_dist_pct = %v, want %v", c.MaxSLDistPct, want)
	}
	if want := decimal.NewFromFloat(1.5); !c.MinTPSLRatio.Equal(want) {
		t.Errorf("min_tp_sl_ratio = %v, want %v", c.MinTPSLRatio, want)
	}
}
