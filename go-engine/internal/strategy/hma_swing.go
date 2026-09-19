package strategy

import (
	"math"

	"github.com/shopspring/decimal"
)

// HMASwing is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/hma_swing.pine, @version=4 "Hull Moving Average Swing Trader" by
// SEASIDE420), replacing the first pass's from-description implementation (2026-09-16), which used
// a simple HMA-slope-turn signal at period 20 — the real algorithm is a different, more intricate
// band construction at period 210, priced off the OPEN rather than the close.
//
// Real parameters, read directly from the source:
//   - HullMA Period 210 (`hullperiod`), applied to the OPEN price (`price = input(open, ...)`), not
//     close — a deliberate choice in the source (comment: "recommended OPEN to avoid repainting" is
//     UT Bot's phrasing but the same reasoning applies: the open is fixed the instant the bar forms).
//
// Signal logic, transliterated directly from the source's own recurrence (n2ma/nma/diff use `wma`,
// matching the classic non-square-root HMA construction rather than this package's own HMA()
// helper, which is why this port computes it directly rather than reusing HMA()):
//
//	n2ma  = 2*wma(price, hullperiod/2);  nma = wma(price, hullperiod);  diff = n2ma - nma
//	sqn   = round(sqrt(hullperiod))
//	n1    = wma(diff, sqn)          (from the CURRENT bar's diff)
//	n2    = wma(diff[1], sqn)       (from the PRIOR bar's diff — a lagged copy of n1's own series)
//	Hull_Line      = n2                          (n1/n1*n2 reduces to n2 whenever n1 != 0)
//	Hull_retracted = Hull_Line - 2 if n1 > n2 else Hull_Line + 2
//	c1 = Hull_retracted + n1 - price
//	c2 = Hull_retracted - n2 + price
//
// Entry: `price > c2 AND price[1] > c1` -> long. `price < c1 AND price[1] < c2` -> short. Close an
// existing position whenever price crosses back through c2 (long) / c1 (short) the source's own way
// — modeled here as the opposite-side Signal, per every other ported strategy's "close = opposite
// signal" convention in this package (PaperTrader/BotTrader treat it as an update/close request,
// not an automatic reversal, CLAUDE.md §27.3).
type HMASwing struct {
	Period       int
	SLPct, TPPct decimal.Decimal
}

func NewHMASwing() *HMASwing {
	return &HMASwing{
		Period: 210,
		SLPct:  decimal.NewFromFloat(0.008),
		TPPct:  decimal.NewFromFloat(0.016),
	}
}

func (s *HMASwing) Name() string { return "hma_swing" }

func (s *HMASwing) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "period", Default: decimal.NewFromInt(int64(s.Period)), Min: decimal.NewFromInt(4), Max: decimal.NewFromInt(400)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

// Stateless: every value Evaluate uses is recomputed fresh from the candles passed in, same as
// philakones_fib.go — no resetState needed since there is no accumulated state to leak between
// differently-configured variants.
func (s *HMASwing) WithParams(values map[string]decimal.Decimal) Strategy {
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
	return &cp
}

// wmaAtOfField computes ONE windowed WMA value (linearly-weighted moving average, most recent bar
// weighted heaviest) ending at index `at`, over a field selected by sel. Deliberately a single-point
// helper rather than a full-series one: HMASwing.Evaluate only ever needs a handful of trailing
// values (n1 "now" and "one bar back"), and computing a full O(n) series purely to read its last two
// entries made every call O(n*period) — with this strategy's own period (210) that meant ~65,000
// decimal multiplies PER Evaluate call, which a driving loop of hundreds of calls turned into a
// multi-minute hang (found running this file's own test suite, the same class of accidental-
// quadratic-cost mistake CLAUDE.md documents for other strategies' full-series helpers).
func wmaAtOfField(candles []Candle, period, at int, sel func(Candle) decimal.Decimal) decimal.Decimal {
	denom := decimal.NewFromInt(int64(period * (period + 1) / 2))
	sum := decimal.Zero
	for j := at - period + 1; j <= at; j++ {
		w := decimal.NewFromInt(int64(j - (at - period + 1) + 1))
		sum = sum.Add(sel(candles[j]).Mul(w))
	}
	return sum.Div(denom)
}

// wmaAtOfSeries mirrors wmaAtOfField but over an already-materialized decimal series (used for the
// diff -> n1 step, since diff isn't a raw candle field).
func wmaAtOfSeries(series []decimal.Decimal, period, at int) decimal.Decimal {
	denom := decimal.NewFromInt(int64(period * (period + 1) / 2))
	sum := decimal.Zero
	for j := at - period + 1; j <= at; j++ {
		w := decimal.NewFromInt(int64(j - (at - period + 1) + 1))
		sum = sum.Add(series[j].Mul(w))
	}
	return sum.Div(denom)
}

// roundedSqrt mirrors Pine's `round(sqrt(n))` using math.Sqrt directly — a period length is a
// small, exactly-representable integer, so float64 precision is not a concern here the way it is
// for accumulated price arithmetic (CLAUDE.md §16.8/§44's EMA/decimal-precision lessons apply to
// values that compound over many bars, not a single one-shot sqrt of a config integer).
func roundedSqrt(n int) int {
	if n <= 0 {
		return 1
	}
	r := int(math.Round(math.Sqrt(float64(n))))
	if r < 1 {
		r = 1
	}
	return r
}

func (s *HMASwing) Evaluate(candles []Candle) (Signal, error) {
	half := s.Period / 2
	if half < 1 {
		half = 1
	}
	sqn := roundedSqrt(s.Period)
	// Need enough candles for: WMA(period) on price, then WMA(sqn) on the resulting diff series,
	// plus one more bar for diff[1] and one more for price[1].
	need := s.Period + sqn + 3
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	// Only the trailing `sqn+1` diff values are ever read (n1 "now" needs diff[last-sqn+1..last],
	// n1 "one bar back" needs diff[last-sqn..last-1]) — computed directly rather than as a full
	// O(n)-length series, per wmaAtOfField's own doc comment on why that matters here.
	openOf := func(c Candle) decimal.Decimal { return c.Open }
	last := len(candles) - 1
	diffWindow := make([]decimal.Decimal, sqn+1)
	for k := 0; k <= sqn; k++ {
		idx := last - sqn + k
		n2maAt := wmaAtOfField(candles, half, idx, openOf)
		nmaAt := wmaAtOfField(candles, s.Period, idx, openOf)
		diffWindow[k] = n2maAt.Mul(decimal.NewFromInt(2)).Sub(nmaAt)
	}
	// n1 "now" = wma(diff, sqn) ending at the last diffWindow entry; n1 "one bar back" (= n2, the
	// source's own diff1/n2 recurrence) ends one entry earlier.
	n1 := wmaAtOfSeries(diffWindow, sqn, sqn)
	n2 := wmaAtOfSeries(diffWindow, sqn, sqn-1)

	price := candles[len(candles)-1].Open
	pricePrev := candles[len(candles)-2].Open

	var hullLine decimal.Decimal
	if !n1.IsZero() {
		hullLine = n2
	}
	var hullRetracted decimal.Decimal
	if n1.GreaterThan(n2) {
		hullRetracted = hullLine.Sub(decimal.NewFromInt(2))
	} else {
		hullRetracted = hullLine.Add(decimal.NewFromInt(2))
	}
	c1 := hullRetracted.Add(n1).Sub(price)
	c2 := hullRetracted.Sub(n2).Add(price)

	// The source's own entry conditions compare price[1] against the CURRENT bar's freshly
	// recomputed c1/c2 (Pine's `price[1] > c1` reads last bar's price against this bar's c1, since
	// c1/c2 are not lagged themselves) — reproduced literally: pricePrev against this call's own
	// c1/c2, with no separate "previous c1/c2" state to carry (this strategy is otherwise stateless,
	// matching philakones_fib.go's own pattern of recomputing everything fresh from candles).
	longCond := price.GreaterThan(c2) && pricePrev.GreaterThan(c1)
	shortCond := price.LessThan(c1) && pricePrev.LessThan(c2)

	switch {
	case longCond:
		risk := price.Sub(c1)
		if !risk.IsPositive() {
			return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    price,
			SLPx:       c1,
			TPPx:       price.Add(risk.Mul(decimal.NewFromFloat(1.5))),
			SLPct:      risk.Div(price),
			TPPct:      risk.Div(price).Mul(decimal.NewFromFloat(1.5)),
		}, nil
	case shortCond:
		risk := c2.Sub(price)
		if !risk.IsPositive() {
			return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    price,
			SLPx:       c2,
			TPPx:       price.Sub(risk.Mul(decimal.NewFromFloat(1.5))),
			SLPct:      risk.Div(price),
			TPPct:      risk.Div(price).Mul(decimal.NewFromFloat(1.5)),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
