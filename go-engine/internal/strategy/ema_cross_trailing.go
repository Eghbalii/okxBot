package strategy

import "github.com/shopspring/decimal"

// EMACrossTrailing is a Go port of the Pine Script "Traling.SL.Target" by Sharad_Gaikwad
// (pinescript/strategy_Traling SL Target.pine, MPL 2.0). Entry is a fast/slow EMA crossover,
// flat-only (no reversal while already in a position, matching the source's
// `strategy.position_size == 0` guard). The original's trailing SL/target — which re-anchors by
// TrailProfitPct/TrailSLPct every time price advances IniateTrailingPct beyond the last anchor —
// is per-trade lifecycle state (like SteppedTrailing), not something Evaluate can express as a
// single SLPct/TPPct; only the initial stop/target percentages are carried on the Signal here.
// The point-based (non-percent) variant from the source isn't ported — no "points" concept in
// this domain, and the percent-based path is what the strategy defaults to.
type EMACrossTrailing struct {
	FastPeriod int
	SlowPeriod int
	SLPct      decimal.Decimal
	TPPct      decimal.Decimal
}

func NewEMACrossTrailing() *EMACrossTrailing {
	return &EMACrossTrailing{
		FastPeriod: 20,
		SlowPeriod: 50,
		SLPct:      decimal.NewFromFloat(0.01),
		TPPct:      decimal.NewFromFloat(0.01),
	}
}

func (s *EMACrossTrailing) Name() string { return "ema_cross_trailing" }

func (s *EMACrossTrailing) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "fast_period", Default: decimal.NewFromInt(int64(s.FastPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "slow_period", Default: decimal.NewFromInt(int64(s.SlowPeriod)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *EMACrossTrailing) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["fast_period"]; ok {
		cp.FastPeriod = int(ClampParam(specByName["fast_period"], v).IntPart())
	}
	if v, ok := values["slow_period"]; ok {
		cp.SlowPeriod = int(ClampParam(specByName["slow_period"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	return &cp
}

func (s *EMACrossTrailing) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.SlowPeriod+1 {
		return Signal{Side: Hold}, nil
	}
	fastSeries, err := EMASeries(candles, s.FastPeriod)
	if err != nil {
		return Signal{}, err
	}
	slowSeries, err := EMASeries(candles, s.SlowPeriod)
	if err != nil {
		return Signal{}, err
	}
	last := len(candles) - 1
	fastNow, fastPrev := fastSeries[last], fastSeries[last-1]
	slowNow, slowPrev := slowSeries[last], slowSeries[last-1]

	crossedUp := fastPrev.LessThanOrEqual(slowPrev) && fastNow.GreaterThan(slowNow)
	crossedDown := fastPrev.GreaterThanOrEqual(slowPrev) && fastNow.LessThan(slowNow)

	switch {
	case crossedUp:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case crossedDown:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
