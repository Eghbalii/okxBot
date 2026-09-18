package strategy

import "github.com/shopspring/decimal"

// HullSuite is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/hull_suite.pine, @version=4 "Hull Suite Strategy" by
// InSilico/DashTrader), replacing the first pass's from-description implementation (2026-09-16),
// which invented a secondary "smoothed HMA" confirmation layer the real source does not have.
//
// Real parameters, read directly from the source:
//   - HMA length 55 (`length`), mode "Hma" (the source also offers Ehma/Thma variants; Hma is the
//     source's own default).
//
// Signal logic, exactly the source's `if HULL[0] > HULL[2] ... strategy.entry("buy", strategy.long)`
// / `if HULL[0] < HULL[2] ... strategy.entry("sell", strategy.short)`: compare the current HMA value
// against its own value 2 bars ago. This is a SLOPE-DIRECTION flip (rising vs. falling over the last
// 2 bars), not a price-crosses-HMA signal — the source never compares HULL against `src`/close at
// all. The source has no stop-loss or take-profit of any kind (a pure directional
// entry/reverse strategy) — SL/TP are a documented addition here, since every strategy in this
// registry needs one (CLAUDE.md §16.9's "a position opened with no stop-loss" incident).
type HullSuite struct {
	HMALen       int
	SLPct, TPPct decimal.Decimal

	prevHull    decimal.Decimal // HULL[1] from the last call, for the [0]-vs-[2] comparison
	prevPrevHull decimal.Decimal // HULL[2] from the last call
	prevWasUp   bool
	hasPrev     int // 0, 1, or 2+ prior evaluations seeded
}

func NewHullSuite() *HullSuite {
	return &HullSuite{
		HMALen: 55,
		SLPct:  decimal.NewFromFloat(0.008),
		TPPct:  decimal.NewFromFloat(0.016),
	}
}

func (s *HullSuite) Name() string { return "hull_suite" }

func (s *HullSuite) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "hma_len", Default: decimal.NewFromInt(int64(s.HMALen)), Min: decimal.NewFromInt(4), Max: decimal.NewFromInt(400)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *HullSuite) resetState() {
	s.prevHull, s.prevPrevHull = decimal.Zero, decimal.Zero
	s.prevWasUp, s.hasPrev = false, 0
}

func (s *HullSuite) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["hma_len"]; ok {
		cp.HMALen = int(ClampParam(spec["hma_len"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	cp.resetState()
	return &cp
}

// hmaLatest returns HMA(period)'s latest value, bounding the input to only as much trailing
// history as the recurrence actually consumes (period + sqrtLen candles, plus a small margin)
// rather than passing the caller's full, ever-growing candle slice into HMASeries — see the call
// site's own comment for why this matters.
func hmaLatest(candles []Candle, period int) (decimal.Decimal, error) {
	sqrtLen := roundedSqrt(period)
	need := period + sqrtLen + 5
	if need > len(candles) {
		need = len(candles)
	}
	trimmed := candles[len(candles)-need:]
	return HMA(trimmed, period)
}

func (s *HullSuite) Evaluate(candles []Candle) (Signal, error) {
	sqrtLen := s.HMALen / 2
	if sqrtLen < 1 {
		sqrtLen = 1
	}
	if len(candles) < s.HMALen+sqrtLen+2 {
		return Signal{Side: Hold}, nil
	}
	// HMA() computes a full O(len(candles))-length series every call (HMASeries builds the whole
	// history so slope/turning-point strategies can compare consecutive values in one call) — but
	// this strategy already carries state across calls (prevHull/prevPrevHull) and only ever needs
	// the LATEST value. Calling HMA() directly here made every Evaluate O(len(candles)*HMALen), and
	// a driving loop of hundreds of calls over a growing window turned that into a multi-minute hang
	// (found running this file's own test suite) — the same "full series when only the tail is
	// needed" cost class hma_swing.go's own wmaAtOfField fix exists for. hmaLatest bounds the input
	// to only as much history as HMASeries' own recurrence actually consumes.
	hmaNow, err := hmaLatest(candles, s.HMALen)
	if err != nil {
		return Signal{}, err
	}

	// Seed HULL[1] and HULL[2] before the first comparison is possible — mirrors the source's own
	// implicit `na` handling for the first two bars of the series.
	if s.hasPrev < 2 {
		s.prevPrevHull = s.prevHull
		s.prevHull = hmaNow
		s.hasPrev++
		return Signal{Side: Hold}, nil
	}

	wasUp := hmaNow.GreaterThan(s.prevPrevHull)
	wasDown := hmaNow.LessThan(s.prevPrevHull)
	prevWasUp, hasFlip := s.prevWasUp, s.hasPrev >= 3

	s.prevPrevHull = s.prevHull
	s.prevHull = hmaNow
	if wasUp || wasDown {
		s.prevWasUp = wasUp
	}
	if s.hasPrev < 3 {
		s.hasPrev = 3
	}

	// Fire only on the transition (the source's `strategy.entry` re-fires every bar the condition
	// holds too, via pyramiding=1's single-slot re-entry semantics — this package's Strategy
	// interface has no equivalent of "already in this position," so restricting to the transition
	// avoids re-signaling every bar of an established trend).
	switch {
	case wasUp && (!hasFlip || !prevWasUp):
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case wasDown && (!hasFlip || prevWasUp):
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
