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
	// No resetState() call: this strategy is stateless. Both histogram values it compares are
	// derived from the candles passed to Evaluate, so a copy has nothing to inherit.
	return &cp
}

// macdHist returns the MACD histogram (the MACD line minus its own EMA-smoothed signal line) for
// the last two candles: the current bar and the one before it, which is exactly what a zero-line
// cross needs.
//
// Both values come from the same computed series rather than one being carried in struct state
// between calls. That matters for correctness as well as cost: a state-carried "previous" is only
// the previous CANDLE's value if Evaluate is called exactly once per closed candle, and the engine
// makes no such guarantee (a restart reseeds the window, and evaluating the same window twice
// would compare a value against itself). Reading both from the series makes the cross depend only
// on the candles passed in.
func (s *MACDMomentum) macdHist(candles []Candle) (now, prev decimal.Decimal, err error) {
	fastSeries, err := EMASeries(candles, s.FastLen)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	slowSeries, err := EMASeries(candles, s.SlowLen)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	// The MACD line is only defined once the slow EMA is, so the signal EMA is taken over that
	// valid tail. EMASeries wants Candles, and only Close is read.
	validMACD := make([]Candle, 0, len(candles)-(s.SlowLen-1))
	for i := s.SlowLen - 1; i < len(candles); i++ {
		validMACD = append(validMACD, Candle{Close: fastSeries[i].Sub(slowSeries[i])})
	}
	signalSeries, err := EMASeries(validMACD, s.SignalLen)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	n := len(validMACD)
	now = validMACD[n-1].Close.Sub(signalSeries[n-1])
	prev = validMACD[n-2].Close.Sub(signalSeries[n-2])
	return now, prev, nil
}

func (s *MACDMomentum) Evaluate(candles []Candle) (Signal, error) {
	// One extra candle beyond the signal EMA's own warm-up, so the previous bar's histogram is
	// defined too and a cross can actually be detected.
	need := s.SlowLen + s.SignalLen
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	histNow, prevHist, err := s.macdHist(candles)
	if err != nil {
		return Signal{}, err
	}

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
