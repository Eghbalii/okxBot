package strategy

import "github.com/shopspring/decimal"

// InsideBarBreakout trades the classic price-action "inside bar" setup: a candle whose entire
// range sits inside the prior candle's range (the "mother bar") signals a pause/consolidation;
// trade the breakout of the inside bar's own high/low. A well-known, mechanical price-action
// pattern that resolves within one or two candles, fitting 5m scalping well. Stop at the inside
// bar's opposite extreme (a structural level), target at RiskReward multiples of that risk.
type InsideBarBreakout struct {
	RiskReward decimal.Decimal

	hasInside  bool
	insideHigh decimal.Decimal
	insideLow  decimal.Decimal
}

func NewInsideBarBreakout() *InsideBarBreakout {
	return &InsideBarBreakout{
		RiskReward: decimal.NewFromFloat(1.5),
	}
}

func (s *InsideBarBreakout) Name() string { return "inside_bar_breakout" }

func (s *InsideBarBreakout) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *InsideBarBreakout) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *InsideBarBreakout) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < 2 {
		return Signal{Side: Hold}, nil
	}
	last := candles[len(candles)-1]
	var sig Signal

	if s.hasInside {
		switch {
		case last.Close.GreaterThan(s.insideHigh):
			low := s.insideLow
			risk := last.Close.Sub(low)
			if risk.IsPositive() {
				sig = Signal{
					Side:       Buy,
					Confidence: decimal.NewFromFloat(0.55),
					EntryPx:    last.Close,
					SLPx:       low,
					TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
					SLPct:      risk.Div(last.Close),
					TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
				}
			}
			s.hasInside = false
		case last.Close.LessThan(s.insideLow):
			high := s.insideHigh
			risk := high.Sub(last.Close)
			if risk.IsPositive() {
				sig = Signal{
					Side:       Sell,
					Confidence: decimal.NewFromFloat(0.55),
					EntryPx:    last.Close,
					SLPx:       high,
					TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
					SLPct:      risk.Div(last.Close),
					TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
				}
			}
			s.hasInside = false
		}
	}

	// Re-check for a fresh inside bar every call, independent of whether a breakout just fired —
	// the just-closed candle may itself be a new mother bar's inside bar.
	s.armIfInside(candles)
	return sig, nil
}

// armIfInside checks whether the just-closed candle is an inside bar relative to the one before
// it, arming the breakout levels if so.
func (s *InsideBarBreakout) armIfInside(candles []Candle) {
	if len(candles) < 2 {
		return
	}
	mother, inside := candles[len(candles)-2], candles[len(candles)-1]
	if inside.High.LessThanOrEqual(mother.High) && inside.Low.GreaterThanOrEqual(mother.Low) {
		s.hasInside, s.insideHigh, s.insideLow = true, inside.High, inside.Low
	} else {
		s.hasInside = false
	}
}

// resetState clears accumulated evaluation state, returning the strategy to how it behaves when
// freshly constructed. Called by WithParams, whose copy must not inherit it (see
// Strategy.WithParams for why).
//
// This lives beside the state fields on purpose: it is the one place that has to know what they
// are, so adding a field means updating the reset right here rather than remembering a zeroing
// line buried at the bottom of WithParams.
func (s *InsideBarBreakout) resetState() {
	s.hasInside = false
	s.insideHigh, s.insideLow = decimal.Zero, decimal.Zero
}
