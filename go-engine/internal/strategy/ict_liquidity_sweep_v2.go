package strategy

import "github.com/shopspring/decimal"

// ICTLiquiditySweepV2 trades the stop-hunt reversal: a wick beyond a recent swing extreme that
// closes back inside the prior range, which reads as the breakout having been a liquidity grab
// rather than genuine continuation.
//
// V1: 35 trades, 34.3% win rate, -0.12 realized. The entry idea is sound and resolves within one
// candle, which suits 5m; what it lacked was any measure of whether the sweep was significant. A
// one-tick poke beyond a swing high counted the same as a decisive flush, and the former is
// indistinguishable from ordinary noise.
//
// V2 requires the wick to extend beyond the swing by a fraction of ATR, and requires the candle to
// close back inside by a similar margin — a sweep that barely closes back inside has not actually
// rejected anything yet.
type ICTLiquiditySweepV2 struct {
	SwingLen   int
	ATRPeriod  int
	SweepATR   decimal.Decimal // how far past the swing the wick must reach
	StopATR    decimal.Decimal
	RiskReward decimal.Decimal
}

func NewICTLiquiditySweepV2() *ICTLiquiditySweepV2 {
	return &ICTLiquiditySweepV2{
		SwingLen:   20,
		ATRPeriod:  14,
		SweepATR:   decimal.NewFromFloat(0.25),
		StopATR:    decimal.NewFromFloat(1.2),
		RiskReward: decimal.NewFromFloat(1.8),
	}
}

func (s *ICTLiquiditySweepV2) Name() string { return "ict_liquidity_sweep_v2" }

func (s *ICTLiquiditySweepV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "swing_len", Default: decimal.NewFromInt(int64(s.SwingLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "sweep_atr", Default: s.SweepATR, Min: decimal.Zero, Max: decimal.NewFromInt(3)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *ICTLiquiditySweepV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["swing_len"]; ok {
		cp.SwingLen = int(ClampParam(spec["swing_len"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["sweep_atr"]; ok {
		cp.SweepATR = ClampParam(spec["sweep_atr"], v)
	}
	if v, ok := values["stop_atr"]; ok {
		cp.StopATR = ClampParam(spec["stop_atr"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

func (s *ICTLiquiditySweepV2) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.SwingLen+1, s.ATRPeriod+1)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	// The swing is measured on the candles BEFORE the current one, so the sweep candle's own wick
	// is not part of the level it is supposed to be sweeping.
	prior := candles[:len(candles)-1]
	swingHigh, err := Highest(prior, s.SwingLen)
	if err != nil {
		return Signal{}, err
	}
	swingLow, err := Lowest(prior, s.SwingLen)
	if err != nil {
		return Signal{}, err
	}
	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	if !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}

	last := candles[len(candles)-1]
	margin := atr.Mul(s.SweepATR)

	switch {
	case last.High.GreaterThan(swingHigh.Add(margin)) && last.Close.LessThan(swingHigh):
		// Swept the highs and closed back below them — the breakout was a grab, fade it.
		return v2Signal(Sell, decimal.NewFromFloat(0.65), last.Close, atr, s.StopATR, s.RiskReward), nil
	case last.Low.LessThan(swingLow.Sub(margin)) && last.Close.GreaterThan(swingLow):
		return v2Signal(Buy, decimal.NewFromFloat(0.65), last.Close, atr, s.StopATR, s.RiskReward), nil
	default:
		return Signal{Side: Hold}, nil
	}
}
