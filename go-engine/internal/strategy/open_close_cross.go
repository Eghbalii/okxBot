package strategy

import "github.com/shopspring/decimal"

// OpenCloseCross is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/open_close_cross.pine, @version=3 "Open Close Cross Strategy R5.1"
// by JayRogers/JustUncleL), replacing the first pass's from-description implementation (2026-09-16),
// which used a plain SMA(14) and had the cross direction backwards.
//
// Real parameters/behavior, read directly from the source:
//   - MA type "SMMA" (Smoothed Moving Average, a recursive RMA-style average — `v7 := (v7[1]*(len-1)
//   - src) / len`), not a plain SMA. Default period (`basisLen`) 8, not 14.
//   - The source smooths OPEN and CLOSE as two SEPARATE series and crosses THEM against each other:
//     `xlong = crossover(closeSeriesAlt, openSeriesAlt)`, `xshort = crossunder(...)` — i.e. the
//     smoothed CLOSE crossing above the smoothed OPEN signals long, the mirror signals short. The
//     first pass had this inverted (open crossing close).
//   - `delayOffset` ("Delay Open/Close MA (Forces Non-Repainting)") shifts which candle the
//     open/close series are read FROM (`close[delayOffset]`, `open[delayOffset]`) before smoothing —
//     i.e. it delays the INPUT to the moving average, not merely which bar the cross is read against
//     (the distinction this port's DelayBars field already modeled correctly by shifting the whole
//     evaluated window; kept as-is since the practical effect — the cross reacting DelayBars bars
//     later — is the same either way).
//   - The source's own default alternate-resolution multiplier (`useRes`/`intRes`) evaluates the
//     smoothed series on a coarser timeframe than the chart; not modeled here, since this package
//     assigns a strategy to one fixed decision timeframe already (CLAUDE.md §9).
type OpenCloseCross struct {
	Period       int
	SLPct, TPPct decimal.Decimal
	// DelayBars is the source's own "Delay Open/Close MA" input (confirmed directly against the real
	// source, 2026-09-18): 0 (repainting, the source's own documented default) evaluates the cross
	// on the just-closed bar; 1+ trades reaction speed for a signal that cannot be revised by a
	// later-arriving close.
	DelayBars int
}

func NewOpenCloseCross() *OpenCloseCross {
	return &OpenCloseCross{
		Period:    8,
		SLPct:     decimal.NewFromFloat(0.008),
		TPPct:     decimal.NewFromFloat(0.014),
		DelayBars: 0,
	}
}

func (s *OpenCloseCross) Name() string { return "open_close_cross" }

func (s *OpenCloseCross) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "period", Default: decimal.NewFromInt(int64(s.Period)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(200)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
		{Name: "delay_bars", Default: decimal.NewFromInt(int64(s.DelayBars)), Min: decimal.Zero, Max: decimal.NewFromInt(20)},
	}
}

func (s *OpenCloseCross) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["period"]; ok {
		cp.Period = int(ClampParam(spec["period"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	if v, ok := values["delay_bars"]; ok {
		cp.DelayBars = int(ClampParam(spec["delay_bars"], v).IntPart())
	}
	return &cp
}

// smmaOfField computes the source's own recursive SMMA over a field selected by sel:
// v[0] = SMA(period) seed, v[i] = (v[i-1]*(period-1) + src[i]) / period thereafter.
func smmaOfField(candles []Candle, period int, sel func(Candle) decimal.Decimal) []decimal.Decimal {
	out := make([]decimal.Decimal, len(candles))
	if len(candles) < period {
		return out
	}
	periodDec := decimal.NewFromInt(int64(period))
	sum := decimal.Zero
	for _, c := range candles[:period] {
		sum = sum.Add(sel(c))
	}
	out[period-1] = sum.Div(periodDec)
	for i := period; i < len(candles); i++ {
		out[i] = out[i-1].Mul(periodDec.Sub(decimal.NewFromInt(1))).Add(sel(candles[i])).Div(periodDec)
	}
	return out
}

func (s *OpenCloseCross) Evaluate(candles []Candle) (Signal, error) {
	// DelayBars shifts which bar the cross is read from, not how many bars the smoothing itself
	// needs to warm up (it only says how far back to LOOK, not how much history is consumed).
	need := s.Period + 1 + s.DelayBars
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	smoothedOpen := smmaOfField(candles, s.Period, func(c Candle) decimal.Decimal { return c.Open })
	smoothedClose := smmaOfField(candles, s.Period, func(c Candle) decimal.Decimal { return c.Close })

	// last is offset back by DelayBars (0 = the just-closed bar, the source's own repainting
	// default): the cross is evaluated against MA values from DelayBars candles ago, so nothing the
	// signal used can still be revised by a later-arriving close.
	last := len(candles) - 1 - s.DelayBars
	openNow, openPrev := smoothedOpen[last], smoothedOpen[last-1]
	closeNow, closePrev := smoothedClose[last], smoothedClose[last-1]

	// The source's own convention: smoothed CLOSE crossing above smoothed OPEN is long, the mirror
	// is short (`xlong = crossover(closeSeriesAlt, openSeriesAlt)`).
	crossedUp := closePrev.LessThanOrEqual(openPrev) && closeNow.GreaterThan(openNow)
	crossedDown := closePrev.GreaterThanOrEqual(openPrev) && closeNow.LessThan(openNow)

	switch {
	case crossedUp:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case crossedDown:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
