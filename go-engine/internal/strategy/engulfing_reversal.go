package strategy

import "github.com/shopspring/decimal"

// EngulfingReversal trades the classic candlestick price-action pattern: a candle whose real body
// fully engulfs the prior candle's real body, in the opposite direction of the preceding
// micro-trend (a short EMA slope filter), signals a reversal. One of the most widely used
// discretionary price-action setups, and well suited to 5m since the pattern resolves in exactly
// two candles. Stop beyond the engulfing candle's own extreme (a structural level), target at
// RiskReward multiples of that risk.
type EngulfingReversal struct {
	TrendEMALen int
	RiskReward  decimal.Decimal

	prevTrendEMA decimal.Decimal
	hasPrev      bool
}

func NewEngulfingReversal() *EngulfingReversal {
	return &EngulfingReversal{
		TrendEMALen: 20,
		RiskReward:  decimal.NewFromFloat(1.5),
	}
}

func (s *EngulfingReversal) Name() string { return "engulfing_reversal" }

func (s *EngulfingReversal) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "trend_ema_len", Default: decimal.NewFromInt(int64(s.TrendEMALen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *EngulfingReversal) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["trend_ema_len"]; ok {
		cp.TrendEMALen = int(ClampParam(specByName["trend_ema_len"], v).IntPart())
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *EngulfingReversal) Evaluate(candles []Candle) (Signal, error) {
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
	falling := trendEMA.LessThan(s.prevTrendEMA)
	rising := trendEMA.GreaterThan(s.prevTrendEMA)
	s.prevTrendEMA = trendEMA

	prev, last := candles[len(candles)-2], candles[len(candles)-1]
	prevBody := prev.Close.Sub(prev.Open).Abs()
	if !prevBody.IsPositive() {
		return Signal{Side: Hold}, nil
	}

	bullishEngulf := prev.Close.LessThan(prev.Open) && // prior candle was bearish
		last.Close.GreaterThan(last.Open) && // this candle is bullish
		last.Open.LessThanOrEqual(prev.Close) &&
		last.Close.GreaterThanOrEqual(prev.Open)

	bearishEngulf := prev.Close.GreaterThan(prev.Open) && // prior candle was bullish
		last.Close.LessThan(last.Open) && // this candle is bearish
		last.Open.GreaterThanOrEqual(prev.Close) &&
		last.Close.LessThanOrEqual(prev.Open)

	switch {
	case bullishEngulf && falling:
		risk := last.Close.Sub(last.Low)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    last.Close,
			SLPx:       last.Low,
			TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	case bearishEngulf && rising:
		risk := last.High.Sub(last.Close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.6),
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

// resetState clears accumulated evaluation state, returning the strategy to how it behaves when
// freshly constructed. Called by WithParams, whose copy must not inherit it (see
// Strategy.WithParams for why).
//
// This lives beside the state fields on purpose: it is the one place that has to know what they
// are, so adding a field means updating the reset right here rather than remembering a zeroing
// line buried at the bottom of WithParams.
func (s *EngulfingReversal) resetState() {
	s.prevTrendEMA, s.hasPrev = decimal.Zero, false
}
