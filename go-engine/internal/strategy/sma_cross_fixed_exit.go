package strategy

import "github.com/shopspring/decimal"

// SMACrossFixedExit is a Go port of the Pine Script "Stop loss and Take Profit in $$ example" by
// adolgov (pinescript/strategy_Stop loss and Take Profit in $$ example.pine, MPL 2.0). Entry is a
// plain fast/slow SMA crossover (author's own comment calls it a "random entry condition" — it's
// really just a demo of the $$-based exit math). The original computes SL/TP as a fixed dollar
// amount converted to price ticks via position size; that's meaningless without knowing position
// notional up front, so here SL/TP are expressed directly as a % of entry price instead — the
// domain has SLPct/TPPct for exactly this and it's the more portable representation anyway.
type SMACrossFixedExit struct {
	FastPeriod int
	SlowPeriod int
	SLPct      decimal.Decimal
	TPPct      decimal.Decimal
}

func NewSMACrossFixedExit() *SMACrossFixedExit {
	return &SMACrossFixedExit{
		FastPeriod: 14,
		SlowPeriod: 28,
		SLPct:      decimal.NewFromFloat(0.01), // $100 in the original example
		TPPct:      decimal.NewFromFloat(0.02), // $200 in the original example
	}
}

func (s *SMACrossFixedExit) Name() string { return "sma_cross_fixed_exit" }

func (s *SMACrossFixedExit) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "fast_period", Default: decimal.NewFromInt(int64(s.FastPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "slow_period", Default: decimal.NewFromInt(int64(s.SlowPeriod)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *SMACrossFixedExit) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["fast_period"]; ok {
		cp.FastPeriod = int(ClampParam(specByName["fast_period"], v).IntPart())
	}
	if v, ok := values["slow_period"]; ok {
		cp.SlowPeriod = int(ClampParam(specByName["slow_period"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	return &cp
}

func (s *SMACrossFixedExit) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.SlowPeriod+1 {
		return Signal{Side: Hold}, nil
	}
	fastNow, err := SMA(candles, s.FastPeriod)
	if err != nil {
		return Signal{}, err
	}
	slowNow, err := SMA(candles, s.SlowPeriod)
	if err != nil {
		return Signal{}, err
	}
	fastPrev, err := SMA(candles[:len(candles)-1], s.FastPeriod)
	if err != nil {
		return Signal{}, err
	}
	slowPrev, err := SMA(candles[:len(candles)-1], s.SlowPeriod)
	if err != nil {
		return Signal{}, err
	}

	crossedUp := fastPrev.LessThanOrEqual(slowPrev) && fastNow.GreaterThan(slowNow)
	crossedDown := fastPrev.GreaterThanOrEqual(slowPrev) && fastNow.LessThan(slowNow)

	switch {
	case crossedUp:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case crossedDown:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
