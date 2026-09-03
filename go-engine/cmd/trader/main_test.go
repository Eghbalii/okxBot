package main

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// TestBuildRealTraderClamps_MapsMaxLossPct guards against the exact class of bug CLAUDE.md §23
// documents for cmd/paper-trader's own buildRLClamps: a struct literal silently dropping a field
// (there, MaxLossPct — §19.2's leverage-aware 15%-loss cap never actually applied to a real order).
// buildRealTraderClamps is a separate copy of that mapping for RealTrader, so it needs its own
// regression test rather than assuming the paper-trader fix covers it.
func TestBuildRealTraderClamps_MapsMaxLossPct(t *testing.T) {
	cfg := &config.Config{}
	cfg.PaperTrading.RLClamps.MinSLDistPct = decimal.NewFromFloat(0.005)
	cfg.PaperTrading.RLClamps.MaxSLDistPct = decimal.NewFromFloat(0.05)
	cfg.PaperTrading.RLClamps.MaxLossPct = decimal.NewFromFloat(0.15)
	cfg.PaperTrading.RLClamps.MinTPSLRatio = decimal.NewFromFloat(1.5)

	got := buildRealTraderClamps(cfg)

	if !got.MinSLDistPct.Equal(cfg.PaperTrading.RLClamps.MinSLDistPct) {
		t.Errorf("MinSLDistPct not mapped: got %s", got.MinSLDistPct)
	}
	if !got.MaxSLDistPct.Equal(cfg.PaperTrading.RLClamps.MaxSLDistPct) {
		t.Errorf("MaxSLDistPct not mapped: got %s", got.MaxSLDistPct)
	}
	if !got.MaxLossPct.Equal(cfg.PaperTrading.RLClamps.MaxLossPct) {
		t.Errorf("MaxLossPct not mapped (this is the exact field class that caused CLAUDE.md §23's incident): got %s, want %s", got.MaxLossPct, cfg.PaperTrading.RLClamps.MaxLossPct)
	}
	if !got.MinTPSLRatio.Equal(cfg.PaperTrading.RLClamps.MinTPSLRatio) {
		t.Errorf("MinTPSLRatio not mapped: got %s", got.MinTPSLRatio)
	}
}

// TestBuildRealTraderClamps_ProductionScenarioIsNowCaught reproduces CLAUDE.md §23's order-636
// scenario (20x leverage, a naive 5%-distance stop — inside MaxSLDistPct alone) through
// buildRealTraderClamps + the real conductor.Clamps.Apply pipeline, confirming the resulting stop
// respects MaxLossPct rather than the leverage-blind raw distance.
func TestBuildRealTraderClamps_ProductionScenarioIsNowCaught(t *testing.T) {
	cfg := &config.Config{}
	cfg.PaperTrading.RLClamps.MinSLDistPct = decimal.NewFromFloat(0.005)
	cfg.PaperTrading.RLClamps.MaxSLDistPct = decimal.NewFromFloat(0.05)
	cfg.PaperTrading.RLClamps.MaxLossPct = decimal.NewFromFloat(0.15)
	clamps := buildRealTraderClamps(cfg)

	entry := decimal.NewFromFloat(0.004533)
	leverage := decimal.NewFromInt(20)
	naiveSL := decimal.NewFromFloat(0.00430635) // a 5%-distance stop, uncapped

	out := clamps.Apply("buy", entry, leverage, conductor.Levels{SLPx: &naiveSL})
	if out.SLPx == nil {
		t.Fatal("expected a stop to survive Apply")
	}

	actualLossPct := entry.Sub(*out.SLPx).Div(entry).Mul(leverage)
	maxAllowed := decimal.NewFromFloat(0.15)
	if actualLossPct.GreaterThan(maxAllowed) {
		t.Errorf("stop %s realizes %.4f%% loss at %sx leverage, want <= %.0f%%",
			out.SLPx, actualLossPct.Mul(decimal.NewFromInt(100)).InexactFloat64(), leverage, maxAllowed.Mul(decimal.NewFromInt(100)).InexactFloat64())
	}
	if out.SLPx.Equal(naiveSL) {
		t.Error("stop was NOT tightened — still at the naive 5% distance, MaxLossPct had no effect")
	}
}

// TestUseConductorLifecycle_DefaultsOff confirms the config flag defaults to false — the flip must
// be explicit, matching CLAUDE.md §27's plan's "off-by-default safety valve" for the riskiest
// commit in the rollout.
func TestUseConductorLifecycle_DefaultsOff(t *testing.T) {
	cfg := &config.Config{}
	if cfg.Trading.UseConductorLifecycle {
		t.Error("expected UseConductorLifecycle to default to false")
	}
}
