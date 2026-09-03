package strategy

import "github.com/shopspring/decimal"

// RangeBreakout is a Donchian-style rolling-range breakout: buy a close above the highest high of
// the last N candles (excluding the current one), sell a close below the lowest low — the same
// family as classic opening-range-breakout scalps, generalized to a rolling window so it isn't
// tied to a specific session open. Stop sits at the opposite side of the range that was just
// broken (a structural level), target projected at RiskReward multiples of that risk.
type RangeBreakout struct {
	RangeLen   int
	RiskReward decimal.Decimal
}

func NewRangeBreakout() *RangeBreakout {
	return &RangeBreakout{
		RangeLen:   12, // 1 hour of 5m candles
		RiskReward: decimal.NewFromFloat(1.5),
	}
}

func (s *RangeBreakout) Name() string { return "range_breakout" }

func (s *RangeBreakout) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "range_len", Default: decimal.NewFromInt(int64(s.RangeLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *RangeBreakout) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["range_len"]; ok {
		cp.RangeLen = int(ClampParam(specByName["range_len"], v).IntPart())
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	return &cp
}

func (s *RangeBreakout) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.RangeLen+1 {
		return Signal{Side: Hold}, nil
	}
	// Exclude the current (just-closed) candle from the range so it's judged against the range
	// that preceded it, not one that already includes its own extreme.
	prior := candles[:len(candles)-1]
	rangeHigh, err := Highest(prior, s.RangeLen)
	if err != nil {
		return Signal{}, err
	}
	rangeLow, err := Lowest(prior, s.RangeLen)
	if err != nil {
		return Signal{}, err
	}

	last := candles[len(candles)-1]

	switch {
	case last.Close.GreaterThan(rangeHigh):
		risk := last.Close.Sub(rangeLow)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       rangeLow,
			TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	case last.Close.LessThan(rangeLow):
		risk := rangeHigh.Sub(last.Close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       rangeHigh,
			TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
