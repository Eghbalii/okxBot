package main

import (
	"testing"

	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// Found 2026-09-15: positionSlots hardcoded `len(instIDs) * 4`, a guess sized for production's
// small live roster, regardless of how many kinds a screening run actually passed. A real run of
// 10 instruments x 34 kinds has 340 concurrent slots; the old formula answered 40 — an 8.5x
// overcommitment that inflated every position's dollar size (and therefore every PnL/reward figure
// in the resulting dataset) by roughly that factor. Win rate was unaffected, since the trade-
// selection logic never consulted this number — only sizing did, which is exactly why the bug was
// invisible in win-rate comparisons and only surfaced when dollar PnL was checked against fees.
func TestPositionSlots_CountsActualKinds(t *testing.T) {
	got := positionSlots([]string{"BTC", "ETH", "SOL"}, []string{"pmax", "stoch_cross"})
	want := 3 * 2 // instruments x kinds, not instruments x 4
	if got != want {
		t.Errorf("positionSlots = %d, want %d (instruments x actual kinds)", got, want)
	}
}

// Empty kinds means "every registered kind" (splitList("") == nil, main's own default), so the
// divisor must count the same set the run will actually iterate — not a fixed per-token guess.
func TestPositionSlots_EmptyKindsCountsTheWholeRegistry(t *testing.T) {
	got := positionSlots([]string{"BTC", "ETH"}, nil)
	want := 2 * len(strategy.Factories)
	if got != want {
		t.Errorf("positionSlots = %d, want %d (instruments x every registered kind)", got, want)
	}
}

func TestPositionSlots_NeverReturnsZeroOrNegative(t *testing.T) {
	if got := positionSlots(nil, nil); got != 1 {
		t.Errorf("positionSlots(nil, nil) = %d, want 1 (never zero, would divide by zero downstream)", got)
	}
}
