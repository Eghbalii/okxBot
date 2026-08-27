package optimizer

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestCandidateResult_WinRatePct(t *testing.T) {
	tests := []struct {
		name   string
		wins   int
		losses int
		want   string
	}{
		{"no trades", 0, 0, "0"},
		{"all wins", 10, 0, "100"},
		{"all losses", 0, 10, "0"},
		{"mixed 60/40", 6, 4, "60"},
		{"mixed 1/3", 1, 2, "33.33333333333333"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CandidateResult{Wins: tt.wins, Losses: tt.losses}
			got := c.WinRatePct()
			if !got.Equal(dec(tt.want)) {
				t.Errorf("WinRatePct() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCandidateResult_TradeCount(t *testing.T) {
	c := CandidateResult{Wins: 3, Losses: 5}
	if got := c.TradeCount(); got != 8 {
		t.Errorf("TradeCount() = %d, want 8", got)
	}
}

func TestCandidateResult_Score(t *testing.T) {
	tests := []struct {
		name   string
		wins   int
		losses int
		want   float64
	}{
		{"untested candidate is neutral 0.5", 0, 0, 0.5},
		{"all wins scores 1.0", 5, 0, 1.0},
		{"all losses scores 0.0", 0, 5, 0.0},
		{"70% win rate scores 0.7", 7, 3, 0.7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CandidateResult{Wins: tt.wins, Losses: tt.losses}
			if got := c.Score(); got != tt.want {
				t.Errorf("Score() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEligibleCandidates_FiltersByMinTrades(t *testing.T) {
	results := []CandidateResult{
		{TrialID: 1, Wins: 10, Losses: 10}, // 20 trades, eligible at min=15
		{TrialID: 2, Wins: 5, Losses: 4},   // 9 trades, not eligible at min=15
		{TrialID: 3, Wins: 20, Losses: 0},  // 20 trades, eligible
	}
	got := EligibleCandidates(results, 15)
	if len(got) != 2 {
		t.Fatalf("expected 2 eligible candidates, got %d: %+v", len(got), got)
	}
	ids := map[int]bool{got[0].TrialID: true, got[1].TrialID: true}
	if !ids[1] || !ids[3] {
		t.Errorf("expected trial ids 1 and 3 to be eligible, got %+v", got)
	}
}

func TestEligibleCandidates_EmptyInputEmptyOutput(t *testing.T) {
	got := EligibleCandidates(nil, 15)
	if len(got) != 0 {
		t.Errorf("expected empty result, got %+v", got)
	}
}

func TestBestCandidate_PicksHighestWinRate(t *testing.T) {
	results := []CandidateResult{
		{TrialID: 1, Wins: 5, Losses: 5}, // 50%
		{TrialID: 2, Wins: 8, Losses: 2}, // 80%
		{TrialID: 3, Wins: 6, Losses: 4}, // 60%
	}
	best, ok := BestCandidate(results)
	if !ok {
		t.Fatal("expected a best candidate")
	}
	if best.TrialID != 2 {
		t.Errorf("expected trial 2 (80%% win rate) to win, got trial %d", best.TrialID)
	}
}

func TestBestCandidate_TiesBrokenByTradeCount(t *testing.T) {
	results := []CandidateResult{
		{TrialID: 1, Wins: 6, Losses: 4},  // 60%, 10 trades
		{TrialID: 2, Wins: 12, Losses: 8}, // 60%, 20 trades — more evidence, should win the tie
	}
	best, ok := BestCandidate(results)
	if !ok {
		t.Fatal("expected a best candidate")
	}
	if best.TrialID != 2 {
		t.Errorf("expected trial 2 (more trades on a tied win rate) to win, got trial %d", best.TrialID)
	}
}

func TestBestCandidate_EmptyReturnsFalse(t *testing.T) {
	_, ok := BestCandidate(nil)
	if ok {
		t.Error("expected ok=false for empty input")
	}
}

func TestShouldPersist_WithBaseline(t *testing.T) {
	tests := []struct {
		name              string
		candidateWinRate  string
		baselineWinRate   string
		minImprovementPct string
		want              bool
	}{
		{"beats baseline by exactly the margin", "65", "60", "5", true},
		{"beats baseline comfortably", "80", "50", "5", true},
		{"does not beat baseline", "62", "60", "5", false},
		{"equal to baseline, no improvement", "60", "60", "5", false},
		{"below baseline", "40", "60", "5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			winRateFrac := dec(tt.candidateWinRate).Div(dec("100"))
			wins := 100
			losses := 0
			// Reconstruct an exact win/loss split matching the target win rate percentage for a
			// 100-trial candidate, so WinRatePct() returns exactly candidateWinRate.
			w, _ := winRateFrac.Mul(dec("100")).Float64()
			wins = int(w)
			losses = 100 - wins
			candidate := CandidateResult{Wins: wins, Losses: losses}

			baseline := Baseline{Exists: true, WinRatePct: dec(tt.baselineWinRate)}
			got := ShouldPersist(candidate, baseline, dec(tt.minImprovementPct), dec("50"))
			if got != tt.want {
				t.Errorf("ShouldPersist() = %v, want %v (candidate %s%%, baseline %s%%, margin %s)",
					got, tt.want, tt.candidateWinRate, tt.baselineWinRate, tt.minImprovementPct)
			}
		})
	}
}

func TestShouldPersist_NoBaselineUsesFloor(t *testing.T) {
	tests := []struct {
		name   string
		wins   int
		losses int
		floor  string
		want   bool
	}{
		{"clears the floor", 55, 45, "50", true},
		{"exactly at the floor", 50, 50, "50", true},
		{"below the floor", 40, 60, "50", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := CandidateResult{Wins: tt.wins, Losses: tt.losses}
			got := ShouldPersist(candidate, Baseline{Exists: false}, dec("5"), dec(tt.floor))
			if got != tt.want {
				t.Errorf("ShouldPersist() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldPersist_ZeroTradesNeverPersists(t *testing.T) {
	candidate := CandidateResult{Wins: 0, Losses: 0}
	if ShouldPersist(candidate, Baseline{Exists: false}, dec("5"), dec("0")) {
		t.Error("a candidate with zero trades must never be persisted, even against a zero floor")
	}
}

func TestParamsToConfig_RoundTrips(t *testing.T) {
	params := map[string]decimal.Decimal{
		"rsi_period": dec("21"),
		"sl_pct":     dec("0.015"),
	}
	raw, err := ParamsToConfig(params)
	if err != nil {
		t.Fatalf("ParamsToConfig failed: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("expected non-empty config JSON")
	}

	// Round-trip through strategy.FromConfig-style unmarshal to confirm the shape is compatible.
	var decoded map[string]float64
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("failed to unmarshal produced config: %v", err)
	}
	if decoded["rsi_period"] != 21 {
		t.Errorf("expected rsi_period=21, got %v", decoded["rsi_period"])
	}
	if decoded["sl_pct"] != 0.015 {
		t.Errorf("expected sl_pct=0.015, got %v", decoded["sl_pct"])
	}
}
