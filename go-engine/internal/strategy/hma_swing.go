package strategy

import "github.com/shopspring/decimal"

// HMASwing is a Go port of the widely-known TradingView strategy family commonly titled "Hull
// Moving Average Swing Trader" (raw PineScript source not extractable via automated fetch;
// implemented from Alan Hull's own published HMA construction, CLAUDE.md conventions).
//
// Standard/default parameters: HMA period 20, a well-known "swing" configuration (Hull's own
// published default for a general-purpose HMA is commonly cited around 16-21 for shorter swing
// use versus 55 for longer-term trend following — 20 is used here as the widely-cited swing-trading
// default that keeps the indicator responsive on 5m).
//
// Signal logic: buy when the HMA turns up (slope changes from falling/flat to rising) after having
// been falling; sell when it turns down after having been rising — the classic "HMA color change"
// entry this strategy family is built around.
type HMASwing struct {
	Period       int
	SLPct, TPPct decimal.Decimal

	prevHMA      decimal.Decimal
	prevSlopeUp  bool
	prevSlopeSet bool
	hasPrev      bool
}

func NewHMASwing() *HMASwing {
	return &HMASwing{
		Period: 20,
		SLPct:  decimal.NewFromFloat(0.008),
		TPPct:  decimal.NewFromFloat(0.016),
	}
}

func (s *HMASwing) Name() string { return "hma_swing" }

func (s *HMASwing) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "period", Default: decimal.NewFromInt(int64(s.Period)), Min: decimal.NewFromInt(4), Max: decimal.NewFromInt(200)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *HMASwing) resetState() {
	s.prevHMA = decimal.Zero
	s.prevSlopeUp, s.prevSlopeSet, s.hasPrev = false, false, false
}

func (s *HMASwing) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["period"]; ok {
		cp.Period = int(ClampParam(spec["period"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	cp.resetState()
	return &cp
}

func (s *HMASwing) Evaluate(candles []Candle) (Signal, error) {
	sqrtLen := s.Period / 2
	if sqrtLen < 1 {
		sqrtLen = 1
	}
	if len(candles) < s.Period+sqrtLen {
		return Signal{Side: Hold}, nil
	}
	hma, err := HMA(candles, s.Period)
	if err != nil {
		return Signal{Side: Hold}, nil
	}

	if !s.hasPrev {
		s.prevHMA, s.hasPrev = hma, true
		return Signal{Side: Hold}, nil
	}
	slopeUp := hma.GreaterThan(s.prevHMA)
	slopeDown := hma.LessThan(s.prevHMA)
	prevSlopeUp, prevSlopeSet := s.prevSlopeUp, s.prevSlopeSet
	s.prevHMA = hma
	if slopeUp || slopeDown {
		s.prevSlopeUp, s.prevSlopeSet = slopeUp, true
	}

	turnedUp := prevSlopeSet && !prevSlopeUp && slopeUp
	turnedDown := prevSlopeSet && prevSlopeUp && slopeDown

	switch {
	case turnedUp:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case turnedDown:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
