package strategy

import "github.com/shopspring/decimal"

// EMARibbonPullback is a well-known scalp/day-trading system: three EMAs (fast/mid/slow) stacked
// in trend order define the ribbon's direction, and an entry triggers when price pulls back to
// touch the middle EMA and closes back in the trend direction — buying dips in an uptrend rather
// than chasing highs, and mirrored for downtrends. Percentage SL/TP since the ribbon itself is
// not a price structure the way a swing point is.
type EMARibbonPullback struct {
	FastLen int
	MidLen  int
	SlowLen int
	SLPct   decimal.Decimal
	TPPct   decimal.Decimal
}

func NewEMARibbonPullback() *EMARibbonPullback {
	return &EMARibbonPullback{
		FastLen: 8,
		MidLen:  21,
		SlowLen: 55,
		SLPct:   decimal.NewFromFloat(0.006),
		TPPct:   decimal.NewFromFloat(0.01),
	}
}

func (s *EMARibbonPullback) Name() string { return "ema_ribbon_pullback" }

func (s *EMARibbonPullback) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "fast_len", Default: decimal.NewFromInt(int64(s.FastLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "mid_len", Default: decimal.NewFromInt(int64(s.MidLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "slow_len", Default: decimal.NewFromInt(int64(s.SlowLen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(400)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *EMARibbonPullback) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["fast_len"]; ok {
		cp.FastLen = int(ClampParam(specByName["fast_len"], v).IntPart())
	}
	if v, ok := values["mid_len"]; ok {
		cp.MidLen = int(ClampParam(specByName["mid_len"], v).IntPart())
	}
	if v, ok := values["slow_len"]; ok {
		cp.SlowLen = int(ClampParam(specByName["slow_len"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	return &cp
}

func (s *EMARibbonPullback) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.SlowLen {
		return Signal{Side: Hold}, nil
	}
	fast, err := EMA(candles, s.FastLen)
	if err != nil {
		return Signal{}, err
	}
	mid, err := EMA(candles, s.MidLen)
	if err != nil {
		return Signal{}, err
	}
	slow, err := EMA(candles, s.SlowLen)
	if err != nil {
		return Signal{}, err
	}
	last := candles[len(candles)-1]

	uptrend := fast.GreaterThan(mid) && mid.GreaterThan(slow)
	downtrend := fast.LessThan(mid) && mid.LessThan(slow)

	switch {
	case uptrend && last.Low.LessThanOrEqual(mid) && last.Close.GreaterThan(mid):
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case downtrend && last.High.GreaterThanOrEqual(mid) && last.Close.LessThan(mid):
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
