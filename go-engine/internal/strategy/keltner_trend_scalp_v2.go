package strategy

import "github.com/shopspring/decimal"

// KeltnerTrendScalpV2 trades a pullback to the Keltner midline in the direction of a trend.
//
// V1: 48 trades, 37.5% win rate, -2.15 realized, with a 42:1 mean realized reward:risk — again the
// clamp-widening signature (CLAUDE.md §45), since its stop was the channel's far band, which sits
// very close to entry when volatility has just contracted.
//
// V2 keeps the pullback entry and the trend requirement but sizes the stop in ATR units, and
// confirms the pullback actually resumed: the candle must close back on the trend side of the
// midline rather than merely having touched it, which is what separates a pullback from the start
// of a reversal.
type KeltnerTrendScalpV2 struct {
	TrendEMALen int
	KCPeriod    int
	ATRPeriod   int
	KCMult      decimal.Decimal
	StopATR     decimal.Decimal
	RiskReward  decimal.Decimal

	prevTrendEMA decimal.Decimal
	hasPrev      bool
}

func NewKeltnerTrendScalpV2() *KeltnerTrendScalpV2 {
	return &KeltnerTrendScalpV2{
		TrendEMALen: 50,
		KCPeriod:    20,
		ATRPeriod:   14,
		KCMult:      decimal.NewFromFloat(1.5),
		StopATR:     decimal.NewFromFloat(1.2),
		RiskReward:  decimal.NewFromFloat(1.8),
	}
}

func (s *KeltnerTrendScalpV2) Name() string { return "keltner_trend_scalp_v2" }

func (s *KeltnerTrendScalpV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "trend_ema_len", Default: decimal.NewFromInt(int64(s.TrendEMALen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(200)},
		{Name: "kc_period", Default: decimal.NewFromInt(int64(s.KCPeriod)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(100)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "kc_mult", Default: s.KCMult, Min: decimal.NewFromFloat(0.3), Max: decimal.NewFromInt(5)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *KeltnerTrendScalpV2) resetState() {
	s.prevTrendEMA, s.hasPrev = decimal.Zero, false
}

func (s *KeltnerTrendScalpV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["trend_ema_len"]; ok {
		cp.TrendEMALen = int(ClampParam(spec["trend_ema_len"], v).IntPart())
	}
	if v, ok := values["kc_period"]; ok {
		cp.KCPeriod = int(ClampParam(spec["kc_period"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["kc_mult"]; ok {
		cp.KCMult = ClampParam(spec["kc_mult"], v)
	}
	if v, ok := values["stop_atr"]; ok {
		cp.StopATR = ClampParam(spec["stop_atr"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *KeltnerTrendScalpV2) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.TrendEMALen, s.KCPeriod+1, s.ATRPeriod+1)
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

	middle, _, _, err := KeltnerChannel(candles, s.KCPeriod, s.KCMult)
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

	last := candles[len(candles)-1]

	switch {
	// Touched the midline from above and closed back above it, in a rising trend: the pullback
	// resumed. V1 required only the touch, which also matches a candle on its way through.
	case rising && last.Low.LessThanOrEqual(middle) && last.Close.GreaterThan(middle) && last.Close.GreaterThan(trendEMA):
		return v2Signal(Buy, decimal.NewFromFloat(0.6), last.Close, atr, s.StopATR, s.RiskReward), nil
	case falling && last.High.GreaterThanOrEqual(middle) && last.Close.LessThan(middle) && last.Close.LessThan(trendEMA):
		return v2Signal(Sell, decimal.NewFromFloat(0.6), last.Close, atr, s.StopATR, s.RiskReward), nil
	default:
		return Signal{Side: Hold}, nil
	}
}
