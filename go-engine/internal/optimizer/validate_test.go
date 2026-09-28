package optimizer

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/backtest"
)

func defaultCfg() ValidationConfig {
	return ValidationConfig{
		RiskProfile:      "low",
		MinTrades:        30,
		MinWinRatePct:    decimal.NewFromInt(45),
		MinRealizedPnL:   decimal.Zero,
		MinSignificanceT: decimal.NewFromInt(2),
		MaxResets:        2,
	}
}

func TestValidate_PassesAGenuinelyGoodCandidate(t *testing.T) {
	result := backtest.Result{
		Resets: 0,
		ByStrategy: map[string]*backtest.KindStats{
			"rsi_sma": {Kind: "rsi_sma", Trades: 50, Wins: 30, WinRate: 0.6, PnLUSD: 12.5, PnLPerTrade: 0.25},
		},
		Significance: []backtest.Significance{
			{Kind: "rsi_sma", T: 2.5, Significant: true},
		},
	}
	v := Validate(result, "rsi_sma", defaultCfg())
	if !v.Passed {
		t.Fatalf("expected pass, got reject: %s", v.Reason)
	}
}

func TestValidate_RejectsTooFewTrades(t *testing.T) {
	result := backtest.Result{
		ByStrategy: map[string]*backtest.KindStats{
			"rsi_sma": {Kind: "rsi_sma", Trades: 5, Wins: 4, WinRate: 0.8, PnLUSD: 5},
		},
	}
	v := Validate(result, "rsi_sma", defaultCfg())
	if v.Passed {
		t.Fatal("expected reject on trade count, got pass")
	}
	if !strings.Contains(v.Reason, "trade count") {
		t.Fatalf("reason should name trade count: %s", v.Reason)
	}
}

func TestValidate_RejectsLowWinRate(t *testing.T) {
	result := backtest.Result{
		ByStrategy: map[string]*backtest.KindStats{
			"rsi_sma": {Kind: "rsi_sma", Trades: 50, Wins: 10, WinRate: 0.2, PnLUSD: 3},
		},
	}
	v := Validate(result, "rsi_sma", defaultCfg())
	if v.Passed {
		t.Fatal("expected reject on win rate, got pass")
	}
	if !strings.Contains(v.Reason, "win rate") {
		t.Fatalf("reason should name win rate: %s", v.Reason)
	}
}

func TestValidate_RejectsNegativePnL(t *testing.T) {
	result := backtest.Result{
		ByStrategy: map[string]*backtest.KindStats{
			"rsi_sma": {Kind: "rsi_sma", Trades: 50, Wins: 30, WinRate: 0.6, PnLUSD: -12.5},
		},
	}
	v := Validate(result, "rsi_sma", defaultCfg())
	if v.Passed {
		t.Fatal("expected reject on PnL, got pass")
	}
	if !strings.Contains(v.Reason, "PnL") {
		t.Fatalf("reason should name PnL: %s", v.Reason)
	}
}

func TestValidate_RejectsTooManyResets(t *testing.T) {
	result := backtest.Result{
		Resets: 5,
		ByStrategy: map[string]*backtest.KindStats{
			"rsi_sma": {Kind: "rsi_sma", Trades: 50, Wins: 30, WinRate: 0.6, PnLUSD: 12.5},
		},
	}
	v := Validate(result, "rsi_sma", defaultCfg())
	if v.Passed {
		t.Fatal("expected reject on resets, got pass")
	}
	if !strings.Contains(v.Reason, "resets") {
		t.Fatalf("reason should name resets: %s", v.Reason)
	}
}

func TestValidate_RejectsInsignificantResultWhenBaselinePresent(t *testing.T) {
	result := backtest.Result{
		ByStrategy: map[string]*backtest.KindStats{
			"rsi_sma": {Kind: "rsi_sma", Trades: 50, Wins: 30, WinRate: 0.6, PnLUSD: 12.5},
		},
		Significance: []backtest.Significance{
			{Kind: "rsi_sma", T: 0.5, Significant: false},
		},
	}
	v := Validate(result, "rsi_sma", defaultCfg())
	if v.Passed {
		t.Fatal("expected reject on significance, got pass")
	}
	if !strings.Contains(v.Reason, "significance") {
		t.Fatalf("reason should name significance: %s", v.Reason)
	}
}

// TestValidate_SkipsSignificanceWhenBaselineAbsent proves the significance check is skipped
// (not treated as an automatic failure) when the caller ran a backtest without coin_flip
// included — a candidate scored on its own, with no null baseline to compare against, must not
// be rejected for a dimension that was never actually measured.
func TestValidate_SkipsSignificanceWhenBaselineAbsent(t *testing.T) {
	result := backtest.Result{
		ByStrategy: map[string]*backtest.KindStats{
			"rsi_sma": {Kind: "rsi_sma", Trades: 50, Wins: 30, WinRate: 0.6, PnLUSD: 12.5},
		},
		// No Significance at all.
	}
	v := Validate(result, "rsi_sma", defaultCfg())
	if !v.Passed {
		t.Fatalf("expected pass when no significance data exists, got reject: %s", v.Reason)
	}
	if v.HasSignificance {
		t.Fatal("HasSignificance should be false when no baseline was measured")
	}
}

func TestValidate_RejectsUnknownKind(t *testing.T) {
	result := backtest.Result{ByStrategy: map[string]*backtest.KindStats{}}
	v := Validate(result, "does_not_exist", defaultCfg())
	if v.Passed {
		t.Fatal("expected reject for a kind with no trades in the result")
	}
}

// TestValidate_ReasonNamesEveryFailure ensures a rejection lists ALL failing dimensions, not just
// the first one encountered — the operator's own "ترکیبی باشه نه فقط تمرکز روی یک چیز" requirement
// means a candidate failing on both win rate AND PnL should say so, not silently stop at win rate.
func TestValidate_ReasonNamesEveryFailure(t *testing.T) {
	result := backtest.Result{
		Resets: 10,
		ByStrategy: map[string]*backtest.KindStats{
			"rsi_sma": {Kind: "rsi_sma", Trades: 5, Wins: 1, WinRate: 0.2, PnLUSD: -50},
		},
	}
	v := Validate(result, "rsi_sma", defaultCfg())
	if v.Passed {
		t.Fatal("expected reject")
	}
	for _, want := range []string{"trade count", "win rate", "PnL", "resets"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("reason %q should mention %q", v.Reason, want)
		}
	}
}
