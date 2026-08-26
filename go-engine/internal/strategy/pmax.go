package strategy

import "github.com/shopspring/decimal"

// PMax is a Go port of the core signal logic from "PMax Explorer" by KivancOzbilgic
// (pinescript/strategy_PMax Explorer STRATEGY & SCREENER.pine, MPL 2.0). The original's
// multi-symbol screener (20 hardcoded tickers scanned via security() for a cross-market label) is
// pure display/reporting and isn't ported — only the PMax indicator and its crossover signal.
// PMax is an ATR-based trailing stop around a moving average (the same "Chandelier"-style
// construction as PMaxExplorer's own name): MAvg trending up ratchets a rising floor
// (MAvg - Multiplier*ATR) that price must stay above; crossing below flips the trend down, and
// vice versa for the ceiling. The original supports 8 MA types (SMA/EMA/WMA/TMA/VAR/WWMA/ZLEMA/
// TSF); only SMA/EMA/WMA are ported here — VAR/WWMA/ZLEMA/TSF are unusual recursive/regression-
// based variants without an existing Go implementation in this package, and porting them
// mechanically without validation risked silently wrong indicator math, so MAType is constrained
// to the three that map onto tested indicator functions.
type PMax struct {
	ATRPeriod  int
	Multiplier decimal.Decimal
	MAType     string // "SMA", "EMA", or "WMA"
	MALength   int
	SLPct      decimal.Decimal
	TPPct      decimal.Decimal

	prevTrend int // +1 long, -1 short, 0 = not yet established
}

func NewPMax() *PMax {
	return &PMax{
		ATRPeriod:  10,
		Multiplier: decimal.NewFromFloat(3.0),
		MAType:     "EMA",
		MALength:   10,
		SLPct:      decimal.NewFromFloat(0.02),
		TPPct:      decimal.NewFromFloat(0.04),
	}
}

func (s *PMax) Name() string { return "pmax" }

func (s *PMax) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "multiplier", Default: s.Multiplier, Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromInt(10)},
		{Name: "ma_length", Default: decimal.NewFromInt(int64(s.MALength)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(300)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.3)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *PMax) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(specByName["atr_period"], v).IntPart())
	}
	if v, ok := values["multiplier"]; ok {
		cp.Multiplier = ClampParam(specByName["multiplier"], v)
	}
	if v, ok := values["ma_length"]; ok {
		cp.MALength = int(ClampParam(specByName["ma_length"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	return &cp
}

func (s *PMax) movingAverage(candles []Candle) (decimal.Decimal, error) {
	switch s.MAType {
	case "SMA":
		return SMA(candles, s.MALength)
	case "WMA":
		return wma(candles, s.MALength)
	default: // "EMA"
		return EMA(candles, s.MALength)
	}
}

func wma(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period {
		return decimal.Zero, errNeedMore(period, len(candles))
	}
	window := candles[len(candles)-period:]
	weightedSum, weightTotal := decimal.Zero, decimal.Zero
	for i, c := range window {
		weight := decimal.NewFromInt(int64(i + 1))
		weightedSum = weightedSum.Add(c.Close.Mul(weight))
		weightTotal = weightTotal.Add(weight)
	}
	return weightedSum.Div(weightTotal), nil
}

func (s *PMax) Evaluate(candles []Candle) (Signal, error) {
	need := s.MALength + s.ATRPeriod + 2
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	maNow, err := s.movingAverage(candles)
	if err != nil {
		return Signal{}, err
	}
	atrNow, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}

	longStop := maNow.Sub(s.Multiplier.Mul(atrNow))
	shortStop := maNow.Add(s.Multiplier.Mul(atrNow))

	trend := s.prevTrend
	if trend == 0 {
		trend = 1
	}
	if trend == -1 && maNow.GreaterThan(shortStop) {
		trend = 1
	} else if trend == 1 && maNow.LessThan(longStop) {
		trend = -1
	}

	prevTrend := s.prevTrend
	s.prevTrend = trend

	if prevTrend == 0 {
		return Signal{Side: Hold}, nil // first evaluable bar: establish trend, no signal yet
	}
	switch {
	case prevTrend == -1 && trend == 1:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case prevTrend == 1 && trend == -1:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
