package strategy

import "github.com/shopspring/decimal"

// PivotReversal is a Go port of the entry logic from the Pine Script "Monthly Returns in
// PineScript Strategies" (pinescript/strategy_Monthly Returns in PineScript Strategies.pine, no
// stated license — public TradingView script). The monthly/yearly P&L table in the original is
// pure reporting and isn't ported. Signal logic: a swing high (pivothigh) confirmed RightBars
// after it forms arms a long-breakout stop order at that pivot's price; a swing low arms a short
// breakout the same way. The Pine version keeps the order pending (stop entry) until price
// trades through the pivot; here Evaluate only fires once, on the candle the breakout actually
// happens, which is the effective equivalent for a strategy that re-evaluates every closed candle.
type PivotReversal struct {
	LeftBars  int
	RightBars int
	SLPct     decimal.Decimal
	TPPct     decimal.Decimal

	armedHigh decimal.Decimal
	armedLow  decimal.Decimal
	hasHigh   bool
	hasLow    bool
}

func NewPivotReversal() *PivotReversal {
	return &PivotReversal{
		LeftBars:  2,
		RightBars: 1,
		SLPct:     decimal.NewFromFloat(0.015),
		TPPct:     decimal.NewFromFloat(0.03),
	}
}

func (s *PivotReversal) Name() string { return "pivot_reversal" }

func (s *PivotReversal) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "left_bars", Default: decimal.NewFromInt(int64(s.LeftBars)), Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(50)},
		{Name: "right_bars", Default: decimal.NewFromInt(int64(s.RightBars)), Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(50)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *PivotReversal) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["left_bars"]; ok {
		cp.LeftBars = int(ClampParam(specByName["left_bars"], v).IntPart())
	}
	if v, ok := values["right_bars"]; ok {
		cp.RightBars = int(ClampParam(specByName["right_bars"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	cp.resetState()
	return &cp
}

// pivotAt reports whether candles[i] is a pivot high/low: its High/Low is strictly the extreme
// among the LeftBars candles before it and RightBars candles after it.
func (s *PivotReversal) pivotAt(candles []Candle, i int) (isHigh, isLow bool) {
	if i-s.LeftBars < 0 || i+s.RightBars >= len(candles) {
		return false, false
	}
	window := candles[i-s.LeftBars : i+s.RightBars+1]
	isHigh, isLow = true, true
	for j, c := range window {
		if j == s.LeftBars {
			continue
		}
		if c.High.GreaterThanOrEqual(candles[i].High) {
			isHigh = false
		}
		if c.Low.LessThanOrEqual(candles[i].Low) {
			isLow = false
		}
	}
	return isHigh, isLow
}

func (s *PivotReversal) Evaluate(candles []Candle) (Signal, error) {
	need := s.LeftBars + s.RightBars + 1
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	// The most recently confirmable pivot is RightBars back from the last candle.
	pivotIdx := len(candles) - 1 - s.RightBars
	if pivotIdx >= s.LeftBars {
		if isHigh, isLow := s.pivotAt(candles, pivotIdx); isHigh {
			s.armedHigh, s.hasHigh = candles[pivotIdx].High, true
		} else if isLow {
			s.armedLow, s.hasLow = candles[pivotIdx].Low, true
		}
	}

	last := candles[len(candles)-1]

	// The armed pivot IS the entry level (CLAUDE.md §15.11): this strategy trades the breakout
	// through that price, so the pivot is where the trade is meant to be taken — not wherever the
	// candle happened to close after running through it. That distinction is the whole reason the
	// observation carries entry_px separately from the live price.
	//
	// The stop deliberately stays percentage-derived: the opposite pivot would be the structural
	// choice, but only one side is armed at a time here, so the other is frequently stale or unset.
	// Reaching for it would sometimes place the stop at a pivot from an unrelated earlier swing,
	// which is worse than an honest fixed distance — a wrong level is more misleading to the model
	// than no level.
	if s.hasHigh && last.High.GreaterThan(s.armedHigh) {
		entry := s.armedHigh
		s.hasHigh = false
		return Signal{
			Side: Buy, Confidence: decimal.NewFromFloat(0.55),
			EntryPx: entry, SLPct: s.SLPct, TPPct: s.TPPct,
		}, nil
	}
	if s.hasLow && last.Low.LessThan(s.armedLow) {
		entry := s.armedLow
		s.hasLow = false
		return Signal{
			Side: Sell, Confidence: decimal.NewFromFloat(0.55),
			EntryPx: entry, SLPct: s.SLPct, TPPct: s.TPPct,
		}, nil
	}
	return Signal{Side: Hold}, nil
}

// resetState clears accumulated evaluation state, returning the strategy to how it behaves when
// freshly constructed. Called by WithParams, whose copy must not inherit it (see
// Strategy.WithParams for why).
//
// This lives beside the state fields on purpose: it is the one place that has to know what they
// are, so adding a field means updating the reset right here rather than remembering a zeroing
// line buried at the bottom of WithParams.
// An armed pivot was found with different LeftBars/RightBars and is not a pivot under the new
// configuration.
func (s *PivotReversal) resetState() {
	s.armedHigh, s.armedLow = decimal.Zero, decimal.Zero
	s.hasHigh, s.hasLow = false, false
}
