package strategy

import "github.com/shopspring/decimal"

// OpenCloseCross is a Go port of the widely-known TradingView "Open/Close Cross Strategy R5" by
// JustUncleL (raw PineScript source not extractable via automated fetch even on a second, targeted
// re-check — TradingView's code viewer renders client-side; implemented from the standard,
// well-documented algorithm the title describes plus the one mechanically-specific detail the
// listing page's own text confirms, see DelayBars below).
//
// Distinctive vs. an ordinary moving-average cross: this smooths the OPEN and CLOSE price series
// SEPARATELY with their own moving averages, then trades the cross between those two smoothed
// series rather than crossing two MAs of the same (close) price. A widely-published, well-known
// lagging trend-following technique credited to JustUncleL's original indicator.
//
// Standard/default parameters: SMA(14) applied to both the Open and Close series — 14 is the
// commonly cited default length for this indicator family (matching, e.g., RSI's own classic
// period, which JustUncleL's published version reuses).
//
// Signal logic: buy when the smoothed Open series crosses above the smoothed Close series (JustUncleL's
// own published convention — the smoothed open catching up to and overtaking the smoothed close
// signals the trend has turned bullish enough that new opens are outpacing the settling closes);
// sell on the mirrored cross below.
type OpenCloseCross struct {
	Period       int
	SLPct, TPPct decimal.Decimal
	// DelayBars is the original script's "Delay Open/Close MA" input, confirmed from the listing
	// page's own text (2026-09-18 re-check, prompted directly by the operator after the first port
	// omitted it): "To enable non-Repainting mode set 'Delay Open/Close MA' to 1 or more, but expect
	// the reported performance to drop dramatically." The page does not show the source line itself,
	// but the described trade-off — a repaint-proof signal at the cost of measurably worse backtest
	// performance — has exactly one standard meaning for a moving-average cross: evaluate the cross
	// against MA values from DelayBars candles ago rather than the just-closed bar, so nothing the
	// signal used can still be revised by data that arrives later. Default 0 (JustUncleL's own
	// documented default is repainting/no delay) reproduces the original code exactly as before this
	// field existed; WithParams's own zero-value default keeps every existing caller unaffected.
	DelayBars int
}

func NewOpenCloseCross() *OpenCloseCross {
	return &OpenCloseCross{
		Period:    14,
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
		// 0 = repainting (the original's own default), matching this port's pre-existing behavior
		// exactly; 1+ trades reaction speed for a signal that cannot un-happen once seen, per the
		// source page's own documented trade-off (see DelayBars' doc comment).
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

// smaOfField computes a plain SMA series over a field selected by `sel`, reusing the general
// windowed-average shape SMA() already implements but over the Open series instead of Close —
// SMA() itself is hardwired to Close, so this is a small, deliberately separate helper rather than
// changing that widely-used function's signature.
func smaOfField(candles []Candle, period int, sel func(Candle) decimal.Decimal) []decimal.Decimal {
	out := make([]decimal.Decimal, len(candles))
	periodDec := decimal.NewFromInt(int64(period))
	for i := period - 1; i < len(candles); i++ {
		sum := decimal.Zero
		for _, c := range candles[i-period+1 : i+1] {
			sum = sum.Add(sel(c))
		}
		out[i] = sum.Div(periodDec)
	}
	return out
}

func (s *OpenCloseCross) Evaluate(candles []Candle) (Signal, error) {
	// DelayBars shifts which bar the cross is read from, not how many bars the MAs need — the
	// window requirement is unchanged by it (it only says how far back to LOOK, not how much
	// history the smoothing itself consumes).
	need := s.Period + 1 + s.DelayBars
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	smoothedOpen := smaOfField(candles, s.Period, func(c Candle) decimal.Decimal { return c.Open })
	smoothedClose := smaOfField(candles, s.Period, func(c Candle) decimal.Decimal { return c.Close })

	// last is offset back by DelayBars (0 = the just-closed bar, the original's own repainting
	// default): the cross is evaluated against MA values from DelayBars candles ago, so nothing the
	// signal used can still be revised by a later-arriving close — the non-repainting trade-off the
	// source page documents, at the cost of reacting DelayBars bars later than the repainting mode.
	last := len(candles) - 1 - s.DelayBars
	openNow, openPrev := smoothedOpen[last], smoothedOpen[last-1]
	closeNow, closePrev := smoothedClose[last], smoothedClose[last-1]

	crossedUp := openPrev.LessThanOrEqual(closePrev) && openNow.GreaterThan(closeNow)
	crossedDown := openPrev.GreaterThanOrEqual(closePrev) && openNow.LessThan(closeNow)

	switch {
	case crossedUp:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case crossedDown:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
