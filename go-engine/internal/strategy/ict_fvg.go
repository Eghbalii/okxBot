package strategy

import "github.com/shopspring/decimal"

// ICTFairValueGap trades the "Fair Value Gap" (FVG) concept from ICT/Smart-Money price action: a
// 3-candle imbalance where candle 1's high sits below candle 3's low (a bullish FVG — the market
// left a gap because it moved too fast for two-sided trading to occur) or candle 1's low sits
// above candle 3's high (bearish FVG). Price frequently returns to "fill" part of that gap before
// continuing in the original direction — this strategy watches for that: an FVG forms, and price
// later trades back into the gap's zone, which is treated as an entry in the FVG's original
// direction (continuation, not reversal — the gap acted as support/resistance).
//
// The gap's own boundary is the structural stop (a return past the far edge invalidates the
// premise that it was ever real support/resistance), and the target is placed at RiskReward
// multiples of that risk — well suited to 5m scalping since FVGs form and get revisited within a
// handful of candles on a fast timeframe.
type ICTFairValueGap struct {
	LookbackBars int // how many recent candles to scan for an unfilled FVG
	RiskReward   decimal.Decimal

	// bullLow/bullHigh describe an active bullish gap's zone [c1.High, c3.Low]; bearLow/bearHigh
	// an active bearish gap's zone [c3.High, c1.Low]. Cleared once traded or invalidated.
	hasBullGap  bool
	bullGapLow  decimal.Decimal
	bullGapHigh decimal.Decimal
	hasBearGap  bool
	bearGapLow  decimal.Decimal
	bearGapHigh decimal.Decimal
}

func NewICTFairValueGap() *ICTFairValueGap {
	return &ICTFairValueGap{
		LookbackBars: 30,
		RiskReward:   decimal.NewFromFloat(2),
	}
}

func (s *ICTFairValueGap) Name() string { return "ict_fvg" }

func (s *ICTFairValueGap) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "lookback_bars", Default: decimal.NewFromInt(int64(s.LookbackBars)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *ICTFairValueGap) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["lookback_bars"]; ok {
		cp.LookbackBars = int(ClampParam(specByName["lookback_bars"], v).IntPart())
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *ICTFairValueGap) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < 3 {
		return Signal{Side: Hold}, nil
	}

	// Scan the trailing LookbackBars window for the most recent unfilled 3-candle imbalance,
	// re-detecting fresh each call rather than trusting only what was armed previously — a new,
	// more recent gap should take priority over a stale one still technically unfilled.
	start := len(candles) - s.LookbackBars
	if start < 0 {
		start = 0
	}
	for i := len(candles) - 1; i >= start+2; i-- {
		c1, c3 := candles[i-2], candles[i]
		if c1.High.LessThan(c3.Low) {
			s.hasBullGap, s.bullGapLow, s.bullGapHigh = true, c1.High, c3.Low
			break
		}
		if c1.Low.GreaterThan(c3.High) {
			s.hasBearGap, s.bearGapLow, s.bearGapHigh = true, c3.High, c1.Low
			break
		}
	}

	last := candles[len(candles)-1]

	if s.hasBullGap {
		// Invalidate if price has since traded fully through the gap's far (lower) edge — the
		// premise that it's real support is broken.
		if last.Low.LessThan(s.bullGapLow) {
			s.hasBullGap = false
		} else if last.Low.LessThanOrEqual(s.bullGapHigh) {
			// Price returned into the gap zone — take the continuation entry.
			s.hasBullGap = false
			risk := last.Close.Sub(s.bullGapLow)
			if risk.IsPositive() {
				return Signal{
					Side:       Buy,
					Confidence: decimal.NewFromFloat(0.6),
					EntryPx:    last.Close,
					SLPx:       s.bullGapLow,
					TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
					SLPct:      risk.Div(last.Close),
					TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
				}, nil
			}
		}
	}

	if s.hasBearGap {
		if last.High.GreaterThan(s.bearGapHigh) {
			s.hasBearGap = false
		} else if last.High.GreaterThanOrEqual(s.bearGapLow) {
			s.hasBearGap = false
			risk := s.bearGapHigh.Sub(last.Close)
			if risk.IsPositive() {
				return Signal{
					Side:       Sell,
					Confidence: decimal.NewFromFloat(0.6),
					EntryPx:    last.Close,
					SLPx:       s.bearGapHigh,
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
func (s *ICTFairValueGap) resetState() {
	s.hasBullGap, s.hasBearGap = false, false
	s.bullGapLow, s.bullGapHigh = decimal.Zero, decimal.Zero
	s.bearGapLow, s.bearGapHigh = decimal.Zero, decimal.Zero
}
