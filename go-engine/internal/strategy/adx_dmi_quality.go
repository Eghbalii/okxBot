package strategy

import "github.com/shopspring/decimal"

// ADXDMIQuality is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/adx_dmi_quality.pine, @version=6 "DMI/ADX Trend Dashboard v3
// (Pullback + Decay)"), replacing the first pass's from-description implementation (2026-09-16),
// which used a bare DI-crossover-plus-ADX-threshold signal — the real source is considerably richer
// (a pullback-entry mode, a trend-quality scoring system, a chandelier trailing stop, an ADX-decay
// exit, a cooldown timer). This port implements the source's REGIME + ENTRY logic and its Chandelier
// stop faithfully (the mechanism that actually shapes what "a signal" means here); the source's
// quality-score-driven POSITION SIZING and ADX-decay/cooldown EXIT refinements are documented but
// not modeled, since this package's Signal has no sizing or multi-bar-managed-exit concept of its
// own — those live in the RL sizing/update layer (CLAUDE.md §15.4/§15.12), not in a strategy's
// entry signal.
//
// Real parameters, read directly from the source:
//   - DI Length 14, ADX Smoothing 14 (`diLen`/`adxLen`).
//   - Min ADX Threshold 20 (`adxThresh`), not 25 — the source's own default is the more permissive
//     end of Wilder's commonly cited 20-25 "trending" range, not the conservative end.
//   - Min DI Spread 10.0 (`diSpreadMin`) — a real filter the first pass omitted: `|+DI - -DI| > 10`
//     required in addition to the ADX threshold.
//   - Trend EMA length 50 (`emaLen`), required (`useEMAFilter` = true by default).
//   - Pullback entries ON by default (`usePullbackEntry` = true), Pullback Trigger EMA length 9
//     (`emaFastLen`).
//   - ATR Length 14, Initial Stop ATR Mult 2.0; Chandelier enabled by default, lookback 22, ATR mult
//     3.0.
//
// Signal logic, exactly the source's own regime + pullback-entry construction:
//   - longRegime  = (+DI > -DI) AND ADX >= 20 AND |+DI - -DI| > 10 AND close > EMA(50).
//   - A "pullback" arms when, WHILE longRegime holds, price crosses UNDER the fast EMA(9) — then
//     fires once price crosses back OVER the fast EMA while still in longRegime. This is a genuine
//     two-step state machine (arm-then-trigger), not a bare crossover — kept as struct state here
//     rather than re-derived per call, since re-deriving "did price dip below fast EMA at some point
//     since regime started" from a candle window alone would need scanning back to the regime's own
//     start, which the source itself tracks incrementally via `var bool longPullback`.
//   - Stop: `strategy.position_avg_price -/+ ATR*2.0` at entry, then the CHANDELIER stop
//     (`highest(high,22) - ATR*3.0` for longs) takes over and only ever tightens in the trade's
//     favor (`math.max(longStop[1], chandLong)`) — reproduced here as the entry-time stop only,
//     since in-trade stop management belongs to the SL/TP-adjust layer (CLAUDE.md §15.4), not to a
//     fresh Evaluate() call with no memory of "how long has this position been open."
type ADXDMIQuality struct {
	Period             int
	ADXThreshold       decimal.Decimal
	DISpreadMin        decimal.Decimal
	TrendEMALen        int
	FastEMALen         int
	ATRPeriod          int
	InitialStopATRMult decimal.Decimal
	ChandelierLen      int
	ChandelierATRMult  decimal.Decimal

	longPullback, shortPullback   bool
	wasLongRegime, wasShortRegime bool
}

func NewADXDMIQuality() *ADXDMIQuality {
	return &ADXDMIQuality{
		Period:             14,
		ADXThreshold:       decimal.NewFromInt(20),
		DISpreadMin:        decimal.NewFromInt(10),
		TrendEMALen:        50,
		FastEMALen:         9,
		ATRPeriod:          14,
		InitialStopATRMult: decimal.NewFromInt(2),
		ChandelierLen:      22,
		ChandelierATRMult:  decimal.NewFromInt(3),
	}
}

func (s *ADXDMIQuality) Name() string { return "adx_dmi_quality" }

func (s *ADXDMIQuality) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "period", Default: decimal.NewFromInt(int64(s.Period)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "adx_threshold", Default: s.ADXThreshold, Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(60)},
		{Name: "di_spread_min", Default: s.DISpreadMin, Min: decimal.Zero, Max: decimal.NewFromInt(50)},
		{Name: "trend_ema_len", Default: decimal.NewFromInt(int64(s.TrendEMALen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "fast_ema_len", Default: decimal.NewFromInt(int64(s.FastEMALen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "chandelier_len", Default: decimal.NewFromInt(int64(s.ChandelierLen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(100)},
		{Name: "chandelier_mult", Default: s.ChandelierATRMult, Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromInt(10)},
	}
}

func (s *ADXDMIQuality) resetState() {
	s.longPullback, s.shortPullback = false, false
	s.wasLongRegime, s.wasShortRegime = false, false
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
	if v, ok := values["di_spread_min"]; ok {
		cp.DISpreadMin = ClampParam(spec["di_spread_min"], v)
	}
	if v, ok := values["trend_ema_len"]; ok {
		cp.TrendEMALen = int(ClampParam(spec["trend_ema_len"], v).IntPart())
	}
	if v, ok := values["fast_ema_len"]; ok {
		cp.FastEMALen = int(ClampParam(spec["fast_ema_len"], v).IntPart())
	}
	if v, ok := values["chandelier_len"]; ok {
		cp.ChandelierLen = int(ClampParam(spec["chandelier_len"], v).IntPart())
	}
	if v, ok := values["chandelier_mult"]; ok {
		cp.ChandelierATRMult = ClampParam(spec["chandelier_mult"], v)
	}
	cp.resetState()
	return &cp
}

func (s *ADXDMIQuality) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(2*s.Period+2, s.TrendEMALen+1, s.FastEMALen+2, s.ChandelierLen+1, s.ATRPeriod+1)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	// ADX()/DirectionalIndex() compute a DX value at EVERY bar of the window they're given
	// (dxSeries, indicators.go) — genuinely O(len(candles)*Period) per call, since that function has
	// no trailing-window bound of its own. Passing this strategy's full, ever-growing candle history
	// into it turned every Evaluate call into work proportional to the ENTIRE trade history, and a
	// driving loop of hundreds of calls over a 1000+ candle window (found running this file's own
	// test suite) made that a multi-minute hang. A trailing window a few times `need` wide gives ADX
	// enough warm-up to smooth correctly while keeping the cost bounded and roughly constant per
	// call, regardless of how much history PaperTrader has accumulated.
	window := candles
	if trim := need * 4; len(window) > trim {
		window = window[len(window)-trim:]
	}
	plusDI, minusDI, err := DirectionalIndex(window, s.Period)
	if err != nil {
		return Signal{}, err
	}
	adx, err := ADX(window, s.Period)
	if err != nil {
		return Signal{}, err
	}
	trendEMA, err := EMA(window, s.TrendEMALen)
	if err != nil {
		return Signal{}, err
	}
	fastEMASeries, err := EMASeries(window, s.FastEMALen)
	if err != nil {
		return Signal{}, err
	}
	atr, err := ATR(window, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	chandHigh, err := Highest(window, s.ChandelierLen)
	if err != nil {
		return Signal{}, err
	}
	chandLow, err := Lowest(window, s.ChandelierLen)
	if err != nil {
		return Signal{}, err
	}

	last := len(window) - 1
	close := window[last].Close
	closePrev := window[last-1].Close
	fastEMANow, fastEMAPrev := fastEMASeries[last], fastEMASeries[last-1]

	diSpread := plusDI.Sub(minusDI).Abs()
	adxOK := adx.GreaterThanOrEqual(s.ADXThreshold)
	spreadOK := diSpread.GreaterThan(s.DISpreadMin)

	longRegime := plusDI.GreaterThan(minusDI) && adxOK && spreadOK && close.GreaterThan(trendEMA)
	shortRegime := minusDI.GreaterThan(plusDI) && adxOK && spreadOK && close.LessThan(trendEMA)

	// Arm the pullback flag on a dip below/above the fast EMA while the regime holds; drop it the
	// moment the regime itself breaks — exactly the source's own `if longRegime and
	// crossunder(close, emaFast) ... if not longRegime ... longPullback := false` pair.
	crossedUnderFast := closePrev.GreaterThanOrEqual(fastEMAPrev) && close.LessThan(fastEMANow)
	crossedOverFast := closePrev.LessThanOrEqual(fastEMAPrev) && close.GreaterThan(fastEMANow)

	if longRegime && crossedUnderFast {
		s.longPullback = true
	}
	if !longRegime {
		s.longPullback = false
	}
	if shortRegime && crossedOverFast {
		s.shortPullback = true
	}
	if !shortRegime {
		s.shortPullback = false
	}

	longTrigger := s.longPullback && crossedOverFast && longRegime
	shortTrigger := s.shortPullback && crossedUnderFast && shortRegime
	if longTrigger {
		s.longPullback = false
	}
	if shortTrigger {
		s.shortPullback = false
	}

	quality := adx.Div(decimal.NewFromInt(40))
	if quality.GreaterThan(decimal.NewFromInt(1)) {
		quality = decimal.NewFromInt(1)
	}

	switch {
	case longTrigger:
		stop := close.Sub(atr.Mul(s.InitialStopATRMult))
		chandStop := chandHigh.Sub(atr.Mul(s.ChandelierATRMult))
		if chandStop.GreaterThan(stop) {
			stop = chandStop
		}
		risk := close.Sub(stop)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: quality,
			EntryPx:    close,
			SLPx:       stop,
			TPPx:       close.Add(risk.Mul(decimal.NewFromFloat(1.5))),
			SLPct:      risk.Div(close),
			TPPct:      risk.Div(close).Mul(decimal.NewFromFloat(1.5)),
		}, nil
	case shortTrigger:
		stop := close.Add(atr.Mul(s.InitialStopATRMult))
		chandStop := chandLow.Add(atr.Mul(s.ChandelierATRMult))
		if chandStop.LessThan(stop) {
			stop = chandStop
		}
		risk := stop.Sub(close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: quality,
			EntryPx:    close,
			SLPx:       stop,
			TPPx:       close.Sub(risk.Mul(decimal.NewFromFloat(1.5))),
			SLPct:      risk.Div(close),
			TPPct:      risk.Div(close).Mul(decimal.NewFromFloat(1.5)),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
