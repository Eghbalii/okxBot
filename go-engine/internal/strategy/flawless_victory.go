package strategy

import "github.com/shopspring/decimal"

// FlawlessVictory is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/flawless_victory.pine, @version=4 "Flawless Victory Strategy" by
// Bunghole), replacing the first pass's from-description implementation (2026-09-16), which
// invented a 3-way RSI/MACD/EMA majority-vote system with no basis in the real source — the actual
// source is a single Bollinger Band + RSI confluence, LONG ONLY, with three selectable variants
// (v1/v2/v3) that differ only in their exact thresholds and whether SL/TP are attached. v1 is the
// source's own default-enabled variant and is what this port implements.
//
// Real parameters, read directly from the source (v1 variant):
//   - Bollinger Bands(20, 1) — length 20, 1 standard deviation (NOT the classic 2).
//   - RSI(14), buy guard `rsi > 42`.
//   - Long entry: `close < lower AND rsi > 42`. Long exit (source's own `strategy.close`, no
//     separate stop/target in v1): `close > upper AND rsi > 70`.
//
// v1 has no stop-loss/take-profit of its own (the source's other two variants, v2/v3, do — v2 uses
// 6.604%/2.328%, v3 uses 8.882%/2.317% — but neither is the default-enabled variant, so this port
// keeps v1's own thresholds and adds the SL/TP every strategy in this registry needs, per CLAUDE.md
// §16.9's "no stop-loss" incident, at v2's own published SL/TP percentages since they are the
// source's own documented numbers for this exact BB+RSI construction rather than an invented value.
type FlawlessVictory struct {
	BBPeriod     int
	BBMult       decimal.Decimal
	RSIPeriod    int
	RSIBuyGuard  decimal.Decimal
	RSISellGuard decimal.Decimal
	SLPct, TPPct decimal.Decimal
}

func NewFlawlessVictory() *FlawlessVictory {
	return &FlawlessVictory{
		BBPeriod:     20,
		BBMult:       decimal.NewFromInt(1),
		RSIPeriod:    14,
		RSIBuyGuard:  decimal.NewFromInt(42),
		RSISellGuard: decimal.NewFromInt(70),
		SLPct:        decimal.NewFromFloat(0.06604),
		TPPct:        decimal.NewFromFloat(0.02328),
	}
}

func (s *FlawlessVictory) Name() string { return "flawless_victory" }

func (s *FlawlessVictory) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "bb_period", Default: decimal.NewFromInt(int64(s.BBPeriod)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(200)},
		{Name: "bb_mult", Default: s.BBMult, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(4)},
		{Name: "rsi_period", Default: decimal.NewFromInt(int64(s.RSIPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "rsi_buy_guard", Default: s.RSIBuyGuard, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(80)},
		{Name: "rsi_sell_guard", Default: s.RSISellGuard, Min: decimal.NewFromInt(50), Max: decimal.NewFromInt(99)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.3)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.3)},
	}
}

func (s *FlawlessVictory) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["bb_period"]; ok {
		cp.BBPeriod = int(ClampParam(spec["bb_period"], v).IntPart())
	}
	if v, ok := values["bb_mult"]; ok {
		cp.BBMult = ClampParam(spec["bb_mult"], v)
	}
	if v, ok := values["rsi_period"]; ok {
		cp.RSIPeriod = int(ClampParam(spec["rsi_period"], v).IntPart())
	}
	if v, ok := values["rsi_buy_guard"]; ok {
		cp.RSIBuyGuard = ClampParam(spec["rsi_buy_guard"], v)
	}
	if v, ok := values["rsi_sell_guard"]; ok {
		cp.RSISellGuard = ClampParam(spec["rsi_sell_guard"], v)
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	return &cp
}

func (s *FlawlessVictory) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.BBPeriod, s.RSIPeriod+1) + 1
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	_, upper, lower, err := BollingerBands(candles, s.BBPeriod, s.BBMult)
	if err != nil {
		return Signal{}, err
	}
	rsi, err := RSI(candles, s.RSIPeriod)
	if err != nil {
		return Signal{}, err
	}
	close := candles[len(candles)-1].Close

	switch {
	case close.LessThan(lower) && rsi.GreaterThan(s.RSIBuyGuard):
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case close.GreaterThan(upper) && rsi.GreaterThan(s.RSISellGuard):
		// The source's own exit (strategy.close on an open long), modeled as the opposite-side
		// close signal per this package's convention (CLAUDE.md §27.3) — the source never opens a
		// short from this condition.
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
