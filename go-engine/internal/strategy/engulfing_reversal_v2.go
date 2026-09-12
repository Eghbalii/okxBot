package strategy

import "github.com/shopspring/decimal"

// EngulfingReversalV2 trades the classic engulfing candlestick pattern.
//
// V1: 13 trades, 38.5% win rate, -0.35 realized. Low sample, but the weakness is structural rather
// than statistical: a bare engulfing candle is one of the most common patterns on a 5m chart and
// most occurrences mean nothing. The pattern carries information when it appears at an extreme —
// after a run, where it marks exhaustion — not in the middle of a range.
//
// V2 requires the engulfing candle to appear after an extended move (price stretched from a slow
// EMA by an ATR multiple) and requires the engulfing body to be materially larger than the one it
// engulfs, so a marginal overlap does not qualify.
type EngulfingReversalV2 struct {
	TrendEMALen int
	ATRPeriod   int
	StretchATR  decimal.Decimal // how far from the EMA price must be for exhaustion to be plausible
	BodyRatio   decimal.Decimal // engulfing body must exceed this multiple of the prior body
	StopATR     decimal.Decimal
	RiskReward  decimal.Decimal
}

func NewEngulfingReversalV2() *EngulfingReversalV2 {
	return &EngulfingReversalV2{
		TrendEMALen: 50,
		ATRPeriod:   14,
		StretchATR:  decimal.NewFromFloat(1.2),
		BodyRatio:   decimal.NewFromFloat(1.3),
		StopATR:     decimal.NewFromFloat(1.1),
		RiskReward:  decimal.NewFromFloat(1.8),
	}
}

func (s *EngulfingReversalV2) Name() string { return "engulfing_reversal_v2" }

func (s *EngulfingReversalV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "trend_ema_len", Default: decimal.NewFromInt(int64(s.TrendEMALen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(200)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "stretch_atr", Default: s.StretchATR, Min: decimal.Zero, Max: decimal.NewFromInt(6)},
		{Name: "body_ratio", Default: s.BodyRatio, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(5)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *EngulfingReversalV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["trend_ema_len"]; ok {
		cp.TrendEMALen = int(ClampParam(spec["trend_ema_len"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["stretch_atr"]; ok {
		cp.StretchATR = ClampParam(spec["stretch_atr"], v)
	}
	if v, ok := values["body_ratio"]; ok {
		cp.BodyRatio = ClampParam(spec["body_ratio"], v)
	}
	if v, ok := values["stop_atr"]; ok {
		cp.StopATR = ClampParam(spec["stop_atr"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

func (s *EngulfingReversalV2) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.TrendEMALen, s.ATRPeriod+1, 2)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	trend, err := EMA(candles, s.TrendEMALen)
	if err != nil {
		return Signal{}, err
	}
	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	if !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}

	prev, last := candles[len(candles)-2], candles[len(candles)-1]
	prevBody := prev.Close.Sub(prev.Open).Abs()
	lastBody := last.Close.Sub(last.Open).Abs()
	if !prevBody.IsPositive() || lastBody.LessThan(prevBody.Mul(s.BodyRatio)) {
		return Signal{Side: Hold}, nil
	}

	stretch := atr.Mul(s.StretchATR)
	bullEngulf := prev.Close.LessThan(prev.Open) &&
		last.Close.GreaterThan(last.Open) &&
		last.Close.GreaterThan(prev.Open) &&
		last.Open.LessThan(prev.Close)
	bearEngulf := prev.Close.GreaterThan(prev.Open) &&
		last.Close.LessThan(last.Open) &&
		last.Close.LessThan(prev.Open) &&
		last.Open.GreaterThan(prev.Close)

	switch {
	// Bullish engulf only counts when price had been pushed well BELOW the mean — that is what
	// makes it exhaustion rather than an ordinary green candle.
	case bullEngulf && last.Close.LessThan(trend.Sub(stretch)):
		return v2Signal(Buy, decimal.NewFromFloat(0.6), last.Close, atr, s.StopATR, s.RiskReward), nil
	case bearEngulf && last.Close.GreaterThan(trend.Add(stretch)):
		return v2Signal(Sell, decimal.NewFromFloat(0.6), last.Close, atr, s.StopATR, s.RiskReward), nil
	default:
		return Signal{Side: Hold}, nil
	}
}
