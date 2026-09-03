package strategy

import (
	"time"

	"github.com/shopspring/decimal"
)

// ICTOrderBlock trades the ICT/Smart-Money "order block" concept: the last opposite-direction
// candle immediately before a strong, decisive move (a candle whose range clears a volatility
// threshold) marks the zone where large orders are presumed to have been placed. Price often
// returns to that candle's range once before continuing the original move ("mitigating" the order
// block) — this strategy arms the most recent qualifying order block and enters when price trades
// back into its range, in the direction of the impulse that followed it (continuation).
//
// The order block's own range is the structural stop (a full close through it invalidates the
// block), target at RiskReward multiples of that risk.
type ICTOrderBlock struct {
	ImpulseATRMult decimal.Decimal // impulse candle's range must be >= this many ATRs
	ATRPeriod      int
	LookbackBars   int
	RiskReward     decimal.Decimal

	// The active block, identified by the TIMESTAMP of the impulse candle that produced it — see
	// ICTFairValueGap for why identity is required (without it the rescan re-arms an already-traded
	// block, firing one setup repeatedly) and why it is a timestamp rather than a slice index (the
	// caller's candle window slides once it reaches CandleWindow, so indices are not stable).
	hasBlock  bool
	blockBull bool
	blockLow  decimal.Decimal
	blockHigh decimal.Decimal
	blockAt   time.Time

	// handledAt is the blockAt of the last block traded or invalidated, so the rescan never
	// re-arms it. Zero means nothing has been handled yet.
	handledAt time.Time
}

func NewICTOrderBlock() *ICTOrderBlock {
	return &ICTOrderBlock{
		ImpulseATRMult: decimal.NewFromFloat(1.5),
		ATRPeriod:      14,
		LookbackBars:   30,
		RiskReward:     decimal.NewFromFloat(2),
	}
}

func (s *ICTOrderBlock) Name() string { return "ict_order_block" }

func (s *ICTOrderBlock) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "impulse_atr_mult", Default: s.ImpulseATRMult, Min: decimal.NewFromFloat(0.3), Max: decimal.NewFromInt(6)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "lookback_bars", Default: decimal.NewFromInt(int64(s.LookbackBars)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *ICTOrderBlock) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["impulse_atr_mult"]; ok {
		cp.ImpulseATRMult = ClampParam(specByName["impulse_atr_mult"], v)
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(specByName["atr_period"], v).IntPart())
	}
	if v, ok := values["lookback_bars"]; ok {
		cp.LookbackBars = int(ClampParam(specByName["lookback_bars"], v).IntPart())
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *ICTOrderBlock) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.ATRPeriod+2 {
		return Signal{Side: Hold}, nil
	}
	last := len(candles) - 1

	// One ATR for the whole scan, computed on the current window. The previous version called ATR
	// inside the loop against a re-sliced window per iteration, making evaluation O(lookback *
	// window) — ~28x the cost of a comparable strategy on a 300-candle window — to gain a
	// per-bar ATR that barely differs across the few dozen bars being scanned.
	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	if !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	minImpulse := atr.Mul(s.ImpulseATRMult)

	start := len(candles) - s.LookbackBars
	if start < 1 {
		start = 1 // need candles[i-1] for the block itself
	}
	for i := last; i >= start; i-- {
		at := candles[i].Timestamp
		if !at.After(s.handledAt) {
			break // this and everything older was already traded or invalidated
		}
		if s.hasBlock && !at.After(s.blockAt) {
			break // already tracking this block or an older one; leave its state alone
		}
		impulse := candles[i]
		if impulse.High.Sub(impulse.Low).LessThan(minImpulse) {
			continue
		}
		ob := candles[i-1]
		switch {
		case impulse.Close.GreaterThan(impulse.Open) && ob.Close.LessThan(ob.Open):
			s.hasBlock, s.blockBull, s.blockLow, s.blockHigh, s.blockAt = true, true, ob.Low, ob.High, at
		case impulse.Close.LessThan(impulse.Open) && ob.Close.GreaterThan(ob.Open):
			s.hasBlock, s.blockBull, s.blockLow, s.blockHigh, s.blockAt = true, false, ob.Low, ob.High, at
		default:
			continue
		}
		break
	}

	if !s.hasBlock {
		return Signal{Side: Hold}, nil
	}

	c := candles[last]

	// Only tradeable on a candle after the impulse that formed the block — the impulse candle
	// itself necessarily overlaps the block's range (it opened where the block closed), so testing
	// it would enter at the impulse's own extreme rather than on the return this strategy trades.
	if !c.Timestamp.After(s.blockAt) {
		return Signal{Side: Hold}, nil
	}

	if s.blockBull {
		switch {
		case c.Close.LessThan(s.blockLow):
			s.hasBlock, s.handledAt = false, s.blockAt
		case c.Low.LessThanOrEqual(s.blockHigh):
			s.hasBlock, s.handledAt = false, s.blockAt
			risk := c.Close.Sub(s.blockLow)
			if risk.IsPositive() {
				return Signal{
					Side:       Buy,
					Confidence: decimal.NewFromFloat(0.6),
					EntryPx:    c.Close,
					SLPx:       s.blockLow,
					TPPx:       c.Close.Add(risk.Mul(s.RiskReward)),
					SLPct:      risk.Div(c.Close),
					TPPct:      risk.Div(c.Close).Mul(s.RiskReward),
				}, nil
			}
		}
		return Signal{Side: Hold}, nil
	}

	switch {
	case c.Close.GreaterThan(s.blockHigh):
		s.hasBlock, s.handledAt = false, s.blockAt
	case c.High.GreaterThanOrEqual(s.blockLow):
		s.hasBlock, s.handledAt = false, s.blockAt
		risk := s.blockHigh.Sub(c.Close)
		if risk.IsPositive() {
			return Signal{
				Side:       Sell,
				Confidence: decimal.NewFromFloat(0.6),
				EntryPx:    c.Close,
				SLPx:       s.blockHigh,
				TPPx:       c.Close.Sub(risk.Mul(s.RiskReward)),
				SLPct:      risk.Div(c.Close),
				TPPct:      risk.Div(c.Close).Mul(s.RiskReward),
			}, nil
		}
	}
	return Signal{Side: Hold}, nil
}

// resetState clears accumulated evaluation state, returning the strategy to how it behaves when
// freshly constructed. Called by WithParams, whose copy must not inherit it (see
// Strategy.WithParams for why).
//
// This lives beside the state fields on purpose: it is the one place that has to know what they
// are, so adding a field means updating the reset right here rather than remembering a zeroing
// line buried at the bottom of WithParams.
func (s *ICTOrderBlock) resetState() {
	s.hasBlock, s.blockBull = false, false
	s.blockLow, s.blockHigh = decimal.Zero, decimal.Zero
	s.blockAt, s.handledAt = time.Time{}, time.Time{}
}
