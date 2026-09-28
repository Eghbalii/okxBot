package optimizer

import (
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/backtest"
)

// Verdict is one candidate's validation outcome against a ValidationConfig — every dimension
// checked, not just the first failure, so a rejection reason names everything that was actually
// wrong rather than whichever check happened to run first (the operator's explicit "ترکیبی باشه
// نه فقط تمرکز روی یک چیز").
type Verdict struct {
	Passed bool
	Reason string // human-readable, names every failing dimension — empty when Passed

	TradeCount      int
	WinRatePct      decimal.Decimal
	RealizedPnL     decimal.Decimal
	Resets          int
	SignificanceT   decimal.Decimal
	HasSignificance bool
}

// Validate reads kind's stats out of a completed backtest.Result (produced by running the
// candidate's params through internal/backtest.Runner alongside backtest.BaselineKind, so
// Result.Significance is populated) and checks every dimension of cfg.
//
// coin_flip itself is never validated — a caller must not pass BaselineKind as kind.
func Validate(result backtest.Result, kind string, cfg ValidationConfig) Verdict {
	stats, ok := result.ByStrategy[kind]
	if !ok || stats.Trades == 0 {
		return Verdict{Passed: false, Reason: fmt.Sprintf("no trades for %q in the backtest window", kind)}
	}

	v := Verdict{
		TradeCount:  stats.Trades,
		WinRatePct:  decimal.NewFromFloat(stats.WinRate * 100),
		RealizedPnL: decimal.NewFromFloat(stats.PnLUSD),
		Resets:      result.Resets,
	}
	for _, sig := range result.Significance {
		if sig.Kind == kind {
			v.SignificanceT = decimal.NewFromFloat(sig.T)
			v.HasSignificance = true
			break
		}
	}

	var failures []string
	if v.TradeCount < cfg.MinTrades {
		failures = append(failures, fmt.Sprintf("trade count %d below floor %d", v.TradeCount, cfg.MinTrades))
	}
	if v.WinRatePct.LessThan(cfg.MinWinRatePct) {
		failures = append(failures, fmt.Sprintf("win rate %s%% below floor %s%%", v.WinRatePct.StringFixed(1), cfg.MinWinRatePct.StringFixed(1)))
	}
	if v.RealizedPnL.LessThan(cfg.MinRealizedPnL) {
		failures = append(failures, fmt.Sprintf("realized PnL %s below floor %s", v.RealizedPnL.StringFixed(2), cfg.MinRealizedPnL.StringFixed(2)))
	}
	if v.Resets > cfg.MaxResets {
		failures = append(failures, fmt.Sprintf("account resets %d above ceiling %d", v.Resets, cfg.MaxResets))
	}
	// A significance check only applies when coin_flip was actually included in the run (a caller
	// might validate a candidate on its own, without the baseline, in which case the statistical
	// dimension is silently skipped rather than failing on data that was never asked for).
	if v.HasSignificance && v.SignificanceT.Abs().LessThan(cfg.MinSignificanceT) {
		failures = append(failures, fmt.Sprintf("significance |t|=%s below floor %s — indistinguishable from a null strategy", v.SignificanceT.Abs().StringFixed(2), cfg.MinSignificanceT.StringFixed(2)))
	}

	if len(failures) == 0 {
		v.Passed = true
		return v
	}
	v.Reason = joinReasons(failures)
	return v
}

func joinReasons(reasons []string) string {
	out := reasons[0]
	for _, r := range reasons[1:] {
		out += "; " + r
	}
	return out
}
