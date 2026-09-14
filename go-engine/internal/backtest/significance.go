package backtest

import "math"

// Significance answers the only question that matters about a new strategy: is it better than
// knowing nothing, by more than chance?
//
// WHY THIS IS IN THE PRODUCT rather than a one-off script. The first full screening ranked 37
// strategies by PnL per trade, and the ranking looked meaningful — until the gaps were tested
// against their own sample sizes. EVERY gap from the coin_flip baseline had t < 1: the entire table
// was one statistical cloud, and coin_flip landing at rank 24 said nothing about coin_flip. Reading
// that table as a ranking would have meant deleting 13 strategies for noise and keeping 23 for
// noise.
//
// So every strategy comparison from here on reports t, and a strategy is only "better" when the
// gap survives its sample size.
type Significance struct {
	Kind string `json:"kind"`
	// Gap is pnl_per_trade minus the baseline's. Positive means better than knowing nothing.
	Gap float64 `json:"gap"`
	// T is the gap in standard errors. |t| >= 2 is the usual bar for "probably not chance"; below
	// that the difference is indistinguishable from sampling noise no matter how large the gap looks.
	T float64 `json:"t"`
	// TradesNeeded is how many trades this strategy would need for its CURRENT gap to reach t=2 —
	// the practical question when a promising result is simply under-sampled.
	TradesNeeded int  `json:"trades_needed"`
	Significant  bool `json:"significant"`
}

// SignificanceVsBaseline compares each strategy against baselineKind.
//
// spread is the standard deviation of per-trade PnL. Estimated from the run rather than assumed:
// using a constant would make the test wrong by exactly the factor the account size changed.
func SignificanceVsBaseline(byStrategy map[string]*KindStats, baselineKind string, spread float64) []Significance {
	base, ok := byStrategy[baselineKind]
	if !ok || spread <= 0 {
		return nil
	}

	out := make([]Significance, 0, len(byStrategy))
	for kind, k := range byStrategy {
		if kind == baselineKind || k.Trades == 0 {
			continue
		}
		gap := k.PnLPerTrade - base.PnLPerTrade
		se := spread / math.Sqrt(float64(k.Trades))
		t := 0.0
		if se > 0 {
			t = gap / se
		}
		s := Significance{Kind: kind, Gap: gap, T: t, Significant: math.Abs(t) >= 2}
		if gap != 0 {
			// n such that gap / (spread/sqrt(n)) = 2
			need := math.Pow(2*spread/math.Abs(gap), 2)
			if need < 1e9 {
				s.TradesNeeded = int(math.Ceil(need))
			}
		}
		out = append(out, s)
	}
	return out
}

// PnLSpread estimates the standard deviation of per-trade PnL across a run.
//
// Computed from the realized win and loss magnitudes rather than assumed, so the test scales with
// the account: the same strategy on a $40 account and a $2,600 one has the same edge and very
// different absolute PnL, and a fixed constant would call one significant and the other not.
func PnLSpread(samples []float64) float64 {
	if len(samples) < 2 {
		return 0
	}
	var mean float64
	for _, v := range samples {
		mean += v
	}
	mean /= float64(len(samples))
	var ss float64
	for _, v := range samples {
		d := v - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(samples)-1))
}

// BaselineKind is the strategy every other is measured against. See internal/strategy/coinflip.go.
const BaselineKind = "coin_flip"
