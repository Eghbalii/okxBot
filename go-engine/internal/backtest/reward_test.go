package backtest

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

// These values were produced by rl_service/reward.py and are pinned here BY NUMBER.
//
// That is the whole point. This package reimplements the reward in Go so a replay of thousands of
// trades does not cross a process boundary per trade — an accepted duplication, but a duplication
// nonetheless, and the failure it invites is silent: the warm start would optimise a subtly
// different objective than production scores, and nothing would report it. That is the §19.1 skew
// class, which this entire schema rewrite exists to remove.
//
// If one side is changed and the other is not, these fail. Regenerate with:
//
//	rl-service/.venv/bin/python -c 'from rl_service.reward import trade_reward; ...'
func TestGoRewardMatchesThePythonReward(t *testing.T) {
	base := RewardInput{
		RealizedPnLUSD: dec("0.125"),
		FeesUSD:        dec("0.005"),
		RiskPct:        dec("0.066"),
		PositionSize:   dec("2.5"),
		Leverage:       dec("10"),
		EquityUSD:      dec("40"),
		PeakEquityUSD:  dec("40"),
	}

	cases := []struct {
		name string
		in   func(RewardInput) RewardInput
		want float64
	}{
		{"typical win", func(i RewardInput) RewardInput { return i }, 0.7272727272727272},
		{"wide stop", func(i RewardInput) RewardInput { i.RiskPct = dec("0.15"); return i }, 0.32},
		{"50x leverage", func(i RewardInput) RewardInput { i.Leverage = dec("50"); return i }, 0.24727272727272726},
		{"20% drawdown", func(i RewardInput) RewardInput { i.EquityUSD = dec("30"); return i }, 0.6522727272727272},
		{"20 adjustments", func(i RewardInput) RewardInput { i.Adjustments = 20; return i }, 0.6472727272727272},
		{"a loss", func(i RewardInput) RewardInput { i.RealizedPnLUSD = dec("-0.125"); return i }, -0.7878787878787878},
		{"unknown risk falls back to size", func(i RewardInput) RewardInput { i.RiskPct = decimal.Zero; return i }, 0.048},
		{"clipped", func(i RewardInput) RewardInput { i.RealizedPnLUSD = dec("1000"); i.FeesUSD = decimal.Zero; return i }, 3.0},
	}

	for _, tc := range cases {
		got := TradeReward(tc.in(base))
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: Go reward %v, Python reward %v — the two implementations have drifted, "+
				"so the warm start would optimise a different objective than production scores",
				tc.name, got, tc.want)
		}
	}
}

func TestLiquidationPenaltyMatchesPython(t *testing.T) {
	// Free at 10x — deliberate, since 10x is the OKX cap this project trades at and penalising the
	// only leverage available would be a constant, not a signal.
	if got := LiquidationPenalty(10); got != 0.0 {
		t.Errorf("10x must be free, got %v", got)
	}
	if got := LiquidationPenalty(20); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("20x penalty %v, Python says 0.5", got)
	}
	if got := LiquidationPenalty(0); got != 0 {
		t.Errorf("zero leverage is not a penalty, got %v", got)
	}
}

func TestDrawdownIsMeasuredFromThePeak(t *testing.T) {
	if got := DrawdownPct(80, 100); math.Abs(got-0.2) > 1e-12 {
		t.Errorf("got %v want 0.2", got)
	}
	// Above the previous peak is not a drawdown.
	if got := DrawdownPct(120, 100); got != 0 {
		t.Errorf("got %v want 0", got)
	}
}

// A manual close trains nothing (§15.12): attributing an operator's action to the policy would
// score it on a decision it never made.
func TestZeroRewardOverridesEverything(t *testing.T) {
	in := RewardInput{
		RealizedPnLUSD: dec("50"), RiskPct: dec("0.05"), PositionSize: dec("2.5"),
		ZeroReward: true,
	}
	if got := TradeReward(in); got != 0 {
		t.Errorf("a zero-reward close must score 0, got %v", got)
	}
}

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}
