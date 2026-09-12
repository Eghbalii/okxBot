package strategy

import (
	"time"

	"github.com/shopspring/decimal"
)

// ICTOrderBlockV2 trades the same concept as V1 — the last opposite-direction candle before an
// impulse marks a zone price often returns to — with V1's level arithmetic replaced.
//
// V1: only 7 trades but the worst ratio distortion measured anywhere, a mean realized reward:risk
// of 80:1. Same mechanism as ict_fvg (CLAUDE.md §45): the block's own range was the stop, which on
// a quiet 5m bar is nearly zero, so the clamp widened it and carried that widening into a target
// price could not reach.
//
// V2 floors the structural stop in ATR units and bounds the target off the realized risk.
type ICTOrderBlockV2 struct {
	LookbackBars int
	ATRPeriod    int
	ImpulseATR   decimal.Decimal // how large the move after the block must be, in ATR units
	MinStopATR   decimal.Decimal
	StopATR      decimal.Decimal
	RiskReward   decimal.Decimal

	hasBlock  bool
	blockBull bool
	blockLow  decimal.Decimal
	blockHigh decimal.Decimal
	blockAt   time.Time
	handledAt time.Time
}

func NewICTOrderBlockV2() *ICTOrderBlockV2 {
	return &ICTOrderBlockV2{
		LookbackBars: 40,
		ATRPeriod:    14,
		ImpulseATR:   decimal.NewFromFloat(1.2),
		MinStopATR:   decimal.NewFromFloat(0.8),
		StopATR:      decimal.NewFromFloat(1.4),
		RiskReward:   decimal.NewFromFloat(2),
	}
}

func (s *ICTOrderBlockV2) Name() string { return "ict_order_block_v2" }

func (s *ICTOrderBlockV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "lookback_bars", Default: decimal.NewFromInt(int64(s.LookbackBars)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "impulse_atr", Default: s.ImpulseATR, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(6)},
		{Name: "min_stop_atr", Default: s.MinStopATR, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromInt(3)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *ICTOrderBlockV2) resetState() {
	s.hasBlock, s.blockBull = false, false
	s.blockLow, s.blockHigh = decimal.Zero, decimal.Zero
	s.blockAt, s.handledAt = time.Time{}, time.Time{}
}

func (s *ICTOrderBlockV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["lookback_bars"]; ok {
		cp.LookbackBars = int(ClampParam(spec["lookback_bars"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["impulse_atr"]; ok {
		cp.ImpulseATR = ClampParam(spec["impulse_atr"], v)
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

func (s *ICTOrderBlockV2) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < maxInt(3, s.ATRPeriod+1) {
		return Signal{Side: Hold}, nil
	}
	last := len(candles) - 1

	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	if !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	impulse := atr.Mul(s.ImpulseATR)

	start := len(candles) - s.LookbackBars
	if start < 1 {
		start = 1
	}
	// One ATR computed for the whole scan rather than recomputed inside it — V1 did the latter,
	// costing 634us per evaluation against 40us (CLAUDE.md §30.1).
	for i := last; i >= start+1; i-- {
		at := candles[i].Timestamp
		if !at.After(s.handledAt) {
			break
		}
		if s.hasBlock && !at.After(s.blockAt) {
			break
		}
		block, move := candles[i-1], candles[i]
		bullBlock := block.Close.LessThan(block.Open) && move.Close.Sub(move.Open).GreaterThan(impulse)
		bearBlock := block.Close.GreaterThan(block.Open) && move.Open.Sub(move.Close).GreaterThan(impulse)
		if bullBlock || bearBlock {
			s.hasBlock, s.blockBull = true, bullBlock
			s.blockLow, s.blockHigh, s.blockAt = block.Low, block.High, at
			break
		}
	}
	if !s.hasBlock {
		return Signal{Side: Hold}, nil
	}

	c := candles[last]
	// The impulse candle necessarily overlaps its own block's range, so a return must be tested on
	// a strictly later candle (same correction as ict_fvg, CLAUDE.md §30.1).
	if !c.Timestamp.After(s.blockAt) {
		return Signal{Side: Hold}, nil
	}

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

	if s.blockBull {
		switch {
		case c.Low.LessThan(s.blockLow):
			s.hasBlock, s.handledAt = false, s.blockAt
		case c.Low.LessThanOrEqual(s.blockHigh):
			s.hasBlock, s.handledAt = false, s.blockAt
			if risk := c.Close.Sub(s.blockLow); risk.IsPositive() {
				return v2Signal(Buy, decimal.NewFromFloat(0.6), c.Close, atr, stopATRFor(risk), s.RiskReward), nil
			}
		}
		return Signal{Side: Hold}, nil
	}

	switch {
	case c.High.GreaterThan(s.blockHigh):
		s.hasBlock, s.handledAt = false, s.blockAt
	case c.High.GreaterThanOrEqual(s.blockLow):
		s.hasBlock, s.handledAt = false, s.blockAt
		if risk := s.blockHigh.Sub(c.Close); risk.IsPositive() {
			return v2Signal(Sell, decimal.NewFromFloat(0.6), c.Close, atr, stopATRFor(risk), s.RiskReward), nil
		}
	}
	return Signal{Side: Hold}, nil
}
