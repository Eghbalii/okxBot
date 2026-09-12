package strategy

import "github.com/shopspring/decimal"

// BBSqueezeBreakoutV2 trades the same premise as V1 — volatility contracts, then expands — but
// judges the breakout differently.
//
// V1: 74 trades, 24.3% win rate, -0.45 realized, 10 TP against 54 SL. That win rate is the second
// worst of the twelve and the diagnosis is specific: V1 fired on ANY close outside the band after
// a squeeze, which on a 5m chart is most often the first probe rather than the move. A squeeze
// resolves in both directions before it picks one, so a close a hair outside the band is the
// single most common false signal this pattern produces.
//
// V2 requires the breakout candle to carry conviction: its close must clear the band by a
// meaningful fraction of ATR, and the candle must close in the top (or bottom) third of its own
// range, so a long rejection wick that happens to close outside does not qualify.
type BBSqueezeBreakoutV2 struct {
	BBPeriod     int
	SqueezeLen   int
	ATRPeriod    int
	BBMult       decimal.Decimal
	SqueezeRatio decimal.Decimal
	BreakATR     decimal.Decimal // how far beyond the band the close must sit, in ATR units
	StopATR      decimal.Decimal
	RiskReward   decimal.Decimal
}

func NewBBSqueezeBreakoutV2() *BBSqueezeBreakoutV2 {
	return &BBSqueezeBreakoutV2{
		BBPeriod:     20,
		SqueezeLen:   10,
		ATRPeriod:    14,
		BBMult:       decimal.NewFromFloat(2),
		SqueezeRatio: decimal.NewFromFloat(0.7),
		BreakATR:     decimal.NewFromFloat(0.15),
		StopATR:      decimal.NewFromFloat(1.3),
		RiskReward:   decimal.NewFromFloat(2),
	}
}

func (s *BBSqueezeBreakoutV2) Name() string { return "bb_squeeze_breakout_v2" }

func (s *BBSqueezeBreakoutV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "bb_period", Default: decimal.NewFromInt(int64(s.BBPeriod)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(100)},
		{Name: "squeeze_len", Default: decimal.NewFromInt(int64(s.SqueezeLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "bb_mult", Default: s.BBMult, Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromInt(5)},
		{Name: "squeeze_ratio", Default: s.SqueezeRatio, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromFloat(0.99)},
		{Name: "break_atr", Default: s.BreakATR, Min: decimal.Zero, Max: decimal.NewFromInt(2)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *BBSqueezeBreakoutV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["bb_period"]; ok {
		cp.BBPeriod = int(ClampParam(spec["bb_period"], v).IntPart())
	}
	if v, ok := values["squeeze_len"]; ok {
		cp.SqueezeLen = int(ClampParam(spec["squeeze_len"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["bb_mult"]; ok {
		cp.BBMult = ClampParam(spec["bb_mult"], v)
	}
	if v, ok := values["squeeze_ratio"]; ok {
		cp.SqueezeRatio = ClampParam(spec["squeeze_ratio"], v)
	}
	if v, ok := values["break_atr"]; ok {
		cp.BreakATR = ClampParam(spec["break_atr"], v)
	}
	if v, ok := values["stop_atr"]; ok {
		cp.StopATR = ClampParam(spec["stop_atr"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

func (s *BBSqueezeBreakoutV2) bandWidth(candles []Candle) (decimal.Decimal, error) {
	basis, upper, lower, err := BollingerBands(candles, s.BBPeriod, s.BBMult)
	if err != nil {
		return decimal.Zero, err
	}
	if !basis.IsPositive() {
		return decimal.Zero, nil
	}
	return upper.Sub(lower).Div(basis), nil
}

func (s *BBSqueezeBreakoutV2) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.BBPeriod+s.SqueezeLen, s.ATRPeriod+1)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	widthNow, err := s.bandWidth(candles)
	if err != nil {
		return Signal{}, err
	}
	widthPast, err := s.bandWidth(candles[:len(candles)-s.SqueezeLen])
	if err != nil {
		return Signal{}, err
	}
	if !widthPast.IsPositive() || widthNow.GreaterThan(widthPast.Mul(s.SqueezeRatio)) {
		return Signal{Side: Hold}, nil
	}

	_, upper, lower, err := BollingerBands(candles, s.BBPeriod, s.BBMult)
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
	margin := atr.Mul(s.BreakATR)
	rng := last.High.Sub(last.Low)
	if !rng.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	// Where in its own range did the candle close? A breakout that closes near its low is a
	// rejection, whatever the band says.
	closePos := last.Close.Sub(last.Low).Div(rng)
	third := decimal.NewFromFloat(0.66)

	switch {
	case last.Close.GreaterThan(upper.Add(margin)) && closePos.GreaterThanOrEqual(third):
		return v2Signal(Buy, decimal.NewFromFloat(0.65), last.Close, atr, s.StopATR, s.RiskReward), nil
	case last.Close.LessThan(lower.Sub(margin)) && closePos.LessThanOrEqual(decimal.NewFromInt(1).Sub(third)):
		return v2Signal(Sell, decimal.NewFromFloat(0.65), last.Close, atr, s.StopATR, s.RiskReward), nil
	default:
		return Signal{Side: Hold}, nil
	}
}
