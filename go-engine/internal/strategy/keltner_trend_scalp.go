package strategy

import "github.com/shopspring/decimal"

// KeltnerTrendScalp trades pullbacks to a Keltner Channel midline in the direction of a short EMA
// trend filter — a faster, tighter cousin of dual_ma_atr aimed at scalping continuation moves
// rather than catching the initial crossover. Buy when the trend EMA is rising and price pulls
// back to (or below) the Keltner midline from above; sell the mirrored case. Stop sits at the
// channel's own opposite band (a volatility-scaled structural level), target at RiskReward
// multiples of that risk.
type KeltnerTrendScalp struct {
	TrendEMALen int
	KCPeriod    int
	KCMult      decimal.Decimal
	RiskReward  decimal.Decimal

	prevTrendEMA decimal.Decimal
	hasPrev      bool
}

func NewKeltnerTrendScalp() *KeltnerTrendScalp {
	return &KeltnerTrendScalp{
		TrendEMALen: 34,
		KCPeriod:    20,
		KCMult:      decimal.NewFromFloat(1.5),
		RiskReward:  decimal.NewFromFloat(1.5),
	}
}

func (s *KeltnerTrendScalp) Name() string { return "keltner_trend_scalp" }

func (s *KeltnerTrendScalp) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "trend_ema_len", Default: decimal.NewFromInt(int64(s.TrendEMALen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "kc_period", Default: decimal.NewFromInt(int64(s.KCPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(200)},
		{Name: "kc_mult", Default: s.KCMult, Min: decimal.NewFromFloat(0.3), Max: decimal.NewFromInt(6)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *KeltnerTrendScalp) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["trend_ema_len"]; ok {
		cp.TrendEMALen = int(ClampParam(specByName["trend_ema_len"], v).IntPart())
	}
	if v, ok := values["kc_period"]; ok {
		cp.KCPeriod = int(ClampParam(specByName["kc_period"], v).IntPart())
	}
	if v, ok := values["kc_mult"]; ok {
		cp.KCMult = ClampParam(specByName["kc_mult"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *KeltnerTrendScalp) Evaluate(candles []Candle) (Signal, error) {
	need := s.TrendEMALen
	if s.KCPeriod+1 > need {
		need = s.KCPeriod + 1
	}
	if len(candles) < need {
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
	rising := trendEMA.GreaterThan(s.prevTrendEMA)
	falling := trendEMA.LessThan(s.prevTrendEMA)
	s.prevTrendEMA = trendEMA

	middle, upper, lower, err := KeltnerChannel(candles, s.KCPeriod, s.KCMult)
	if err != nil {
		return Signal{}, err
	}
	last := candles[len(candles)-1]

	switch {
	case rising && last.Close.GreaterThan(trendEMA) && last.Low.LessThanOrEqual(middle):
		risk := last.Close.Sub(lower)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       lower,
			TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	case falling && last.Close.LessThan(trendEMA) && last.High.GreaterThanOrEqual(middle):
		risk := upper.Sub(last.Close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       upper,
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
func (s *KeltnerTrendScalp) resetState() {
	s.prevTrendEMA, s.hasPrev = decimal.Zero, false
}
