package main

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// TestBuildRLClamps_MapsMaxLossPct is a direct regression test for the 2026-09-01 production
// incident: buildRLClamps' struct literal previously omitted MaxLossPct entirely, so §19.2's
// leverage-aware 15%-loss cap was never applied to a single real paper order — a 20x-leverage
// position (order 636) opened with a 5% price-distance stop, realizing a 100% margin loss on
// touch instead of the intended 15% ceiling. This asserts every configured RLClamps field,
// MaxLossPct included, survives the mapping into conductor.Clamps.
func TestBuildRLClamps_MapsMaxLossPct(t *testing.T) {
	cfg := &config.Config{}
	cfg.PaperTrading.RLClamps.MinSLDistPct = decimal.NewFromFloat(0.005)
	cfg.PaperTrading.RLClamps.MaxSLDistPct = decimal.NewFromFloat(0.05)
	cfg.PaperTrading.RLClamps.MaxLossPct = decimal.NewFromFloat(0.15)
	cfg.PaperTrading.RLClamps.MinTPSLRatio = decimal.NewFromFloat(1.5)

	got := buildRLClamps(cfg)

	if !got.MinSLDistPct.Equal(cfg.PaperTrading.RLClamps.MinSLDistPct) {
		t.Errorf("MinSLDistPct not mapped: got %s", got.MinSLDistPct)
	}
	if !got.MaxSLDistPct.Equal(cfg.PaperTrading.RLClamps.MaxSLDistPct) {
		t.Errorf("MaxSLDistPct not mapped: got %s", got.MaxSLDistPct)
	}
	if !got.MaxLossPct.Equal(cfg.PaperTrading.RLClamps.MaxLossPct) {
		t.Errorf("MaxLossPct not mapped (this is the exact field that caused the production incident): got %s, want %s", got.MaxLossPct, cfg.PaperTrading.RLClamps.MaxLossPct)
	}
	if !got.MinTPSLRatio.Equal(cfg.PaperTrading.RLClamps.MinTPSLRatio) {
		t.Errorf("MinTPSLRatio not mapped: got %s", got.MinTPSLRatio)
	}
}

// TestBuildRLClamps_ProductionScenarioIsNowCaught reproduces order 636's exact numbers (20x
// leverage, entry 0.004533, a naive 5% stop at 0.00430635 — inside MaxSLDistPct=0.05 alone) end-
// to-end through buildRLClamps + the real conductor.Clamps.Apply pipeline evaluateStrategies uses,
// and asserts the resulting stop respects MaxLossPct=0.15 (tightened to 0.75% price distance at
// 20x), not the leverage-blind 5% the production bug actually let through.
func TestBuildRLClamps_ProductionScenarioIsNowCaught(t *testing.T) {
	cfg := &config.Config{}
	cfg.PaperTrading.RLClamps.MinSLDistPct = decimal.NewFromFloat(0.005)
	cfg.PaperTrading.RLClamps.MaxSLDistPct = decimal.NewFromFloat(0.05)
	cfg.PaperTrading.RLClamps.MaxLossPct = decimal.NewFromFloat(0.15)
	clamps := buildRLClamps(cfg)

	entry := decimal.NewFromFloat(0.004533)
	leverage := decimal.NewFromInt(20)
	naiveSL := decimal.NewFromFloat(0.00430635) // order 636's actual, uncapped 5%-distance stop

	out := clamps.Apply("buy", entry, leverage, conductor.Levels{SLPx: &naiveSL})
	if out.SLPx == nil {
		t.Fatal("expected a stop to survive Apply")
	}

	actualLossPct := entry.Sub(*out.SLPx).Div(entry).Mul(leverage)
	maxAllowed := decimal.NewFromFloat(0.15)
	if actualLossPct.GreaterThan(maxAllowed) {
		t.Errorf("stop %s realizes %.4f%% loss at %sx leverage, want <= %.0f%% (this is the exact bug from order 636)",
			out.SLPx, actualLossPct.Mul(decimal.NewFromInt(100)).InexactFloat64(), leverage, maxAllowed.Mul(decimal.NewFromInt(100)).InexactFloat64())
	}
	if !out.SLPx.Equal(naiveSL) {
		t.Logf("stop correctly tightened from %s (5%% naive) to %s (0.75%% capped)", naiveSL, out.SLPx)
	} else {
		t.Errorf("stop was NOT tightened — still at the naive 5%% distance, MaxLossPct had no effect")
	}
}
