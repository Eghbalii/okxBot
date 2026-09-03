package strategy

import "github.com/shopspring/decimal"

// MACDMomentum trades MACD histogram momentum shifts — a zero-line-cross of the MACD
// histogram (fast EMA minus slow EMA, minus its own signal-line EMA), one of the most widely used
// momentum scalp signals. A rising histogram crossing above zero signals accelerating bullish
// momentum; crossing below zero signals accelerating bearish momentum. Percentage SL/TP, since
// MACD has no structural price level of its own.
type MACDMomentum struct {
	FastLen   int
	SlowLen   int
	SignalLen int
	SLPct     decimal.Decimal
	TPPct     decimal.Decimal

	prevHist decimal.Decimal
	hasPrev  bool
}

func NewMACDMomentum() *MACDMomentum {
	return &MACDMomentum{
		FastLen:   12,
		SlowLen:   26,
		SignalLen: 9,
		SLPct:     decimal.NewFromFloat(0.007),
		TPPct:     decimal.NewFromFloat(0.012),
	}
}

func (s *MACDMomentum) Name() string { return "macd_momentum" }

func (s *MACDMomentum) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "fast_len", Default: decimal.NewFromInt(int64(s.FastLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "slow_len", Default: decimal.NewFromInt(int64(s.SlowLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "signal_len", Default: decimal.NewFromInt(int64(s.SignalLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *MACDMomentum) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["fast_len"]; ok {
		cp.FastLen = int(ClampParam(specByName["fast_len"], v).IntPart())
	}
	if v, ok := values["slow_len"]; ok {
		cp.SlowLen = int(ClampParam(specByName["slow_len"], v).IntPart())
	}
	if v, ok := values["signal_len"]; ok {
		cp.SignalLen = int(ClampParam(specByName["signal_len"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	cp.resetState()
	return &cp
}

// macdHistSeries returns the MACD histogram (macd line minus its own EMA-smoothed signal line)
// for every candle from the point both the slow EMA and the signal EMA are valid onward.
func (s *MACDMomentum) macdHistSeries(candles []Candle) ([]decimal.Decimal, error) {
	fastSeries, err := EMASeries(candles, s.FastLen)
	if err != nil {
		return nil, err
	}
	slowSeries, err := EMASeries(candles, s.SlowLen)
	if err != nil {
		return nil, err
	}
	macdLine := make([]Candle, len(candles))
	for i := s.SlowLen - 1; i < len(candles); i++ {
		macdLine[i] = Candle{Close: fastSeries[i].Sub(slowSeries[i])}
	}
	validMACD := macdLine[s.SlowLen-1:]
	signalSeries, err := EMASeries(validMACD, s.SignalLen)
	if err != nil {
		return nil, err
	}
	hist := make([]decimal.Decimal, len(candles))
	for i := s.SignalLen - 1; i < len(validMACD); i++ {
		hist[s.SlowLen-1+i] = validMACD[i].Close.Sub(signalSeries[i])
	}
	return hist, nil
}

func (s *MACDMomentum) Evaluate(candles []Candle) (Signal, error) {
	need := s.SlowLen + s.SignalLen
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	hist, err := s.macdHistSeries(candles)
	if err != nil {
		return Signal{}, err
	}
	histNow := hist[len(hist)-1]
	if !s.hasPrev {
		s.prevHist, s.hasPrev = histNow, true
		return Signal{Side: Hold}, nil
	}
	prevHist := s.prevHist
	s.prevHist = histNow

	crossedUp := prevHist.LessThanOrEqual(decimal.Zero) && histNow.GreaterThan(decimal.Zero)
	crossedDown := prevHist.GreaterThanOrEqual(decimal.Zero) && histNow.LessThan(decimal.Zero)

	switch {
	case crossedUp:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case crossedDown:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}

// resetState clears accumulated evaluation state, returning the strategy to how it behaves when
// freshly constructed. Called by WithParams, whose copy must not inherit it (see
// Strategy.WithParams for why).
//
// This lives beside the state fields on purpose: it is the one place that has to know what they
// are, so adding a field means updating the reset right here rather than remembering a zeroing
// line buried at the bottom of WithParams.
func (s *MACDMomentum) resetState() {
	s.prevHist, s.hasPrev = decimal.Zero, false
}
