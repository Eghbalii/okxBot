package tester

import "testing"

func statsWith(wins, losses int64, pnl string) VersionStats {
	return VersionStats{Wins: wins, Losses: losses, RealizedPnL: d(pnl)}
}

func TestScoreFromStats_ComputesWinRateAndCarriesPnL(t *testing.T) {
	s := scoreFromStats("grid_like", 7, statsWith(21, 9, "42.50"))
	if s.TradeCount != 30 {
		t.Fatalf("trade count = %d, want 30", s.TradeCount)
	}
	if !s.WinRatePct.Equal(d("70")) {
		t.Fatalf("win rate = %s, want 70", s.WinRatePct)
	}
	if !s.RealizedPnL.Equal(d("42.50")) {
		t.Fatalf("pnl = %s, want 42.50", s.RealizedPnL)
	}
}

func TestScoreFromStats_ZeroDecidedTradesIsZeroWinRate(t *testing.T) {
	s := scoreFromStats("grid_like", 1, statsWith(0, 0, "0"))
	if !s.WinRatePct.Equal(d("0")) {
		t.Fatalf("win rate = %s, want 0", s.WinRatePct)
	}
}

func TestVersionScore_Eligible(t *testing.T) {
	below := scoreFromStats("k", 1, statsWith(10, 10, "0")) // 20 trades
	if below.Eligible() {
		t.Error("20 trades should not be eligible against a 30-trade floor")
	}
	at := scoreFromStats("k", 1, statsWith(15, 15, "0")) // 30 trades
	if !at.Eligible() {
		t.Error("exactly 30 trades should be eligible")
	}
}

func TestVersionScore_Better_RequiresNotWorseOnEitherDimension(t *testing.T) {
	base := scoreFromStats("k", 1, statsWith(15, 15, "10")) // 50% win, pnl 10

	higherWinSamePnl := scoreFromStats("k", 2, statsWith(18, 12, "10")) // 60% win, pnl 10
	if !higherWinSamePnl.Better(base) {
		t.Error("strictly better win rate with equal pnl should be Better")
	}

	samePnlLowerWin := scoreFromStats("k", 3, statsWith(12, 18, "10")) // 40% win, pnl 10
	if samePnlLowerWin.Better(base) {
		t.Error("strictly worse win rate should never be Better regardless of pnl")
	}

	higherPnlSameWin := scoreFromStats("k", 4, statsWith(15, 15, "20"))
	if !higherPnlSameWin.Better(base) {
		t.Error("strictly better pnl with equal win rate should be Better")
	}

	// Mixed: better win rate but worse PnL must NOT count as Better (operator's explicit
	// "winrate , pnl در کنار هم" — ambiguous improvement is not improvement).
	betterWinWorsePnl := scoreFromStats("k", 5, statsWith(18, 12, "5"))
	if betterWinWorsePnl.Better(base) {
		t.Error("better win rate but worse pnl should NOT be Better (ambiguous, must not count)")
	}
	betterPnlWorseWin := scoreFromStats("k", 6, statsWith(12, 18, "20"))
	if betterPnlWorseWin.Better(base) {
		t.Error("better pnl but worse win rate should NOT be Better (ambiguous, must not count)")
	}

	identical := scoreFromStats("k", 7, statsWith(15, 15, "10"))
	if identical.Better(base) {
		t.Error("identical score should not be Better than itself")
	}
}

func TestBestScore_PicksBestEligibleAcrossHistory(t *testing.T) {
	origin := scoreFromStats("k", 1, statsWith(20, 10, "5")) // 30 trades, 66.6%, eligible
	v2 := scoreFromStats("k", 2, statsWith(25, 5, "20"))     // 30 trades, 83%, eligible, better
	v3 := scoreFromStats("k", 3, statsWith(1, 0, "100"))     // 1 trade, not eligible despite huge pnl

	best, ok := BestScore([]VersionScore{origin, v2, v3})
	if !ok {
		t.Fatal("expected a result")
	}
	if best.VersionID != 2 {
		t.Fatalf("best version = %d, want 2 (v3 must be excluded as ineligible)", best.VersionID)
	}
}

func TestBestScore_FallsBackToFirstWhenNoneEligible(t *testing.T) {
	origin := scoreFromStats("k", 1, statsWith(1, 0, "0")) // 1 trade, not eligible
	candidate := scoreFromStats("k", 2, statsWith(2, 0, "0"))

	best, ok := BestScore([]VersionScore{origin, candidate})
	if !ok {
		t.Fatal("expected a result")
	}
	if best.VersionID != 1 {
		t.Fatalf("expected fallback to the first score (origin) when nothing is eligible, got version %d", best.VersionID)
	}
}

func TestBestScore_EmptyInput(t *testing.T) {
	if _, ok := BestScore(nil); ok {
		t.Error("expected ok=false for empty input")
	}
}

func TestSidecarScore_ClampedToUnitRangeAndNudgedByPnLSign(t *testing.T) {
	positive := sidecarScore(scoreFromStats("k", 1, statsWith(15, 15, "10"))) // 50% win + positive pnl
	negative := sidecarScore(scoreFromStats("k", 2, statsWith(15, 15, "-10")))
	if positive <= negative {
		t.Fatalf("positive pnl (%v) should score higher than negative pnl (%v) at equal win rate", positive, negative)
	}
	if positive < 0 || positive > 1 {
		t.Fatalf("score %v out of [0,1] range", positive)
	}

	allWinsNegativePnl := sidecarScore(scoreFromStats("k", 3, statsWith(30, 0, "-1"))) // 100% win, still nudged down but clamped >= 0
	if allWinsNegativePnl < 0 {
		t.Fatalf("score must not go below 0, got %v", allWinsNegativePnl)
	}
}

func TestStudyID_PrefixedApartFromProductionOptimizer(t *testing.T) {
	got := StudyID("rsi_sma")
	want := "tester:rsi_sma"
	if got != want {
		t.Fatalf("StudyID = %q, want %q", got, want)
	}
	// Production's internal/optimizer.StudyID uses "{inst_id}:{kind}" with no "tester:" prefix —
	// as long as no real inst_id is literally "tester", the namespaces can never collide.
	if got == "tester" {
		t.Fatal("StudyID must not collapse to a bare kind name")
	}
}
