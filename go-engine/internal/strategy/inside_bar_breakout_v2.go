package strategy

import "github.com/shopspring/decimal"

// InsideBarBreakoutV2 trades the break of an inside bar — a consolidation candle contained entirely
// within its predecessor's range.
//
// V1: 29 trades, 48.3% win rate, +0.26 realized — one of the few V1 strategies that made money, so
// the entry logic is carried over deliberately intact. Its weakness was exits: a fixed percentage
// stop, and a 56:1 mean realized reward:risk from the usual clamp widening (CLAUDE.md §45).
//
// V2 keeps the pattern and the break test, sizes the levels in ATR units, and adds a filter the
// 48% win rate suggests is worth having: the inside bar must be a genuine contraction (materially
// narrower than its mother bar) rather than a candle that merely happens to fit inside a large one.
type InsideBarBreakoutV2 struct {
	ATRPeriod     int
	MaxInsideFrac decimal.Decimal // inside bar's range as a fraction of the mother bar's
	StopATR       decimal.Decimal
	RiskReward    decimal.Decimal
}

func NewInsideBarBreakoutV2() *InsideBarBreakoutV2 {
	return &InsideBarBreakoutV2{
		ATRPeriod:     14,
		MaxInsideFrac: decimal.NewFromFloat(0.7),
		StopATR:       decimal.NewFromFloat(1.1),
		RiskReward:    decimal.NewFromFloat(2),
	}
}

func (s *InsideBarBreakoutV2) Name() string { return "inside_bar_breakout_v2" }

func (s *InsideBarBreakoutV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "max_inside_frac", Default: s.MaxInsideFrac, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromInt(1)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *InsideBarBreakoutV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["max_inside_frac"]; ok {
		cp.MaxInsideFrac = ClampParam(spec["max_inside_frac"], v)
	}
	if v, ok := values["stop_atr"]; ok {
		cp.StopATR = ClampParam(spec["stop_atr"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

func (s *InsideBarBreakoutV2) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(3, s.ATRPeriod+1)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	// mother is the bar that contains inside; last is the candle attempting the break.
	mother, inside, last := candles[len(candles)-3], candles[len(candles)-2], candles[len(candles)-1]

	isInside := inside.High.LessThanOrEqual(mother.High) && inside.Low.GreaterThanOrEqual(mother.Low)
	if !isInside {
		return Signal{Side: Hold}, nil
	}
	motherRange := mother.High.Sub(mother.Low)
	insideRange := inside.High.Sub(inside.Low)
	if !motherRange.IsPositive() || insideRange.GreaterThan(motherRange.Mul(s.MaxInsideFrac)) {
		// Not a real contraction — the point of the pattern is coiling, not mere containment.
		return Signal{Side: Hold}, nil
	}

	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	if !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}

	switch {
	case last.Close.GreaterThan(inside.High):
		return v2Signal(Buy, decimal.NewFromFloat(0.6), last.Close, atr, s.StopATR, s.RiskReward), nil
	case last.Close.LessThan(inside.Low):
		return v2Signal(Sell, decimal.NewFromFloat(0.6), last.Close, atr, s.StopATR, s.RiskReward), nil
	default:
		return Signal{Side: Hold}, nil
	}
}
