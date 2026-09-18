package strategy

import "github.com/shopspring/decimal"

// ADXDMIQuality is a Go port of the widely-known TradingView indicator family commonly titled
// "Gravity Trend ADX Strength Meter" / "Quality Scored DMI System" (raw PineScript source not
// extractable via automated fetch; implemented from Welles Wilder's own standard, well-documented
// ADX/DMI construction).
//
// Standard/default parameters, Wilder's own published defaults:
//   - DMI/ADX period 14.
//   - ADX trend-strength threshold 25 (Wilder's own commonly cited "trending" cutoff, at the
//     conservative end of the well-known 20-25 range).
//
// Signal logic: buy when +DI crosses above -DI AND ADX is above the threshold (a trending market,
// not just a directional flicker in a chop); sell on the mirrored -DI crossing above +DI. ADX's own
// magnitude is reported as the signal's Confidence (scaled to 0-1), which is the "quality score"
// concept the name describes — a stronger trend at the moment of the cross is a higher-confidence
// signal.
type ADXDMIQuality struct {
	Period       int
	ADXThreshold decimal.Decimal
	SLPct, TPPct decimal.Decimal

	prevPlusDI, prevMinusDI decimal.Decimal
	hasPrev                 bool
}

func NewADXDMIQuality() *ADXDMIQuality {
	return &ADXDMIQuality{
		Period:       14,
		ADXThreshold: decimal.NewFromInt(25),
		SLPct:        decimal.NewFromFloat(0.01),
		TPPct:        decimal.NewFromFloat(0.02),
	}
}

func (s *ADXDMIQuality) Name() string { return "adx_dmi_quality" }

func (s *ADXDMIQuality) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "period", Default: decimal.NewFromInt(int64(s.Period)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "adx_threshold", Default: s.ADXThreshold, Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(60)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *ADXDMIQuality) resetState() {
	s.prevPlusDI, s.prevMinusDI, s.hasPrev = decimal.Zero, decimal.Zero, false
}

func (s *ADXDMIQuality) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["period"]; ok {
		cp.Period = int(ClampParam(spec["period"], v).IntPart())
	}
	if v, ok := values["adx_threshold"]; ok {
		cp.ADXThreshold = ClampParam(spec["adx_threshold"], v)
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	cp.resetState()
	return &cp
}

func (s *ADXDMIQuality) Evaluate(candles []Candle) (Signal, error) {
	need := 2*s.Period + 2
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	plusDI, minusDI, err := DirectionalIndex(candles, s.Period)
	if err != nil {
		return Signal{}, err
	}
	adx, err := ADX(candles, s.Period)
	if err != nil {
		return Signal{}, err
	}

	if !s.hasPrev {
		s.prevPlusDI, s.prevMinusDI, s.hasPrev = plusDI, minusDI, true
		return Signal{Side: Hold}, nil
	}
	crossedUp := s.prevPlusDI.LessThanOrEqual(s.prevMinusDI) && plusDI.GreaterThan(minusDI)
	crossedDown := s.prevPlusDI.GreaterThanOrEqual(s.prevMinusDI) && plusDI.LessThan(minusDI)
	s.prevPlusDI, s.prevMinusDI = plusDI, minusDI

	trending := adx.GreaterThan(s.ADXThreshold)
	// Quality score: ADX magnitude scaled to [0,1], capped at 100 (ADX's own theoretical ceiling).
	quality := adx.Div(hundred)
	if quality.GreaterThan(decimal.NewFromInt(1)) {
		quality = decimal.NewFromInt(1)
	}

	switch {
	case crossedUp && trending:
		return Signal{Side: Buy, Confidence: quality, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case crossedDown && trending:
		return Signal{Side: Sell, Confidence: quality, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
