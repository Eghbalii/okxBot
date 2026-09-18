package strategy

import "github.com/shopspring/decimal"

// PhilakonesFib is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/philakones_fib.pine, @version=3 "Combined Strategy"), replacing the
// first pass's from-description implementation (2026-09-16), which turned out to already match this
// source closely — the corrections here are the two real deltas the source revealed rather than a
// rewrite from scratch:
//   - The source's short-side Stochastic gate is `kFast > 20` (i.e. NOT yet oversold), not the
//     mirror of the long side's `kFast < 80`. The prior port assumed a symmetric 100-StochBullMax
//     mirror; the real strategy uses an independent lower bound (20) instead.
//   - The source requires `strategy.position_size == 0` before either entry — i.e. it never
//     re-enters while already in a trade. This package has no direct equivalent (PaperTrader/the
//     panel gate one-open-position separately), so it is intentionally not modeled here, same as
//     every other ported strategy in this batch.
//
// Real parameters, read directly from the source:
//   - Five EMAs at Fibonacci lengths 8, 13, 21, 34, 55.
//   - RSI(14): long zone 40 < rsi < 70, short zone 30 < rsi < 60.
//   - Stochastic %K(14) with %D smoothing 3 (the source's `dSlow` is unused by any entry condition):
//     long requires %K < 80, short requires %K > 20.
//
// Signal logic: a long requires the five EMAs stacked in strictly ascending order fast-to-slow
// (8>13>21>34>55) AND RSI inside (40,70) AND %K < 80. A short requires strictly descending EMA order
// AND RSI inside (30,60) AND %K > 20. The source has no explicit stop/target — SL/TP are a
// documented addition here, since every strategy in this registry needs one (CLAUDE.md §16.9's
// "a position opened with no stop-loss" incident).
type PhilakonesFib struct {
	EMA1, EMA2, EMA3, EMA4, EMA5             int // 8, 13, 21, 34, 55
	RSIPeriod                                int
	RSILongMin, RSILongMax                   decimal.Decimal
	RSIShortMin, RSIShortMax                 decimal.Decimal
	StochKPeriod, StochKSmooth, StochDPeriod int
	StochLongMax, StochShortMin              decimal.Decimal
	SLPct, TPPct                             decimal.Decimal
}

func NewPhilakonesFib() *PhilakonesFib {
	return &PhilakonesFib{
		EMA1: 8, EMA2: 13, EMA3: 21, EMA4: 34, EMA5: 55,
		RSIPeriod:    14,
		RSILongMin:   decimal.NewFromInt(40),
		RSILongMax:   decimal.NewFromInt(70),
		RSIShortMin:  decimal.NewFromInt(30),
		RSIShortMax:  decimal.NewFromInt(60),
		StochKPeriod: 14,
		StochKSmooth: 3,
		StochDPeriod: 3,
		StochLongMax: decimal.NewFromInt(80),
		StochShortMin: decimal.NewFromInt(20),
		SLPct:        decimal.NewFromFloat(0.01),
		TPPct:        decimal.NewFromFloat(0.02),
	}
}

func (s *PhilakonesFib) Name() string { return "philakones_fib" }

func (s *PhilakonesFib) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "rsi_period", Default: decimal.NewFromInt(int64(s.RSIPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "rsi_long_min", Default: s.RSILongMin, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(60)},
		{Name: "rsi_long_max", Default: s.RSILongMax, Min: decimal.NewFromInt(50), Max: decimal.NewFromInt(99)},
		{Name: "rsi_short_min", Default: s.RSIShortMin, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(60)},
		{Name: "rsi_short_max", Default: s.RSIShortMax, Min: decimal.NewFromInt(20), Max: decimal.NewFromInt(80)},
		{Name: "stoch_long_max", Default: s.StochLongMax, Min: decimal.NewFromInt(50), Max: decimal.NewFromInt(99)},
		{Name: "stoch_short_min", Default: s.StochShortMin, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(50)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *PhilakonesFib) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["rsi_period"]; ok {
		cp.RSIPeriod = int(ClampParam(spec["rsi_period"], v).IntPart())
	}
	if v, ok := values["rsi_long_min"]; ok {
		cp.RSILongMin = ClampParam(spec["rsi_long_min"], v)
	}
	if v, ok := values["rsi_long_max"]; ok {
		cp.RSILongMax = ClampParam(spec["rsi_long_max"], v)
	}
	if v, ok := values["rsi_short_min"]; ok {
		cp.RSIShortMin = ClampParam(spec["rsi_short_min"], v)
	}
	if v, ok := values["rsi_short_max"]; ok {
		cp.RSIShortMax = ClampParam(spec["rsi_short_max"], v)
	}
	if v, ok := values["stoch_long_max"]; ok {
		cp.StochLongMax = ClampParam(spec["stoch_long_max"], v)
	}
	if v, ok := values["stoch_short_min"]; ok {
		cp.StochShortMin = ClampParam(spec["stoch_short_min"], v)
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	// Stateless: every value Evaluate uses is recomputed fresh from the candles passed in.
	return &cp
}

func (s *PhilakonesFib) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.EMA5, s.StochKPeriod+s.StochKSmooth+s.StochDPeriod, s.RSIPeriod+1) + 1
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	e1, err := EMA(candles, s.EMA1)
	if err != nil {
		return Signal{}, err
	}
	e2, err := EMA(candles, s.EMA2)
	if err != nil {
		return Signal{}, err
	}
	e3, err := EMA(candles, s.EMA3)
	if err != nil {
		return Signal{}, err
	}
	e4, err := EMA(candles, s.EMA4)
	if err != nil {
		return Signal{}, err
	}
	e5, err := EMA(candles, s.EMA5)
	if err != nil {
		return Signal{}, err
	}
	rsi, err := RSI(candles, s.RSIPeriod)
	if err != nil {
		return Signal{}, err
	}
	k, _, err := Stochastic(candles, s.StochKPeriod, s.StochKSmooth, s.StochDPeriod)
	if err != nil {
		return Signal{}, err
	}

	ascending := e1.GreaterThan(e2) && e2.GreaterThan(e3) && e3.GreaterThan(e4) && e4.GreaterThan(e5)
	descending := e1.LessThan(e2) && e2.LessThan(e3) && e3.LessThan(e4) && e4.LessThan(e5)

	rsiLongZone := rsi.GreaterThan(s.RSILongMin) && rsi.LessThan(s.RSILongMax)
	rsiShortZone := rsi.GreaterThan(s.RSIShortMin) && rsi.LessThan(s.RSIShortMax)
	stochLong := k.LessThan(s.StochLongMax)
	stochShort := k.GreaterThan(s.StochShortMin)

	switch {
	case ascending && rsiLongZone && stochLong:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case descending && rsiShortZone && stochShort:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
