package strategy

import "github.com/shopspring/decimal"

// HullSuite is a Go port of the widely-known TradingView "Hull Suite" by InSilico (raw PineScript
// source not extractable via automated fetch; implemented from Alan Hull's own published HMA
// construction, matching the well-known Hull Suite's "HMA mode" trend-ribbon behavior).
//
// Standard/default parameters: HMA length 55 — Hull Suite's own well-known default length (the same
// "longer-term trend" length pmax.go's doc references for Hull-based trend following, versus the
// shorter ~16-20 used for swing entries in hma_swing.go), plus a secondary smoothed HMA (a further
// HMA of length 4 applied over the primary HMA's own series, matching Hull Suite's published
// "length mult" smoothing idea) used purely for trend CONFIRMATION.
//
// Signal logic: buy when price crosses above the primary HMA AND the secondary smoothed HMA is also
// rising (confirms the ribbon's own trend direction, the ribbon-color-change concept Hull Suite is
// built around); sell on the mirrored cross below with the secondary HMA falling.
type HullSuite struct {
	HMALen       int
	SmoothLen    int
	SLPct, TPPct decimal.Decimal

	prevSmoothHMA decimal.Decimal
	hasPrevSmooth bool
	prevClose     decimal.Decimal
	prevHMA       decimal.Decimal
	hasPrevCross  bool
}

func NewHullSuite() *HullSuite {
	return &HullSuite{
		HMALen:    55,
		SmoothLen: 4,
		SLPct:     decimal.NewFromFloat(0.008),
		TPPct:     decimal.NewFromFloat(0.016),
	}
}

func (s *HullSuite) Name() string { return "hull_suite" }

func (s *HullSuite) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "hma_len", Default: decimal.NewFromInt(int64(s.HMALen)), Min: decimal.NewFromInt(4), Max: decimal.NewFromInt(400)},
		{Name: "smooth_len", Default: decimal.NewFromInt(int64(s.SmoothLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(50)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *HullSuite) resetState() {
	s.prevSmoothHMA, s.hasPrevSmooth = decimal.Zero, false
	s.prevClose, s.prevHMA, s.hasPrevCross = decimal.Zero, decimal.Zero, false
}

func (s *HullSuite) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["hma_len"]; ok {
		cp.HMALen = int(ClampParam(spec["hma_len"], v).IntPart())
	}
	if v, ok := values["smooth_len"]; ok {
		cp.SmoothLen = int(ClampParam(spec["smooth_len"], v).IntPart())
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

func (s *HullSuite) Evaluate(candles []Candle) (Signal, error) {
	sqrtLen := s.HMALen / 2
	if sqrtLen < 1 {
		sqrtLen = 1
	}
	need := s.HMALen + sqrtLen + s.SmoothLen + 2
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	hmaSeries, err := HMASeries(candles, s.HMALen)
	if err != nil {
		return Signal{}, err
	}
	// The HMA series is only valid (non-zero) from index HMALen-1+sqrtLen-1 onward; build a
	// contiguous valid tail to smooth over.
	firstValid := s.HMALen - 1
	for firstValid < len(hmaSeries) && hmaSeries[firstValid].IsZero() {
		firstValid++
	}
	if len(hmaSeries)-firstValid < s.SmoothLen+1 {
		return Signal{Side: Hold}, nil
	}
	validTail := make([]Candle, len(hmaSeries)-firstValid)
	for i, v := range hmaSeries[firstValid:] {
		validTail[i] = Candle{Close: v}
	}
	smoothSeries, err := WMASeries(validTail, s.SmoothLen)
	if err != nil {
		return Signal{}, err
	}

	hmaNow := hmaSeries[len(hmaSeries)-1]
	smoothNow := smoothSeries[len(smoothSeries)-1]
	close := candles[len(candles)-1].Close

	if !s.hasPrevCross {
		s.prevClose, s.prevHMA = close, hmaNow
		s.prevSmoothHMA, s.hasPrevSmooth = smoothNow, true
		s.hasPrevCross = true
		return Signal{Side: Hold}, nil
	}
	smoothRising := s.hasPrevSmooth && smoothNow.GreaterThan(s.prevSmoothHMA)
	smoothFalling := s.hasPrevSmooth && smoothNow.LessThan(s.prevSmoothHMA)

	crossedUp := s.prevClose.LessThanOrEqual(s.prevHMA) && close.GreaterThan(hmaNow)
	crossedDown := s.prevClose.GreaterThanOrEqual(s.prevHMA) && close.LessThan(hmaNow)

	s.prevClose, s.prevHMA, s.prevSmoothHMA = close, hmaNow, smoothNow

	switch {
	case crossedUp && smoothRising:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case crossedDown && smoothFalling:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
