package main

import (
	"log/slog"
	"strings"
	"testing"
)

// TestBuildAssignmentStrategy_SkipsUnknownKindRatherThanFailing reproduces the exact production
// outage of 2026-09-13 (CLAUDE.md §47).
//
// "bb_squeeze_breakout_v2" was enabled in strategy_assignments while cmd/trader's deployed binary
// predated those strategies. FromConfig failed, the whole assignment load returned an error, and
// main called os.Exit(1) — on every restart, forever. Docker's DNS then could not resolve a
// container that was not running, so the panel's Pause button reported
// "dial tcp: lookup trader on 127.0.0.11:53: no such host", and an open REAL position sat with a
// pending manual-close request that nothing was alive to execute.
//
// The kind used here is the real one from that incident, so this test fails for the same reason
// production did rather than for a constructed approximation of it.
func TestBuildAssignmentStrategy_SkipsUnknownKindRatherThanFailing(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	s, ok := buildAssignmentStrategy("bb_squeeze_breakout_v2_that_does_not_exist", []byte("{}"),
		logger, "BTC", 4304, 27)

	if ok {
		t.Fatal("an unknown strategy kind was accepted — it cannot be built, so it must not be traded")
	}
	if s != nil {
		t.Errorf("expected a nil strategy alongside ok=false, got %T", s)
	}
	// Skipping silently would be its own failure: the operator still needs to know a configured
	// strategy is not running.
	out := logged.String()
	if !strings.Contains(out, "skipping unusable strategy assignment") {
		t.Errorf("the skip was not logged — a strategy silently not trading is invisible:\n%s", out)
	}
	if !strings.Contains(out, "assignmentId=4304") {
		t.Errorf("the log does not identify which assignment to fix:\n%s", out)
	}
}

// TestBuildAssignmentStrategy_BuildsAKnownKind is the anti-vacuity half: if this returned false for
// everything, the test above would pass while no strategy ever traded.
func TestBuildAssignmentStrategy_BuildsAKnownKind(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	s, ok := buildAssignmentStrategy("macd_momentum", []byte("{}"), logger, "BTC", 1, 17)
	if !ok {
		t.Fatal("a registered strategy kind was rejected")
	}
	if s == nil {
		t.Fatal("ok=true but the strategy is nil")
	}
	if s.Name() != "macd_momentum" {
		t.Errorf("built %q, want macd_momentum", s.Name())
	}
}

// TestBuildAssignmentStrategy_BuildsV2Kinds guards the specific gap that caused the outage: once
// this binary is rebuilt with the V2 strategies, every one of them must be buildable. If a future
// change drops one from the registry while assignments still reference it, this fails at build time
// rather than at 4am on a live account.
func TestBuildAssignmentStrategy_BuildsV2Kinds(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	v2 := []string{
		"vwap_reversion_v2", "bb_squeeze_breakout_v2", "range_breakout_v2",
		"keltner_trend_scalp_v2", "ict_fvg_v2", "ict_order_block_v2",
		"ict_liquidity_sweep_v2", "engulfing_reversal_v2", "inside_bar_breakout_v2",
		"macd_momentum_v2", "volume_breakout_v2", "ema_ribbon_pullback_v2",
	}
	for _, kind := range v2 {
		if _, ok := buildAssignmentStrategy(kind, []byte("{}"), logger, "BTC", 1, 1); !ok {
			t.Errorf("%s is not buildable by this binary — enabling it in the database would "+
				"stop that strategy trading", kind)
		}
	}
}
