package strategy

import "github.com/shopspring/decimal"

// ScalperMACDPsarEMA200 is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/scalper_macd_psar_ema200.pine, @version=4 "Crypto Scalper" by
// exlux99), replacing the first pass's from-description implementation (2026-09-16), which assumed
// a trend-FOLLOWING combo (long when price above an EMA200 filter and PSAR flips up). The real
// source is a mean-reversion fade against a much shorter EMA and does not require PSAR to have just
// flipped at all — both real mismatches worth stating plainly since they invert the strategy's
// character.
//
// Real parameters, read directly from the source:
//   - EMA length 60 (`len`), not 200 — the "EMA200" in the strategy's public title does not match
//     its own default input.
//   - MACD(12,26,9) — matches the prior port.
//   - Parabolic SAR(0.02, 0.02, 0.2) — matches the prior port (Wilder's own defaults, which the
//     source also happens to use).
//   - tplong=0.245%, sllong=1.0%, tpshort=0.055%, slshort=0.03% — the source's own per-side TP/SL
//     percentages (converted from its `profit=close*X/mintick` tick-count form), asymmetric between
//     long and short by design.
//
// Signal logic, exactly the source's two conditions (note SHORT triggers on an UPTREND SAR state and
// LONG on a DOWNTREND SAR state — a fade against the immediate SAR trend, not a breakout with it):
//   - SHORT: `uptrend AND hist > 0 AND close < ema` — SAR still reads uptrend, MACD histogram is
//     positive, but price has already dipped back below the EMA: fade the move, betting price
//     reverts toward (or below) the EMA before SAR flips.
//   - LONG: `NOT uptrend AND hist < 0 AND close > ema` — the mirror: SAR reads downtrend, histogram
//     negative, but price has already popped back above the EMA.
//
// PSAR's own uptrend/downtrend state (not a fresh flip) is exactly what the source's bare `uptrend`
// boolean reads — no crossover/flip detection is used anywhere in the source's entry conditions.
type ScalperMACDPsarEMA200 struct {
	EMALen                         int
	MACDFast, MACDSlow, MACDSignal int
	SARStart, SARStep, SARMax      decimal.Decimal
	TPLongPct, SLLongPct           decimal.Decimal
	TPShortPct, SLShortPct         decimal.Decimal
}

func NewScalperMACDPsarEMA200() *ScalperMACDPsarEMA200 {
	return &ScalperMACDPsarEMA200{
		EMALen:     60,
		MACDFast:   12,
		MACDSlow:   26,
		MACDSignal: 9,
		SARStart:   decimal.NewFromFloat(0.02),
		SARStep:    decimal.NewFromFloat(0.02),
		SARMax:     decimal.NewFromFloat(0.2),
		TPLongPct:  decimal.NewFromFloat(0.00245),
		SLLongPct:  decimal.NewFromFloat(0.01),
		TPShortPct: decimal.NewFromFloat(0.00055),
		SLShortPct: decimal.NewFromFloat(0.0003),
	}
}

func (s *ScalperMACDPsarEMA200) Name() string { return "scalper_macd_psar_ema200" }

func (s *ScalperMACDPsarEMA200) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "ema_len", Default: decimal.NewFromInt(int64(s.EMALen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(400)},
		{Name: "macd_fast", Default: decimal.NewFromInt(int64(s.MACDFast)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "macd_slow", Default: decimal.NewFromInt(int64(s.MACDSlow)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "macd_signal", Default: decimal.NewFromInt(int64(s.MACDSignal)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "tp_long_pct", Default: s.TPLongPct, Min: decimal.NewFromFloat(0.0001), Max: decimal.NewFromFloat(0.1)},
		{Name: "sl_long_pct", Default: s.SLLongPct, Min: decimal.NewFromFloat(0.0001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_short_pct", Default: s.TPShortPct, Min: decimal.NewFromFloat(0.0001), Max: decimal.NewFromFloat(0.1)},
		{Name: "sl_short_pct", Default: s.SLShortPct, Min: decimal.NewFromFloat(0.0001), Max: decimal.NewFromFloat(0.2)},
	}
}

func (s *ScalperMACDPsarEMA200) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["ema_len"]; ok {
		cp.EMALen = int(ClampParam(spec["ema_len"], v).IntPart())
	}
	if v, ok := values["macd_fast"]; ok {
		cp.MACDFast = int(ClampParam(spec["macd_fast"], v).IntPart())
	}
	if v, ok := values["macd_slow"]; ok {
		cp.MACDSlow = int(ClampParam(spec["macd_slow"], v).IntPart())
	}
	if v, ok := values["macd_signal"]; ok {
		cp.MACDSignal = int(ClampParam(spec["macd_signal"], v).IntPart())
	}
	if v, ok := values["tp_long_pct"]; ok {
		cp.TPLongPct = ClampParam(spec["tp_long_pct"], v)
	}
	if v, ok := values["sl_long_pct"]; ok {
		cp.SLLongPct = ClampParam(spec["sl_long_pct"], v)
	}
	if v, ok := values["tp_short_pct"]; ok {
		cp.TPShortPct = ClampParam(spec["tp_short_pct"], v)
	}
	if v, ok := values["sl_short_pct"]; ok {
		cp.SLShortPct = ClampParam(spec["sl_short_pct"], v)
	}
	// Stateless: PSAR and MACD are both recomputed from scratch over the passed-in window each call.
	return &cp
}

// macdHistSeries returns the current and previous MACD histogram values, computed fresh from the
// series each call — the same "never carry state between calls" reasoning as macd_momentum.go.
func macdHistSeries(candles []Candle, fast, slow, signal int) (now, prev decimal.Decimal, err error) {
	fastSeries, err := EMASeries(candles, fast)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	slowSeries, err := EMASeries(candles, slow)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	validMACD := make([]Candle, 0, len(candles)-(slow-1))
	for i := slow - 1; i < len(candles); i++ {
		validMACD = append(validMACD, Candle{Close: fastSeries[i].Sub(slowSeries[i])})
	}
	signalSeries, err := EMASeries(validMACD, signal)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	n := len(validMACD)
	if n < 2 {
		return decimal.Zero, decimal.Zero, errNeedMore(2, n)
	}
	now = validMACD[n-1].Close.Sub(signalSeries[n-1])
	prev = validMACD[n-2].Close.Sub(signalSeries[n-2])
	return now, prev, nil
}

func (s *ScalperMACDPsarEMA200) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.EMALen, s.MACDSlow+s.MACDSignal+1) + 2
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	ema, err := EMA(candles, s.EMALen)
	if err != nil {
		return Signal{}, err
	}
	histNow, _, err := macdHistSeries(candles, s.MACDFast, s.MACDSlow, s.MACDSignal)
	if err != nil {
		return Signal{}, err
	}
	_, sarTrends, err := ParabolicSARSeries(candles, s.SARStart, s.SARStep, s.SARMax)
	if err != nil {
		return Signal{}, err
	}
	last := len(candles) - 1
	uptrend := sarTrends[last]
	close := candles[last].Close

	switch {
	case uptrend && histNow.IsPositive() && close.LessThan(ema):
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLShortPct, TPPct: s.TPShortPct}, nil
	case !uptrend && histNow.IsNegative() && close.GreaterThan(ema):
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLLongPct, TPPct: s.TPLongPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
