package strategy

import "github.com/shopspring/decimal"

// HammersStars is a Go port of the widely-known TradingView "Hammers & Stars Strategy" family
// (raw PineScript source not extractable via automated fetch; implemented from the standard,
// well-documented hammer/shooting-star candlestick pattern definitions).
//
// Standard/default parameters: a small-body threshold (body <= 33% of the candle's total range,
// the common published definition of "small body" for these patterns) and a long-wick threshold
// (the opposite wick >= 2x the body, the standard hammer/shooting-star wick-to-body ratio), plus a
// short EMA(20) trend context filter — hammers are a bullish REVERSAL pattern (wants a preceding
// downtrend) and shooting stars a bearish reversal (wants a preceding uptrend), which the source
// strategies typically require.
//
// Signal logic: a hammer (small body near the top of its range, long lower wick, short/absent upper
// wick) after a downtrend signals a bullish reversal; a shooting star (small body near the bottom,
// long upper wick) after an uptrend signals bearish. Stop beyond the pattern candle's own extreme
// (a structural level), target at RiskReward multiples of that risk.
type HammersStars struct {
	TrendEMALen  int
	BodyMaxPct   decimal.Decimal // body as fraction of total range, e.g. 0.33
	WickMinRatio decimal.Decimal // long wick / body, e.g. 2
	RiskReward   decimal.Decimal

	prevTrendEMA decimal.Decimal
	hasPrev      bool
}

func NewHammersStars() *HammersStars {
	return &HammersStars{
		TrendEMALen:  20,
		BodyMaxPct:   decimal.NewFromFloat(0.33),
		WickMinRatio: decimal.NewFromInt(2),
		RiskReward:   decimal.NewFromFloat(1.5),
	}
}

func (s *HammersStars) Name() string { return "hammers_stars" }

func (s *HammersStars) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "trend_ema_len", Default: decimal.NewFromInt(int64(s.TrendEMALen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "body_max_pct", Default: s.BodyMaxPct, Min: decimal.NewFromFloat(0.05), Max: decimal.NewFromFloat(0.9)},
		{Name: "wick_min_ratio", Default: s.WickMinRatio, Min: decimal.NewFromFloat(1), Max: decimal.NewFromInt(10)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *HammersStars) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["trend_ema_len"]; ok {
		cp.TrendEMALen = int(ClampParam(spec["trend_ema_len"], v).IntPart())
	}
	if v, ok := values["body_max_pct"]; ok {
		cp.BodyMaxPct = ClampParam(spec["body_max_pct"], v)
	}
	if v, ok := values["wick_min_ratio"]; ok {
		cp.WickMinRatio = ClampParam(spec["wick_min_ratio"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *HammersStars) resetState() {
	s.prevTrendEMA, s.hasPrev = decimal.Zero, false
}

func (s *HammersStars) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.TrendEMALen+1 {
		return Signal{Side: Hold}, nil
	}
	trendEMA, err := EMA(candles, s.TrendEMALen)
	if err != nil {
		return Signal{}, err
	}
	if !s.hasPrev {
		s.prevTrendEMA, s.hasPrev = trendEMA, true
		return Signal{Side: Hold}, nil
	}
	priorDowntrend := trendEMA.LessThan(s.prevTrendEMA)
	priorUptrend := trendEMA.GreaterThan(s.prevTrendEMA)
	s.prevTrendEMA = trendEMA

	last := candles[len(candles)-1]
	rng := last.High.Sub(last.Low)
	if !rng.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	body := last.Close.Sub(last.Open).Abs()
	bodyTop := decimal.Max(last.Open, last.Close)
	bodyBottom := decimal.Min(last.Open, last.Close)
	upperWick := last.High.Sub(bodyTop)
	lowerWick := bodyBottom.Sub(last.Low)

	smallBody := body.Div(rng).LessThanOrEqual(s.BodyMaxPct)

	// Hammer: small body, long lower wick, short upper wick.
	isHammer := smallBody && lowerWick.IsPositive() &&
		(body.IsZero() || lowerWick.Div(body).GreaterThanOrEqual(s.WickMinRatio)) &&
		lowerWick.GreaterThan(upperWick)

	// Shooting star: small body, long upper wick, short lower wick.
	isStar := smallBody && upperWick.IsPositive() &&
		(body.IsZero() || upperWick.Div(body).GreaterThanOrEqual(s.WickMinRatio)) &&
		upperWick.GreaterThan(lowerWick)

	switch {
	case isHammer && priorDowntrend:
		risk := last.Close.Sub(last.Low)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       last.Low,
			TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	case isStar && priorUptrend:
		risk := last.High.Sub(last.Close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       last.High,
			TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
