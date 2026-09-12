package strategy

import "github.com/shopspring/decimal"

// RangeBreakoutV2 trades a Donchian-style break of a rolling N-candle high/low.
//
// V1: 40 trades, 37.5% win rate, -0.62 realized, and a mean realized reward:risk of 69:1 — the
// third-worst ratio distortion, for the familiar reason (CLAUDE.md §45): its stop was the opposite
// side of the broken range, which in a tight range is very close to entry, so the clamp widened it
// and inflated the target with it. It also took every breakout, including the many that immediately
// fail back into the range on a 5m chart.
//
// V2 sizes the stop in ATR units instead of at the range's far side (a range that is 8 ATR wide
// does not justify an 8 ATR stop on a scalp), and requires the break to clear the level by a
// margin rather than by a tick.
type RangeBreakoutV2 struct {
	RangeLen   int
	ATRPeriod  int
	BreakATR   decimal.Decimal
	StopATR    decimal.Decimal
	RiskReward decimal.Decimal
}

func NewRangeBreakoutV2() *RangeBreakoutV2 {
	return &RangeBreakoutV2{
		RangeLen:   20,
		ATRPeriod:  14,
		BreakATR:   decimal.NewFromFloat(0.2),
		StopATR:    decimal.NewFromFloat(1.3),
		RiskReward: decimal.NewFromFloat(2),
	}
}

func (s *RangeBreakoutV2) Name() string { return "range_breakout_v2" }

func (s *RangeBreakoutV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "range_len", Default: decimal.NewFromInt(int64(s.RangeLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "break_atr", Default: s.BreakATR, Min: decimal.Zero, Max: decimal.NewFromInt(2)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *RangeBreakoutV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["range_len"]; ok {
		cp.RangeLen = int(ClampParam(spec["range_len"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
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

func (s *RangeBreakoutV2) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.RangeLen+1, s.ATRPeriod+1)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	prior := candles[:len(candles)-1]
	rangeHigh, err := Highest(prior, s.RangeLen)
	if err != nil {
		return Signal{}, err
	}
	rangeLow, err := Lowest(prior, s.RangeLen)
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

	switch {
	case last.Close.GreaterThan(rangeHigh.Add(margin)):
		return v2Signal(Buy, decimal.NewFromFloat(0.6), last.Close, atr, s.StopATR, s.RiskReward), nil
	case last.Close.LessThan(rangeLow.Sub(margin)):
		return v2Signal(Sell, decimal.NewFromFloat(0.6), last.Close, atr, s.StopATR, s.RiskReward), nil
	default:
		return Signal{Side: Hold}, nil
	}
}
