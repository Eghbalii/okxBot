package strategy

import (
	"time"

	"github.com/shopspring/decimal"
)

// ICTFairValueGapV2 trades the same 3-candle imbalance as V1, with the level arithmetic rebuilt.
//
// V1 was the clearest case of the 2026-09-12 target problem and the reason it was found: 48 trades,
// 37.5% win rate, -1.27 realized, and a mean realized reward:risk of 47:1. Its stop was the gap's
// own far edge, which on a 5m bar measured as little as 0.011% from entry — so tight that
// conductor.Clamps widened it ~45x to the MinSLDistPct floor, and MinTPSLRatio then carried that
// widening into the target. Production orders 2514/2518/2549 all show the signature: a stop 0.011%-
// 0.074% away paired with a target 4.5%-6.5% away. At 10x that target is 45-65% of margin, which
// price on a 5m candle does not reach, so those trades could only ever end at the stop. 37 of 48
// did.
//
// V2 keeps the gap as the STRUCTURE that defines the idea but sizes the stop as max(gap edge, ATR
// floor): the level still sits where the premise is invalidated, and it can no longer be so tight
// that a downstream clamp has to rescue it. The target is then a bounded multiple of that realized
// risk rather than a multiple of a number that was about to be rewritten.
type ICTFairValueGapV2 struct {
	LookbackBars int
	ATRPeriod    int
	MinStopATR   decimal.Decimal // stop floor in ATR units, so the gap edge can never be absurdly tight
	StopATR      decimal.Decimal
	RiskReward   decimal.Decimal

	hasGap    bool
	gapBull   bool
	gapLow    decimal.Decimal
	gapHigh   decimal.Decimal
	gapAt     time.Time
	handledAt time.Time
}

func NewICTFairValueGapV2() *ICTFairValueGapV2 {
	return &ICTFairValueGapV2{
		LookbackBars: 30,
		ATRPeriod:    14,
		MinStopATR:   decimal.NewFromFloat(0.8),
		StopATR:      decimal.NewFromFloat(1.2),
		RiskReward:   decimal.NewFromFloat(2),
	}
}

func (s *ICTFairValueGapV2) Name() string { return "ict_fvg_v2" }

func (s *ICTFairValueGapV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "lookback_bars", Default: decimal.NewFromInt(int64(s.LookbackBars)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "min_stop_atr", Default: s.MinStopATR, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromInt(3)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

// resetState clears evaluation state so a WithParams copy never inherits a warmed-up instance's
// gap tracking. Kept beside the fields it zeroes (CLAUDE.md §16.8) — grid_like was missed by a
// by-eye scan precisely because that discipline was not followed.
func (s *ICTFairValueGapV2) resetState() {
	s.hasGap, s.gapBull = false, false
	s.gapLow, s.gapHigh = decimal.Zero, decimal.Zero
	s.gapAt, s.handledAt = time.Time{}, time.Time{}
}

func (s *ICTFairValueGapV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["lookback_bars"]; ok {
		cp.LookbackBars = int(ClampParam(spec["lookback_bars"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["min_stop_atr"]; ok {
		cp.MinStopATR = ClampParam(spec["min_stop_atr"], v)
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

func (s *ICTFairValueGapV2) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < maxInt(3, s.ATRPeriod+1) {
		return Signal{Side: Hold}, nil
	}
	last := len(candles) - 1

	start := len(candles) - s.LookbackBars
	if start < 0 {
		start = 0
	}
	for i := last; i >= start+2; i-- {
		at := candles[i].Timestamp
		if !at.After(s.handledAt) {
			break
		}
		if s.hasGap && !at.After(s.gapAt) {
			break
		}
		c1, c3 := candles[i-2], candles[i]
		if c1.High.LessThan(c3.Low) {
			s.hasGap, s.gapBull, s.gapLow, s.gapHigh, s.gapAt = true, true, c1.High, c3.Low, at
			break
		}
		if c1.Low.GreaterThan(c3.High) {
			s.hasGap, s.gapBull, s.gapLow, s.gapHigh, s.gapAt = true, false, c3.High, c1.Low, at
			break
		}
	}
	if !s.hasGap {
		return Signal{Side: Hold}, nil
	}

	c := candles[last]
	// Only tradeable on a candle AFTER the one that formed the gap — the premise is that price
	// RETURNED to the zone, and testing the forming candle against its own zone is trivially true
	// (CLAUDE.md §30.1, where this entered at the worst price of the setup).
	if !c.Timestamp.After(s.gapAt) {
		return Signal{Side: Hold}, nil
	}

	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	if !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}

	// stopATRFor converts the structural distance into ATR units and floors it, which is the whole
	// correction over V1: the gap edge still decides WHERE the premise fails, but it can no longer
	// produce a stop so tight that the clamp rewrites the trade.
	stopATRFor := func(structural decimal.Decimal) decimal.Decimal {
		units := structural.Div(atr)
		if units.LessThan(s.MinStopATR) {
			return s.MinStopATR
		}
		if units.GreaterThan(s.StopATR) {
			return s.StopATR
		}
		return units
	}

	if s.gapBull {
		switch {
		case c.Low.LessThan(s.gapLow):
			s.hasGap, s.handledAt = false, s.gapAt
		case c.Low.LessThanOrEqual(s.gapHigh):
			s.hasGap, s.handledAt = false, s.gapAt
			risk := c.Close.Sub(s.gapLow)
			if risk.IsPositive() {
				return v2Signal(Buy, decimal.NewFromFloat(0.6), c.Close, atr, stopATRFor(risk), s.RiskReward), nil
			}
		}
		return Signal{Side: Hold}, nil
	}

	switch {
	case c.High.GreaterThan(s.gapHigh):
		s.hasGap, s.handledAt = false, s.gapAt
	case c.High.GreaterThanOrEqual(s.gapLow):
		s.hasGap, s.handledAt = false, s.gapAt
		risk := s.gapHigh.Sub(c.Close)
		if risk.IsPositive() {
			return v2Signal(Sell, decimal.NewFromFloat(0.6), c.Close, atr, stopATRFor(risk), s.RiskReward), nil
		}
	}
	return Signal{Side: Hold}, nil
}
