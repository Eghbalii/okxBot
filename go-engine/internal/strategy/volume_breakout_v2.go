package strategy

import "github.com/shopspring/decimal"

// VolumeBreakoutV2 trades a range break confirmed by a volume surge.
//
// V1: 71 trades, 26.8% win rate, -4.48 realized — the third-worst PnL of the twelve. The volume
// filter was doing less than it appears: it compared the breakout candle's volume against a recent
// average, which on a 5m chart is satisfied by most breakout candles simply because a break is
// itself a volume event. The filter therefore removed almost nothing while the strategy took every
// break, including the many that fail immediately.
//
// V2 raises the surge requirement and adds a direction test: the breakout candle must close in the
// upper (or lower) part of its own range, so a high-volume candle that spiked through the level and
// closed back inside — a failed break on heavy volume, which is the opposite signal — does not
// qualify.
type VolumeBreakoutV2 struct {
	RangeLen   int
	VolLen     int
	ATRPeriod  int
	VolMult    decimal.Decimal
	BreakATR   decimal.Decimal
	StopATR    decimal.Decimal
	RiskReward decimal.Decimal
}

func NewVolumeBreakoutV2() *VolumeBreakoutV2 {
	return &VolumeBreakoutV2{
		RangeLen:   20,
		VolLen:     20,
		ATRPeriod:  14,
		VolMult:    decimal.NewFromFloat(1.8),
		BreakATR:   decimal.NewFromFloat(0.2),
		StopATR:    decimal.NewFromFloat(1.3),
		RiskReward: decimal.NewFromFloat(2),
	}
}

func (s *VolumeBreakoutV2) Name() string { return "volume_breakout_v2" }

func (s *VolumeBreakoutV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "range_len", Default: decimal.NewFromInt(int64(s.RangeLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "vol_len", Default: decimal.NewFromInt(int64(s.VolLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "vol_mult", Default: s.VolMult, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(10)},
		{Name: "break_atr", Default: s.BreakATR, Min: decimal.Zero, Max: decimal.NewFromInt(2)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *VolumeBreakoutV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["range_len"]; ok {
		cp.RangeLen = int(ClampParam(spec["range_len"], v).IntPart())
	}
	if v, ok := values["vol_len"]; ok {
		cp.VolLen = int(ClampParam(spec["vol_len"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["vol_mult"]; ok {
		cp.VolMult = ClampParam(spec["vol_mult"], v)
	}
	if v, ok := values["break_atr"]; ok {
		cp.BreakATR = ClampParam(spec["break_atr"], v)
	}
	if v, ok := values["stop_atr"]; ok {
		cp.StopATR = ClampParam(spec["stop_atr"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

func (s *VolumeBreakoutV2) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.RangeLen+1, s.VolLen+1, s.ATRPeriod+1)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	prior := candles[:len(candles)-1]
	rangeHigh, err := Highest(prior, s.RangeLen)
	if err != nil {
		return Signal{}, err
	}
	rangeLow, err := Lowest(prior, s.RangeLen)
	if err != nil {
		return Signal{}, err
	}
	// Average volume over the PRIOR candles, so the surge candle is not diluting the baseline it
	// is being judged against.
	avgVol, err := AvgVolume(prior, s.VolLen)
	if err != nil {
		return Signal{}, err
	}
	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	if !atr.IsPositive() || !avgVol.IsPositive() {
		return Signal{Side: Hold}, nil
	}

	last := candles[len(candles)-1]
	if last.Volume.LessThan(avgVol.Mul(s.VolMult)) {
		return Signal{Side: Hold}, nil
	}

	rng := last.High.Sub(last.Low)
	if !rng.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	closePos := last.Close.Sub(last.Low).Div(rng)
	strong := decimal.NewFromFloat(0.66)
	margin := atr.Mul(s.BreakATR)

	switch {
	case last.Close.GreaterThan(rangeHigh.Add(margin)) && closePos.GreaterThanOrEqual(strong):
		return v2Signal(Buy, decimal.NewFromFloat(0.65), last.Close, atr, s.StopATR, s.RiskReward), nil
	case last.Close.LessThan(rangeLow.Sub(margin)) && closePos.LessThanOrEqual(decimal.NewFromInt(1).Sub(strong)):
		return v2Signal(Sell, decimal.NewFromFloat(0.65), last.Close, atr, s.StopATR, s.RiskReward), nil
	default:
		return Signal{Side: Hold}, nil
	}
}
