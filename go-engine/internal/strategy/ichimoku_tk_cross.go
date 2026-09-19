package strategy

import "github.com/shopspring/decimal"

// IchimokuTKCross is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/ichimoku_tk_cross.pine, @version=3 "TK Cross > EMA200 Strat"),
// replacing the first pass's from-description implementation (2026-09-16), which used the standard
// Ichimoku Tenkan(9)/Kijun(26) periods and added a symmetric short side neither exists in the source.
//
// Real parameters, read directly from the source:
//   - Conversion Line ("Tenkan") period 20, Base Line ("Kijun") period 60 — both Donchian midpoints
//     (avg(lowest(len), highest(len))), the same construction standard Ichimoku uses, just at the
//     source's own non-standard lengths rather than Ichimoku's usual 9/26.
//   - EMA(200) trend filter, fixed length (not exposed as an input in the source).
//
// Signal logic, exactly the source's `strategy.entry(... when=conversionLine>baseLine and
// close>ema200)` / `strategy.close(... when=conversionLine<baseLine)`: LONG ONLY. Enter when the
// conversion line is above the base line AND price is above EMA200; close (flatten, do not
// reverse into a short) when the conversion line drops back below the base line. The source never
// opens a short position at all.
type IchimokuTKCross struct {
	ConversionPeriod int
	BasePeriod       int
	EMALen           int
	SLPct, TPPct     decimal.Decimal

	wasAboveBase bool
	hasPrev      bool
}

func NewIchimokuTKCross() *IchimokuTKCross {
	return &IchimokuTKCross{
		ConversionPeriod: 20,
		BasePeriod:       60,
		EMALen:           200,
		SLPct:            decimal.NewFromFloat(0.008),
		TPPct:            decimal.NewFromFloat(0.016),
	}
}

func (s *IchimokuTKCross) Name() string { return "ichimoku_tk_cross" }

func (s *IchimokuTKCross) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "conversion_period", Default: decimal.NewFromInt(int64(s.ConversionPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "base_period", Default: decimal.NewFromInt(int64(s.BasePeriod)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "ema_len", Default: decimal.NewFromInt(int64(s.EMALen)), Min: decimal.NewFromInt(20), Max: decimal.NewFromInt(400)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *IchimokuTKCross) resetState() {
	s.wasAboveBase, s.hasPrev = false, false
}

func (s *IchimokuTKCross) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["conversion_period"]; ok {
		cp.ConversionPeriod = int(ClampParam(spec["conversion_period"], v).IntPart())
	}
	if v, ok := values["base_period"]; ok {
		cp.BasePeriod = int(ClampParam(spec["base_period"], v).IntPart())
	}
	if v, ok := values["ema_len"]; ok {
		cp.EMALen = int(ClampParam(spec["ema_len"], v).IntPart())
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

// donchianMid returns the source's `avg(lowest(len), highest(len))` over the trailing `len` candles.
func donchianMid(candles []Candle, period int) (decimal.Decimal, error) {
	hi, err := Highest(candles, period)
	if err != nil {
		return decimal.Zero, err
	}
	lo, err := Lowest(candles, period)
	if err != nil {
		return decimal.Zero, err
	}
	return hi.Add(lo).Div(decimal.NewFromInt(2)), nil
}

func (s *IchimokuTKCross) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.ConversionPeriod, s.BasePeriod, s.EMALen) + 1
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	conversion, err := donchianMid(candles, s.ConversionPeriod)
	if err != nil {
		return Signal{}, err
	}
	base, err := donchianMid(candles, s.BasePeriod)
	if err != nil {
		return Signal{}, err
	}
	ema200, err := EMA(candles, s.EMALen)
	if err != nil {
		return Signal{}, err
	}
	close := candles[len(candles)-1].Close

	aboveBase := conversion.GreaterThan(base)
	wasAbove, hasPrev := s.wasAboveBase, s.hasPrev
	s.wasAboveBase, s.hasPrev = aboveBase, true

	switch {
	case aboveBase && close.GreaterThan(ema200) && (!hasPrev || !wasAbove):
		// Entering long — only on the transition, matching this package's edge-triggered signal
		// convention (the source's own strategy.entry keeps a position open, it does not re-signal).
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case !aboveBase && hasPrev && wasAbove:
		// The source flattens (strategy.close) rather than reversing into a short — modeled as a
		// Sell signal here since this interface has no bare "close" side; PaperTrader/BotTrader
		// treat an opposite-side signal on an open position as an update/close request, not an
		// automatic reversal into a new short (CLAUDE.md §27.3), so this stays faithful to
		// "long-only, exit on cross-down" in practice.
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
