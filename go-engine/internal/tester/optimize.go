package tester

import "github.com/shopspring/decimal"

// Package-level scoring for cmd/strategy-tester's automatic per-kind optimization loop
// (2026-08-31 request). Entirely independent of internal/optimizer (production's
// cmd/strategy-optimizer): no shared types, no shared store, no shared Optuna study namespace —
// see StudyID below. This file holds only pure, dependency-free decision logic so it is
// unit-testable without a database or a running sidecar, matching internal/optimizer/scoring.go's
// precedent.

// MinTradesToScore is how many closed trades a version needs before it is eligible to be compared
// at all (operator's explicit "حداقل ۳۰ تا ترید داشته باشه هر ورژن برای اینکه بتونه مقایسه بشه").
const MinTradesToScore = 30

// VersionScore is one version's scored track record — win rate and PnL considered together (the
// operator's explicit "winrate , pnl در کنار هم و در تعداد بالای ترید"), not win rate alone like
// production's optimizer (CLAUDE.md §16's scoring.go): a version can have a good win rate on
// trades with a poor risk:reward, or vice versa, and this loop is meant to reward genuine edge.
type VersionScore struct {
	VersionID   int64
	Kind        string
	TradeCount  int64
	WinRatePct  decimal.Decimal
	RealizedPnL decimal.Decimal
}

// Eligible reports whether v has accumulated enough trades to be scored/compared at all.
func (v VersionScore) Eligible() bool { return v.TradeCount >= MinTradesToScore }

// scoreFromStats builds a VersionScore from one version's aggregated VersionStats.
func scoreFromStats(kind string, versionID int64, stats VersionStats) VersionScore {
	decided := stats.Wins + stats.Losses
	winRate := decimal.Zero
	if decided > 0 {
		winRate = decimal.NewFromInt(stats.Wins).Div(decimal.NewFromInt(decided)).Mul(decimal.NewFromInt(100))
	}
	return VersionScore{
		VersionID:   versionID,
		Kind:        kind,
		TradeCount:  decided,
		WinRatePct:  winRate,
		RealizedPnL: stats.RealizedPnL,
	}
}

// Better reports whether v scores strictly better than other. Combines win rate and PnL rather
// than picking one (operator's explicit instruction): a version wins only if it is not worse on
// EITHER dimension and strictly better on at least one — a candidate with a higher win rate but
// worse PnL (e.g. many small wins, a few large losses) does not automatically displace one with
// better PnL, and vice versa. This is deliberately conservative: ambiguous improvement does not
// count as improvement, so a genuinely mixed result leaves the existing best version in place
// rather than churning versions on noise.
func (v VersionScore) Better(other VersionScore) bool {
	winNotWorse := v.WinRatePct.GreaterThanOrEqual(other.WinRatePct)
	pnlNotWorse := v.RealizedPnL.GreaterThanOrEqual(other.RealizedPnL)
	winBetter := v.WinRatePct.GreaterThan(other.WinRatePct)
	pnlBetter := v.RealizedPnL.GreaterThan(other.RealizedPnL)
	return winNotWorse && pnlNotWorse && (winBetter || pnlBetter)
}

// BestScore returns the best-scoring ELIGIBLE score among scores, falling back to the first score
// regardless of eligibility if none qualify (e.g. only the origin exists and hasn't traded 30
// times yet — the loop still needs a base to propose the first candidate from). Returns false only
// if scores is empty.
func BestScore(scores []VersionScore) (VersionScore, bool) {
	if len(scores) == 0 {
		return VersionScore{}, false
	}
	var best VersionScore
	found := false
	for _, s := range scores {
		if !s.Eligible() {
			continue
		}
		if !found || s.Better(best) {
			best = s
			found = true
		}
	}
	if found {
		return best, true
	}
	return scores[0], true
}

// StudyID builds this loop's Optuna study id, deliberately prefixed apart from production's own
// "{inst_id}:{kind}" convention (internal/optimizer.StudyID) so the two callers can never collide
// in the shared optimizer-service sidecar — see optimizer_service/api.py's doc comment: studies
// are keyed by an opaque string with no cross-key state, so distinct prefixes are sufficient
// isolation, no separate sidecar deployment needed.
func StudyID(kind string) string {
	return "tester:" + kind
}
