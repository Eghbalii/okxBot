package strategy

import "github.com/shopspring/decimal"

// MicuRobertEMACross is a Go port of the widely-known TradingView strategy "STRATEGY RS MicuRobert
// EMA cross V2" (raw PineScript source not extractable via automated fetch; implemented from the
// standard dual-EMA-crossover-with-trend-filter algorithm the title describes).
//
// Standard/default parameters used: a classic dual-EMA cross pair, fast 12 / slow 26 (the same
// well-known pairing MACD's own two EMAs use), plus a longer EMA(100) trend filter — a common
// "only trade crosses in the direction of the higher-timeframe trend" addition well-documented
// across dual-EMA-cross strategy variants.
//
// Signal logic: buy when the fast EMA crosses above the slow EMA AND price is above the trend EMA
// (uptrend context); sell on the mirrored cross below, with price below the trend EMA.
type MicuRobertEMACross struct {
	FastLen, SlowLen, TrendLen int
	SLPct, TPPct               decimal.Decimal
}

func NewMicuRobertEMACross() *MicuRobertEMACross {
	return &MicuRobertEMACross{
		FastLen:  12,
		SlowLen:  26,
		TrendLen: 100,
		SLPct:    decimal.NewFromFloat(0.008),
		TPPct:    decimal.NewFromFloat(0.016),
	}
}

func (s *MicuRobertEMACross) Name() string { return "micurobert_ema_cross" }

func (s *MicuRobertEMACross) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "fast_len", Default: decimal.NewFromInt(int64(s.FastLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "slow_len", Default: decimal.NewFromInt(int64(s.SlowLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "trend_len", Default: decimal.NewFromInt(int64(s.TrendLen)), Min: decimal.NewFromInt(10), Max: decimal.NewFromInt(400)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *MicuRobertEMACross) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["fast_len"]; ok {
		cp.FastLen = int(ClampParam(spec["fast_len"], v).IntPart())
	}
	if v, ok := values["slow_len"]; ok {
		cp.SlowLen = int(ClampParam(spec["slow_len"], v).IntPart())
	}
	if v, ok := values["trend_len"]; ok {
		cp.TrendLen = int(ClampParam(spec["trend_len"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	return &cp
}

func (s *MicuRobertEMACross) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.SlowLen, s.TrendLen) + 2
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	fastSeries, err := EMASeries(candles, s.FastLen)
	if err != nil {
		return Signal{}, err
	}
	slowSeries, err := EMASeries(candles, s.SlowLen)
	if err != nil {
		return Signal{}, err
	}
	trendEMA, err := EMA(candles, s.TrendLen)
	if err != nil {
		return Signal{}, err
	}
	last := len(candles) - 1
	fastNow, fastPrev := fastSeries[last], fastSeries[last-1]
	slowNow, slowPrev := slowSeries[last], slowSeries[last-1]
	close := candles[last].Close

	crossedUp := fastPrev.LessThanOrEqual(slowPrev) && fastNow.GreaterThan(slowNow)
	crossedDown := fastPrev.GreaterThanOrEqual(slowPrev) && fastNow.LessThan(slowNow)

	switch {
	case crossedUp && close.GreaterThan(trendEMA):
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case crossedDown && close.LessThan(trendEMA):
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
