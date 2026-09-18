package strategy

import "github.com/shopspring/decimal"

// ScalperMACDPsarEMA200 is a Go port of the widely-known TradingView scalping combo commonly
// titled "Crypto Scalper: Divergence MACD + PSAR + EMA200" (raw PineScript source not extractable
// via automated fetch; implemented from the standard, well-documented indicators it combines).
//
// Standard/default parameters used:
//   - MACD(12,26,9) — the universal standard MACD defaults.
//   - Parabolic SAR(0.02, 0.02, 0.2) — Wilder's own published defaults.
//   - EMA(200) — the standard long-term trend filter length.
//
// Signal logic: a long requires price above EMA200 (uptrend filter) AND the Parabolic SAR just
// flipping to uptrend (dot moves below price) AND the MACD histogram rising (accelerating momentum,
// the "divergence" component simplified to histogram direction — true swing-based bullish/bearish
// divergence detection needs multi-bar pivot tracking that the source's screener/divergence-drawing
// layer performs visually; the core actionable signal for a scalp entry is the histogram itself
// turning in the trade's favor at the same moment PSAR flips). Mirrored for a short.
type ScalperMACDPsarEMA200 struct {
	EMALen                         int
	MACDFast, MACDSlow, MACDSignal int
	SARStart, SARStep, SARMax      decimal.Decimal
	SLPct, TPPct                   decimal.Decimal
}

func NewScalperMACDPsarEMA200() *ScalperMACDPsarEMA200 {
	return &ScalperMACDPsarEMA200{
		EMALen:     200,
		MACDFast:   12,
		MACDSlow:   26,
		MACDSignal: 9,
		SARStart:   decimal.NewFromFloat(0.02),
		SARStep:    decimal.NewFromFloat(0.02),
		SARMax:     decimal.NewFromFloat(0.2),
		SLPct:      decimal.NewFromFloat(0.006),
		TPPct:      decimal.NewFromFloat(0.01),
	}
}

func (s *ScalperMACDPsarEMA200) Name() string { return "scalper_macd_psar_ema200" }

func (s *ScalperMACDPsarEMA200) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "ema_len", Default: decimal.NewFromInt(int64(s.EMALen)), Min: decimal.NewFromInt(20), Max: decimal.NewFromInt(400)},
		{Name: "macd_fast", Default: decimal.NewFromInt(int64(s.MACDFast)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "macd_slow", Default: decimal.NewFromInt(int64(s.MACDSlow)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "macd_signal", Default: decimal.NewFromInt(int64(s.MACDSignal)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
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
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	// Stateless: PSAR and MACD are both recomputed from scratch over the passed-in window each call.
	return &cp
}

// macdHistPair returns the current and previous MACD histogram values, computed fresh from the
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

	ema200, err := EMA(candles, s.EMALen)
	if err != nil {
		return Signal{}, err
	}
	histNow, histPrev, err := macdHistSeries(candles, s.MACDFast, s.MACDSlow, s.MACDSignal)
	if err != nil {
		return Signal{}, err
	}
	_, sarTrends, err := ParabolicSARSeries(candles, s.SARStart, s.SARStep, s.SARMax)
	if err != nil {
		return Signal{}, err
	}
	last := len(candles) - 1
	sarUptrendNow, sarUptrendPrev := sarTrends[last], sarTrends[last-1]
	sarFlippedUp := sarUptrendNow && !sarUptrendPrev
	sarFlippedDown := !sarUptrendNow && sarUptrendPrev

	close := candles[last].Close
	histRising := histNow.GreaterThan(histPrev)
	histFalling := histNow.LessThan(histPrev)

	switch {
	case close.GreaterThan(ema200) && sarFlippedUp && histRising:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case close.LessThan(ema200) && sarFlippedDown && histFalling:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
