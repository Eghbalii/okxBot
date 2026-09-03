package strategy

import "github.com/shopspring/decimal"

// VolumeBreakout trades a range breakout that's confirmed by a volume surge — a standard scalp
// filter added on top of plain price breakouts, since a breakout on average/low volume is far more
// likely to be a fakeout than one backed by a genuine surge in participation. Buy a close above
// the recent high with volume well above its own recent average; sell the mirrored case. Stop at
// the opposite side of the range just broken (a structural level), target at RiskReward multiples.
type VolumeBreakout struct {
	RangeLen     int
	VolAvgLen    int
	VolSurgeMult decimal.Decimal // current candle's volume must be >= this multiple of its average
	RiskReward   decimal.Decimal
}

func NewVolumeBreakout() *VolumeBreakout {
	return &VolumeBreakout{
		RangeLen:     12,
		VolAvgLen:    20,
		VolSurgeMult: decimal.NewFromFloat(1.8),
		RiskReward:   decimal.NewFromFloat(1.5),
	}
}

func (s *VolumeBreakout) Name() string { return "volume_breakout" }

func (s *VolumeBreakout) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "range_len", Default: decimal.NewFromInt(int64(s.RangeLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "vol_avg_len", Default: decimal.NewFromInt(int64(s.VolAvgLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "vol_surge_mult", Default: s.VolSurgeMult, Min: decimal.NewFromFloat(1), Max: decimal.NewFromInt(10)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *VolumeBreakout) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["range_len"]; ok {
		cp.RangeLen = int(ClampParam(specByName["range_len"], v).IntPart())
	}
	if v, ok := values["vol_avg_len"]; ok {
		cp.VolAvgLen = int(ClampParam(specByName["vol_avg_len"], v).IntPart())
	}
	if v, ok := values["vol_surge_mult"]; ok {
		cp.VolSurgeMult = ClampParam(specByName["vol_surge_mult"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	return &cp
}

func (s *VolumeBreakout) Evaluate(candles []Candle) (Signal, error) {
	need := s.RangeLen
	if s.VolAvgLen+1 > need {
		need = s.VolAvgLen + 1
	}
	if len(candles) < need+1 {
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
	avgVol, err := AvgVolume(prior, s.VolAvgLen)
	if err != nil {
		return Signal{}, err
	}
	last := candles[len(candles)-1]
	if !avgVol.IsPositive() || last.Volume.LessThan(avgVol.Mul(s.VolSurgeMult)) {
		return Signal{Side: Hold}, nil
	}

	switch {
	case last.Close.GreaterThan(rangeHigh):
		risk := last.Close.Sub(rangeLow)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    last.Close,
			SLPx:       rangeLow,
			TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	case last.Close.LessThan(rangeLow):
		risk := rangeHigh.Sub(last.Close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    last.Close,
			SLPx:       rangeHigh,
			TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
