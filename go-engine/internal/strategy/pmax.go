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

	// prevLongStop/prevShortStop are the previous bar's RATCHETED bands. Both the ratchet and the
	// comparison-against-previous are load-bearing (see Evaluate): without them this strategy can
	// never emit a signal at all.
	prevLongStop  decimal.Decimal
	prevShortStop decimal.Decimal
	hasPrevStops  bool
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
	cp.resetState()
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

	// The bands RATCHET: a rising MA can only raise the long floor, a falling MA can only lower the
	// short ceiling. This is the whole "trailing stop" character of PMax, and the trend flip is
	// tested against the PREVIOUS bar's band, not the current one.
	//
	// Both details were missing from the original port, which compared maNow against the band
	// derived from that same maNow — i.e. `maNow > maNow + multiplier*atr`, which is impossible for
	// any positive ATR. The trend could therefore never leave its initial +1, and this strategy
	// emitted exactly zero signals for its entire existence while looking perfectly healthy:
	// registered, assignable from the panel, and silently returning Hold forever. Restored to match
	// the Pine source (strategy_PMax Explorer, lines 82-90).
	longStopPrev, shortStopPrev := longStop, shortStop
	if s.hasPrevStops {
		longStopPrev, shortStopPrev = s.prevLongStop, s.prevShortStop
	}
	if maNow.GreaterThan(longStopPrev) && longStop.LessThan(longStopPrev) {
		longStop = longStopPrev
	}
	if maNow.LessThan(shortStopPrev) && shortStop.GreaterThan(shortStopPrev) {
		shortStop = shortStopPrev
	}

	trend := s.prevTrend
	if trend == 0 {
		trend = 1
	}
	if trend == -1 && maNow.GreaterThan(shortStopPrev) {
		trend = 1
	} else if trend == 1 && maNow.LessThan(longStopPrev) {
		trend = -1
	}

	prevTrend := s.prevTrend
	s.prevTrend = trend
	s.prevLongStop, s.prevShortStop, s.hasPrevStops = longStop, shortStop, true

	if prevTrend == 0 {
		return Signal{Side: Hold}, nil // first evaluable bar: establish trend, no signal yet
	}
	// longStop/shortStop are this strategy's OWN invalidation levels — the ATR trailing bands whose
	// crossing is what defines a trend flip here. Emitting them as prices (CLAUDE.md §15.11) means
	// the stop sits exactly where the strategy would consider itself wrong, instead of at a fixed
	// SLPct distance that has nothing to do with the band. The target keeps the configured
	// SLPct:TPPct reward ratio, applied to the real band distance rather than to SLPct.
	close := candles[len(candles)-1].Close
	switch {
	case prevTrend == -1 && trend == 1:
		risk := close.Sub(longStop)
		if !risk.IsPositive() {
			// Price sits at or below its own stop band, so there is no coherent long to describe.
			// Fall back to the configured percentages rather than inventing a level — a wrong level
			// misleads the model more than no level does.
			return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
		}
		reward := rewardDist(risk, s.SLPct, s.TPPct)
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    close,
			SLPx:       longStop,
			TPPx:       close.Add(reward),
			SLPct:      risk.Div(close),
			TPPct:      reward.Div(close),
		}, nil
	case prevTrend == 1 && trend == -1:
		risk := shortStop.Sub(close)
		if !risk.IsPositive() {
			return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
		}
		reward := rewardDist(risk, s.SLPct, s.TPPct)
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    close,
			SLPx:       shortStop,
			TPPx:       close.Sub(reward),
			SLPct:      risk.Div(close),
			TPPct:      reward.Div(close),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}

// rewardDist scales a structural risk distance by the configured SLPct:TPPct reward ratio.
//
// Multiplying before dividing is deliberate: risk*TPPct/SLPct keeps one division where the obvious
// risk*(TPPct/SLPct) has two, and a 16-digit intermediate quotient multiplied back out produced
// several-hundred-digit values headed for NUMERIC columns and the model's JSON.
func rewardDist(risk, slPct, tpPct decimal.Decimal) decimal.Decimal {
	if !slPct.IsPositive() || !tpPct.IsPositive() {
		return risk
	}
	return risk.Mul(tpPct).Div(slPct)
}

// resetState clears accumulated evaluation state, returning the strategy to how it behaves when
// freshly constructed. Called by WithParams, whose copy must not inherit it (see
// Strategy.WithParams for why).
//
// This lives beside the state fields on purpose: it is the one place that has to know what they
// are, so adding a field means updating the reset right here rather than remembering a zeroing
// line buried at the bottom of WithParams.
// A variant must establish its own trend and trailing bands from its own parameters, never
// inherit another configuration's mid-trend position.
func (s *PMax) resetState() {
	s.prevTrend = 0
	s.prevLongStop, s.prevShortStop, s.hasPrevStops = decimal.Zero, decimal.Zero, false
}
