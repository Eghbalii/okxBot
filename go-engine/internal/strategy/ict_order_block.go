package strategy

import "github.com/shopspring/decimal"

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

	hasBull  bool
	bullLow  decimal.Decimal
	bullHigh decimal.Decimal
	hasBear  bool
	bearLow  decimal.Decimal
	bearHigh decimal.Decimal
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
	need := s.ATRPeriod + 2
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	start := len(candles) - s.LookbackBars
	if start < s.ATRPeriod+1 {
		start = s.ATRPeriod + 1
	}

	// Scan for the most recent impulse candle and the opposite candle immediately before it.
	for i := len(candles) - 1; i >= start; i-- {
		atr, err := ATR(candles[:i+1], s.ATRPeriod)
		if err != nil {
			continue
		}
		if !atr.IsPositive() {
			continue
		}
		impulse := candles[i]
		impulseRange := impulse.High.Sub(impulse.Low)
		if impulseRange.LessThan(atr.Mul(s.ImpulseATRMult)) {
			continue
		}
		ob := candles[i-1]
		bullish := impulse.Close.GreaterThan(impulse.Open)
		bearish := impulse.Close.LessThan(impulse.Open)

		if bullish && ob.Close.LessThan(ob.Open) {
			s.hasBull, s.bullLow, s.bullHigh = true, ob.Low, ob.High
			break
		}
		if bearish && ob.Close.GreaterThan(ob.Open) {
			s.hasBear, s.bearLow, s.bearHigh = true, ob.Low, ob.High
			break
		}
	}

	last := candles[len(candles)-1]

	if s.hasBull {
		if last.Close.LessThan(s.bullLow) {
			s.hasBull = false
		} else if last.Low.LessThanOrEqual(s.bullHigh) {
			s.hasBull = false
			risk := last.Close.Sub(s.bullLow)
			if risk.IsPositive() {
				return Signal{
					Side:       Buy,
					Confidence: decimal.NewFromFloat(0.6),
					EntryPx:    last.Close,
					SLPx:       s.bullLow,
					TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
					SLPct:      risk.Div(last.Close),
					TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
				}, nil
			}
		}
	}

	if s.hasBear {
		if last.Close.GreaterThan(s.bearHigh) {
			s.hasBear = false
		} else if last.High.GreaterThanOrEqual(s.bearLow) {
			s.hasBear = false
			risk := s.bearHigh.Sub(last.Close)
			if risk.IsPositive() {
				return Signal{
					Side:       Sell,
					Confidence: decimal.NewFromFloat(0.6),
					EntryPx:    last.Close,
					SLPx:       s.bearHigh,
					TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
					SLPct:      risk.Div(last.Close),
					TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
				}, nil
			}
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
	s.hasBull, s.hasBear = false, false
	s.bullLow, s.bullHigh = decimal.Zero, decimal.Zero
	s.bearLow, s.bearHigh = decimal.Zero, decimal.Zero
}
