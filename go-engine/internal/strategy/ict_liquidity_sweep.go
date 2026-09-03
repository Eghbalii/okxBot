package strategy

import "github.com/shopspring/decimal"

// ICTLiquiditySweep trades the ICT "stop hunt" / liquidity-sweep concept: price briefly wicks
// beyond a recent swing high/low (where retail stop-losses and breakout entries are presumed to
// cluster — "liquidity") and then closes back inside the prior range on the very same candle,
// signaling the breakout was a sweep rather than genuine continuation. Enters in the reversal
// direction, stop beyond the sweep's own wick (a structural level — a genuine break would trade
// back through it), target at RiskReward multiples of that risk. A well-known, fast-resolving
// price-action reversal pattern that fits 5m scalping well since the whole pattern completes
// within one candle once the swing level is known.
type ICTLiquiditySweep struct {
	SwingLookback int // candles (excluding current) to find the swing high/low being swept
	RiskReward    decimal.Decimal
}

func NewICTLiquiditySweep() *ICTLiquiditySweep {
	return &ICTLiquiditySweep{
		SwingLookback: 20,
		RiskReward:    decimal.NewFromFloat(2),
	}
}

func (s *ICTLiquiditySweep) Name() string { return "ict_liquidity_sweep" }

func (s *ICTLiquiditySweep) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "swing_lookback", Default: decimal.NewFromInt(int64(s.SwingLookback)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *ICTLiquiditySweep) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["swing_lookback"]; ok {
		cp.SwingLookback = int(ClampParam(specByName["swing_lookback"], v).IntPart())
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	return &cp
}

func (s *ICTLiquiditySweep) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.SwingLookback+1 {
		return Signal{Side: Hold}, nil
	}
	prior := candles[:len(candles)-1]
	swingHigh, err := Highest(prior, s.SwingLookback)
	if err != nil {
		return Signal{}, err
	}
	swingLow, err := Lowest(prior, s.SwingLookback)
	if err != nil {
		return Signal{}, err
	}
	last := candles[len(candles)-1]

	sweptHigh := last.High.GreaterThan(swingHigh) && last.Close.LessThan(swingHigh)
	sweptLow := last.Low.LessThan(swingLow) && last.Close.GreaterThan(swingLow)

	switch {
	case sweptLow:
		risk := last.Close.Sub(last.Low)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.65),
			EntryPx:    last.Close,
			SLPx:       last.Low,
			TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	case sweptHigh:
		risk := last.High.Sub(last.Close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.65),
			EntryPx:    last.Close,
			SLPx:       last.High,
			TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
