package main

import (
	"os"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
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

// The v8 observation inputs must actually be wired into the engine (docs/RL_V8_PLAN.md).
//
// This is the §23 incident class, and the reason it gets a source-level test rather than a
// behavioural one: BTCCandles and TokenStats are two fields in a ~40-field struct literal, and a
// field dropped from a large literal is invisible — §23 records exactly that costing every real
// position its 15% loss cap. Here the failure is quieter still: a nil BTCCandles makes
// buildObservation fail, which SKIPS every model call, so the service would run indefinitely
// looking healthy while never consulting the model at all.
func TestPaperTraderWiresTheV8ObservationInputs(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, want := range []string{
		"BTCCandles: btcRef.Window",
		"TokenStats: tokenStats.For",
		"errCh <- btcRef.Run(ctx, logger)",
		"errCh <- tokenStats.Run(ctx, logger)",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("cmd/paper-trader no longer wires %q — without it the engine builds no valid "+
				"observation and silently never calls the model", want)
		}
	}
}

// countPositionSlots is the dynamic-sizing divisor (CurrentEquity / PositionSlots). This pins the
// 2026-09-17 revert: it must count distinct TOKENS with at least one enabled assignment, not
// (strategy, token) pairs — the latter under-sized every position by however many strategies were
// assigned to a token, once the open guard went back to one position per TOKEN (papertrade.go's
// own revert of the 2026-09-14 per-strategy trial). Measured in production: 13 strategies turned a
// $4-per-token target into roughly $0.10/position.
func TestCountPositionSlots_CountsTokensNotStrategyPairs(t *testing.T) {
	got := countPositionSlots(map[string][]usecase.StrategyAssignment{
		"BTC": {{StrategyID: 1}, {StrategyID: 2}, {StrategyID: 3}}, // 3 strategies, still 1 token
		"ETH": {{StrategyID: 1}},
		"SOL": {{StrategyID: 4}, {StrategyID: 5}},
	})
	if got != 3 {
		t.Errorf("slots = %d, want 3 (one per token, regardless of how many strategies each carries)", got)
	}
}

// A token present in the map with zero assignments (the panel enabled it, but nothing is assigned
// to trade it yet) must not claim a slot — it can never open, so dividing the account for it would
// shrink every real position's size for a token that will never spend its share.
func TestCountPositionSlots_IgnoresTokensWithNoAssignments(t *testing.T) {
	got := countPositionSlots(map[string][]usecase.StrategyAssignment{
		"BTC": {{StrategyID: 1}},
		"ETH": {}, // enabled, but nothing assigned
	})
	if got != 1 {
		t.Errorf("slots = %d, want 1 — a token with zero assignments must not claim a slot", got)
	}
}

// An empty roster (nothing enabled, or every enabled token has zero assignments) sizes to zero,
// which main() must refuse to divide by rather than falling through to the defensive default of 1
// and opening full-account positions the moment an assignment later appears.
func TestCountPositionSlots_EmptyRosterIsZero(t *testing.T) {
	if got := countPositionSlots(map[string][]usecase.StrategyAssignment{}); got != 0 {
		t.Errorf("slots = %d, want 0", got)
	}
}
