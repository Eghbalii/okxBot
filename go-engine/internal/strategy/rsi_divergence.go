package strategy

import "github.com/shopspring/decimal"

// RSIDivergence is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/rsi_divergence.pine, @version=4 "RSI Divergence Indicator"),
// replacing the first pass's from-description implementation (2026-09-16), which used RSI(14), a
// symmetric fractal depth of 5, and both long and short sides — the real source uses RSI(9), an
// ASYMMETRIC pivot lookback (1 bar left, 3 bars right), and is LONG ONLY.
//
// Real parameters, read directly from the source:
//   - RSI period 9 (`len`), not 14.
//   - Pivot Lookback Left = 1, Pivot Lookback Right = 3 (`lbL`/`lbR`) — asymmetric, unlike a
//     standard fractal's symmetric depth. `pivotlow(osc, lbL, lbR)` confirms a pivot once it has 1
//     bar of "look-left" and 3 bars of "look-right" confirmation.
//   - `rangeLower`/`rangeUpper` = 5/60: the two pivots being compared must be between 5 and 60 bars
//     apart (`_inRange`), rejecting divergences that are either too close together (noise) or too
//     far apart (unrelated swings) — a real filter the first pass's port omitted entirely.
//   - `takeProfitRSILevel` = 80: exit on RSI crossing above 80, IN ADDITION to a bearish-divergence
//     exit — this port keeps only the divergence exit (mirrored as a Sell/close signal) since an
//     RSI-level exit has no natural place in this package's Strategy interface without carrying
//     extra cross-call RSI state; the divergence exit is the source's OTHER (and primary) exit path.
//   - `sl_type` defaults to "NONE" (no stop at all) — SL/TP are a documented addition here
//     regardless, since every strategy in this registry needs one (CLAUDE.md §16.9).
//
// Signal logic, exactly the source's own "Regular Bullish" condition: price makes a LOWER pivot low
// (`priceLL = low[lbR] < valuewhen(plFound, low[lbR], 1)`) while RSI makes a HIGHER pivot low
// (`oscHL = osc[lbR] > valuewhen(plFound, osc[lbR], 1)`) at the SAME pivot, and the two pivots being
// compared are within [rangeLower, rangeUpper] bars of each other. This is the classic bullish
// divergence. The source's own "Hidden Bullish"/"Regular Bearish"/"Hidden Bearish" plots exist but
// only "Regular Bullish" feeds `strategy.entry` — the other three are indicator-only in the source
// (no `strategy.entry`/`strategy.close` reads them), so only regular bullish divergence is
// implemented here as the entry signal; regular bearish divergence closes an open long (the source's
// `longCloseCondition = crossover(osc, 80) or bearCond`), modeled as this package's opposite-side
// close convention.
type RSIDivergence struct {
	RSIPeriod          int
	PivotLookbackLeft  int
	PivotLookbackRight int
	RangeLower         int
	RangeUpper         int
	SLPct, TPPct       decimal.Decimal

	tradedLowPrice  decimal.Decimal
	tradedHighPrice decimal.Decimal
	hasTradedLow    bool
	hasTradedHigh   bool
}

func NewRSIDivergence() *RSIDivergence {
	return &RSIDivergence{
		RSIPeriod:          9,
		PivotLookbackLeft:  1,
		PivotLookbackRight: 3,
		RangeLower:         5,
		RangeUpper:         60,
		SLPct:              decimal.NewFromFloat(0.02),
		TPPct:              decimal.NewFromFloat(0.04),
	}
}

func (s *RSIDivergence) Name() string { return "rsi_divergence" }

func (s *RSIDivergence) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "rsi_period", Default: decimal.NewFromInt(int64(s.RSIPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "pivot_lookback_left", Default: decimal.NewFromInt(int64(s.PivotLookbackLeft)), Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(20)},
		{Name: "pivot_lookback_right", Default: decimal.NewFromInt(int64(s.PivotLookbackRight)), Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(20)},
		{Name: "range_lower", Default: decimal.NewFromInt(int64(s.RangeLower)), Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(60)},
		{Name: "range_upper", Default: decimal.NewFromInt(int64(s.RangeUpper)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(200)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *RSIDivergence) resetState() {
	s.tradedLowPrice, s.tradedHighPrice = decimal.Zero, decimal.Zero
	s.hasTradedLow, s.hasTradedHigh = false, false
}

func (s *RSIDivergence) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["rsi_period"]; ok {
		cp.RSIPeriod = int(ClampParam(spec["rsi_period"], v).IntPart())
	}
	if v, ok := values["pivot_lookback_left"]; ok {
		cp.PivotLookbackLeft = int(ClampParam(spec["pivot_lookback_left"], v).IntPart())
	}
	if v, ok := values["pivot_lookback_right"]; ok {
		cp.PivotLookbackRight = int(ClampParam(spec["pivot_lookback_right"], v).IntPart())
	}
	if v, ok := values["range_lower"]; ok {
		cp.RangeLower = int(ClampParam(spec["range_lower"], v).IntPart())
	}
	if v, ok := values["range_upper"]; ok {
		cp.RangeUpper = int(ClampParam(spec["range_upper"], v).IntPart())
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

// rsiPivot is a confirmed RSI pivot (low or high) at a candle index, carrying both the RSI value
// and the price extreme at that same bar — divergence compares price against RSI at the SAME pivot.
type rsiPivot struct {
	index      int
	rsiValue   decimal.Decimal
	priceValue decimal.Decimal
}

// rsiSeriesWindowed computes RSI(period) at every index of candles from index `period` onward
// (earlier indices are the zero value), using the same simple windowed-average RSI() already
// implements, but computed once as a series rather than re-scanning the whole trailing window on
// every candidate index — the same "series, not O(n) repeated whole-window calls" reasoning
// EMASeries exists for.
func rsiSeriesWindowed(candles []Candle, period int) []decimal.Decimal {
	out := make([]decimal.Decimal, len(candles))
	if len(candles) < period+1 {
		return out
	}
	periodDec := decimal.NewFromInt(int64(period))
	for i := period; i < len(candles); i++ {
		avgGain, avgLoss := decimal.Zero, decimal.Zero
		for j := i - period + 1; j <= i; j++ {
			delta := candles[j].Close.Sub(candles[j-1].Close)
			if delta.IsPositive() {
				avgGain = avgGain.Add(delta)
			} else {
				avgLoss = avgLoss.Add(delta.Neg())
			}
		}
		avgGain = avgGain.Div(periodDec)
		avgLoss = avgLoss.Div(periodDec)
		if avgLoss.IsZero() {
			out[i] = decimal.NewFromInt(100)
			continue
		}
		rs := avgGain.Div(avgLoss)
		out[i] = decimal.NewFromInt(100).Sub(decimal.NewFromInt(100).Div(decimal.NewFromInt(1).Add(rs)))
	}
	return out
}

// rsiPivotsAsymmetric finds RSI pivot lows/highs with an ASYMMETRIC lookback (left != right bars),
// matching the source's own lbL=1/lbR=3 default — ZigZagPivots (indicators.go) only supports a
// symmetric depth, so this is a dedicated scan rather than a reuse.
func rsiPivotsAsymmetric(candles []Candle, rsiPeriod, left, right int) (lows, highs []rsiPivot) {
	rsiSeries := rsiSeriesWindowed(candles, rsiPeriod)
	warmup := rsiPeriod + 1
	for i := warmup + left; i < len(candles)-right; i++ {
		rsiHere := rsiSeries[i]
		isLow, isHigh := true, true
		for j := i - left; j <= i+right; j++ {
			if j == i {
				continue
			}
			rsiJ := rsiSeries[j]
			if rsiJ.LessThanOrEqual(rsiHere) {
				isHigh = false
			}
			if rsiJ.GreaterThanOrEqual(rsiHere) {
				isLow = false
			}
		}
		if isLow {
			lows = append(lows, rsiPivot{index: i, rsiValue: rsiHere, priceValue: candles[i].Low})
		}
		if isHigh {
			highs = append(highs, rsiPivot{index: i, rsiValue: rsiHere, priceValue: candles[i].High})
		}
	}
	return lows, highs
}

func (s *RSIDivergence) Evaluate(candles []Candle) (Signal, error) {
	need := s.RSIPeriod + s.PivotLookbackLeft + s.PivotLookbackRight + s.RangeUpper + 5
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	// Bound the scan to a recent window for cost, wide enough to still find pivots up to
	// RangeUpper bars apart plus pivot-confirmation slack.
	scanFrom := len(candles) - (s.RangeUpper + s.PivotLookbackLeft + s.PivotLookbackRight + 20)
	if scanFrom < 0 {
		scanFrom = 0
	}
	lows, highs := rsiPivotsAsymmetric(candles[scanFrom:], s.RSIPeriod, s.PivotLookbackLeft, s.PivotLookbackRight)
	last := candles[len(candles)-1]

	// Regular bullish divergence: price lower low, RSI higher low, within the source's own
	// [rangeLower, rangeUpper] bar-distance window.
	if n := len(lows); n >= 2 {
		latest, prior := lows[n-1], lows[n-2]
		barsApart := latest.index - prior.index
		inRange := barsApart >= s.RangeLower && barsApart <= s.RangeUpper
		alreadyTraded := s.hasTradedLow && s.tradedLowPrice.Equal(latest.priceValue)
		if inRange && !alreadyTraded &&
			latest.priceValue.LessThan(prior.priceValue) && latest.rsiValue.GreaterThan(prior.rsiValue) {
			risk := last.Close.Mul(s.SLPct)
			s.tradedLowPrice, s.hasTradedLow = latest.priceValue, true
			return Signal{
				Side:       Buy,
				Confidence: decimal.NewFromFloat(0.6),
				EntryPx:    last.Close,
				SLPx:       last.Close.Sub(risk),
				TPPx:       last.Close.Add(last.Close.Mul(s.TPPct)),
				SLPct:      s.SLPct,
				TPPct:      s.TPPct,
			}, nil
		}
	}

	// Regular bearish divergence closes an open long in the source (crossover(osc,80) or bearCond) —
	// modeled as the opposite-side close signal, per this package's convention (CLAUDE.md §27.3).
	if n := len(highs); n >= 2 {
		latest, prior := highs[n-1], highs[n-2]
		barsApart := latest.index - prior.index
		inRange := barsApart >= s.RangeLower && barsApart <= s.RangeUpper
		alreadyTraded := s.hasTradedHigh && s.tradedHighPrice.Equal(latest.priceValue)
		if inRange && !alreadyTraded &&
			latest.priceValue.GreaterThan(prior.priceValue) && latest.rsiValue.LessThan(prior.rsiValue) {
			s.tradedHighPrice, s.hasTradedHigh = latest.priceValue, true
			return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
		}
	}

	return Signal{Side: Hold}, nil
}
