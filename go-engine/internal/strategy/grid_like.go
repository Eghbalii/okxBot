package strategy

import "github.com/shopspring/decimal"

// GridLike is a Go port of the Pine Script "Grid Like Strategy" by alexgrover
// (pinescript/strategy_Grid Like Strategy.pine, CC BY-SA 4.0).
//
// A baseline tracks price: it only moves when close deviates from the last baseline value by
// more than Point. Each time the baseline moves up, that's a buy signal; each time it moves
// down, that's a sell signal — so trades cluster around a grid spaced Point apart. The original
// script also martingales position size on win/loss streaks (strategy.wintrades/losstrades),
// which isn't observable from Evaluate's candle-only signature — that's account state PaperTrader
// tracks, not something a Strategy computes from price alone. Point is exposed as a tunable; the
// martingale sizing itself is left to the execution layer (order sizing), not modeled here.
type GridLike struct {
	Point decimal.Decimal
	SLPct decimal.Decimal
	TPPct decimal.Decimal

	baseline decimal.Decimal
	hasPrev  bool
}

func NewGridLike(point decimal.Decimal) *GridLike {
	return &GridLike{
		Point: point,
		SLPct: decimal.NewFromFloat(0.01),
		TPPct: decimal.NewFromFloat(0.02),
	}
}

func (s *GridLike) Name() string { return "grid_like" }

func (s *GridLike) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "point", Default: s.Point, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromInt(1000)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *GridLike) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["point"]; ok {
		cp.Point = ClampParam(specByName["point"], v)
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

func (s *GridLike) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) == 0 {
		return Signal{}, nil
	}
	close := candles[len(candles)-1].Close

	if !s.hasPrev {
		s.baseline = close
		s.hasPrev = true
		return Signal{Side: Hold}, nil
	}

	upper := s.baseline.Add(s.Point)
	lower := s.baseline.Sub(s.Point)

	if close.LessThanOrEqual(upper) && close.GreaterThanOrEqual(lower) {
		// Baseline unchanged (Pine's nz(... ? close : baseline[1], close) keeps last value).
		return Signal{Side: Hold}, nil
	}

	prevBaseline := s.baseline
	s.baseline = close

	if s.baseline.GreaterThan(prevBaseline) {
		return Signal{Side: Buy, Confidence: decimal.NewFromInt(1), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	}
	return Signal{Side: Sell, Confidence: decimal.NewFromInt(1), SLPct: s.SLPct, TPPct: s.TPPct}, nil
}

// resetState clears accumulated evaluation state, returning the strategy to how it behaves when
// freshly constructed. Called by WithParams, whose copy must not inherit it (see
// Strategy.WithParams for why).
//
// This lives beside the state fields on purpose: it is the one place that has to know what they
// are, so adding a field means updating the reset right here rather than remembering a zeroing
// line buried at the bottom of WithParams.
// The baseline is the price this grid is anchored to — inheriting it would leave a variant
// measuring its offsets from a price captured during a completely different run.
func (s *GridLike) resetState() {
	s.baseline, s.hasPrev = decimal.Zero, false
}
