// Package optimizer holds the pure, dependency-free trial-scoring/candidate-selection logic for
// cmd/strategy-optimizer (CLAUDE.md §16). Kept separate from the Redis/HTTP-plumbing-heavy
// service code in cmd/strategy-optimizer so the actual decision logic (is this candidate
// eligible, which candidate wins, does it beat the baseline) is unit-testable without a real
// Redis instance or sidecar — matching this repo's existing pattern of pure functions in
// internal/usecase (e.g. RatchetSLTP) backed by table-driven tests.
package optimizer

import "github.com/shopspring/decimal"

// CandidateResult is one candidate parameter set's accumulated trial outcomes over a run
// (CLAUDE.md §16.3: "a candidate accumulates trades over the run duration, it isn't 'one trial =
// one candidate' but 'one candidate = many trades over the run'").
type CandidateResult struct {
	TrialID int // the sidecar's opaque per-candidate trial id (Optuna's ask/tell trial number)
	Params  map[string]decimal.Decimal
	Wins    int // TP touched
	Losses  int // SL touched
}

// TradeCount is how many trades (wins+losses) this candidate has completed so far.
func (c CandidateResult) TradeCount() int { return c.Wins + c.Losses }

// WinRatePct is this candidate's win rate as a percentage (0-100). Returns 0 if it has no
// completed trades yet — callers must check TradeCount()/eligibility separately, this does not
// itself signal "no data."
func (c CandidateResult) WinRatePct() decimal.Decimal {
	total := c.TradeCount()
	if total == 0 {
		return decimal.Zero
	}
	return decimal.NewFromInt(int64(c.Wins)).Div(decimal.NewFromInt(int64(total))).Mul(decimal.NewFromInt(100))
}

// Score is the scalar reported back to the Optuna sidecar via POST /report (CLAUDE.md §16.2) —
// the win rate as a fraction in [0,1], so Optuna's TPE sampler (direction="maximize") converges
// toward parameter sets with a higher SL/TP-touch win rate. A candidate with zero trades reports
// a neutral 0.5 rather than 0.0 — an untested candidate must not look like a confirmed loser to
// the sampler, it just hasn't been evaluated yet (this can happen if a run's time box closes
// before a freshly-asked candidate's strategy ever emits a signal).
func (c CandidateResult) Score() float64 {
	if c.TradeCount() == 0 {
		return 0.5
	}
	f, _ := c.WinRatePct().Div(decimal.NewFromInt(100)).Float64()
	return f
}

// EligibleCandidates filters results to those meeting minTrades — CLAUDE.md §16.3 step 5's
// noise-rejection floor: a candidate with too few completed trades when the run's time box closes
// is simply not eligible to win, regardless of how good its win rate looks on a handful of trades.
func EligibleCandidates(results []CandidateResult, minTrades int) []CandidateResult {
	out := make([]CandidateResult, 0, len(results))
	for _, r := range results {
		if r.TradeCount() >= minTrades {
			out = append(out, r)
		}
	}
	return out
}

// BestCandidate picks the highest win-rate candidate among results, ties broken by trade count
// (more evidence wins a tie) — CLAUDE.md §16.6's "pick the best candidate(s)... ties broken by
// trade count" instruction. Returns false if results is empty.
func BestCandidate(results []CandidateResult) (CandidateResult, bool) {
	if len(results) == 0 {
		return CandidateResult{}, false
	}
	best := results[0]
	for _, r := range results[1:] {
		if r.WinRatePct().GreaterThan(best.WinRatePct()) {
			best = r
			continue
		}
		if r.WinRatePct().Equal(best.WinRatePct()) && r.TradeCount() > best.TradeCount() {
			best = r
		}
	}
	return best, true
}

// Baseline is the current win-rate-of-record for a (inst_id, kind) pair that a winning candidate
// must beat — either the currently-assigned strategy's own track record, or, when no clean
// baseline assignment exists yet, nothing (see ShouldPersist's fallback-floor path).
type Baseline struct {
	Exists     bool
	WinRatePct decimal.Decimal
}

// ShouldPersist decides whether best is good enough to persist as a new sub-strategy
// (CLAUDE.md §16's run-end decision, one of §16.6's "resolve at implementation time" items —
// documented here as: if a clean baseline exists, best must beat it by at least minImprovementPct
// percentage points; otherwise, best must merely clear minWinRateFloorPct outright). Returns
// false (never persist) if best has zero trades — an untested "winner" is not a real result.
func ShouldPersist(best CandidateResult, baseline Baseline, minImprovementPct, minWinRateFloorPct decimal.Decimal) bool {
	if best.TradeCount() == 0 {
		return false
	}
	if baseline.Exists {
		return best.WinRatePct().Sub(baseline.WinRatePct).GreaterThanOrEqual(minImprovementPct)
	}
	return best.WinRatePct().GreaterThanOrEqual(minWinRateFloorPct)
}
