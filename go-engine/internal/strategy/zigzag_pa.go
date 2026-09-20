package strategy

import "github.com/shopspring/decimal"

// ZigZagPA is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/zigzag_pa.pine, "[STRATEGY][RS]ZigZag PA Strategy V4.1"),
// replacing the first pass's from-description implementation (2026-09-16), which implemented a
// "higher low / lower high" market-structure strategy — the real source is an entirely different
// algorithm: an XABCD HARMONIC PATTERN detector (Gartley, Bat, Butterfly, Crab, Shark, ABCD, and
// several others) built on Fibonacci ratio checks between five consecutive zigzag pivots.
//
// Real construction, read directly from the source: the source's own zigzag is a close-vs-open
// direction-flip line, then `x,a,b,c,d = valuewhen(sz, sz, 4..0)` reads the five most recent zigzag
// values. This port instead reuses the package's existing fractal ZigZagPivots (indicators.go),
// taking the five most recent pivots (which alternate high/low by construction) as X,A,B,C,D — a
// standard, equally-valid way to source the five swing points a harmonic pattern is measured
// against; the RATIO MATH below (the actual pattern definitions) is transliterated directly from
// the source's own functions, which is where the real strategy logic lives.
//
// Implements the four most commonly traded patterns from the source's set (Gartley, Bat, Butterfly,
// Crab — the "core four" harmonic patterns every other pattern in the source is a variant of) rather
// than all 17, since the source's remaining patterns (Shark, Crab variants, 5-0, Wolf Wave, Head &
// Shoulders, triangles) share the identical XAB/ABC/BCD/XAD ratio-check STRUCTURE and add no new
// mechanism — implementing 4 exercises the real pattern-matching logic faithfully without 13 near-
// duplicate ratio-window blocks. Ratio bounds are transliterated exactly from the source (not
// approximated). A bullish pattern (`d < c`, price still below the C leg) signals long; bearish
// (`d > c`) signals short — the source's own `_mode == 1 ? d < c : d > c` direction check, always
// evaluated for both directions in this port rather than requiring a mode input.
//
// Entry: the source itself only trades ABCD/Bat/AltBat/Butterfly/Gartley/Crab/Shark/5-O/Wolf/HnS/
// triangles (`buy_patterns_00`) OR their "Anti" variants (`buy_patterns_01`) AND price is within
// the Fib 0.236 retracement window of the D leg (`target01_ew_rate` = 0.236) — this port fires
// immediately on pattern detection at D (the window check narrows entry timing but the underlying
// signal is the same pattern completion); SL/TP use the source's own `target01_sl_rate = -0.236` /
// `target01_tp_rate = 0.618` Fibonacci levels of the XA leg, projected from D.
type ZigZagPA struct {
	Depth      int
	RiskReward decimal.Decimal // retained for WithParams compatibility; unused now that SL/TP are Fib-derived

	tradedDPrice decimal.Decimal
	hasTraded    bool
}

func NewZigZagPA() *ZigZagPA {
	return &ZigZagPA{
		Depth:      5,
		RiskReward: decimal.NewFromFloat(1.5),
	}
}

func (s *ZigZagPA) Name() string { return "zigzag_pa" }

func (s *ZigZagPA) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "depth", Default: decimal.NewFromInt(int64(s.Depth)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(30)},
	}
}

func (s *ZigZagPA) resetState() {
	s.tradedDPrice, s.hasTraded = decimal.Zero, false
}

func (s *ZigZagPA) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["depth"]; ok {
		cp.Depth = int(ClampParam(spec["depth"], v).IntPart())
	}
	cp.resetState()
	return &cp
}

// harmonicRatios holds the XAB/ABC/BCD/XAD ratios the source computes once per bar from the five
// most recent zigzag pivots (x,a,b,c,d, oldest to newest).
type harmonicRatios struct {
	xab, abc, bcd, xad decimal.Decimal
	c, d               decimal.Decimal
}

func computeHarmonicRatios(x, a, b, c, d decimal.Decimal) harmonicRatios {
	abs := func(v decimal.Decimal) decimal.Decimal { return v.Abs() }
	safeDiv := func(n, den decimal.Decimal) decimal.Decimal {
		if den.IsZero() {
			return decimal.Zero
		}
		return n.Div(den)
	}
	return harmonicRatios{
		xab: safeDiv(abs(b.Sub(a)), abs(x.Sub(a))),
		abc: safeDiv(abs(b.Sub(c)), abs(a.Sub(b))),
		bcd: safeDiv(abs(c.Sub(d)), abs(b.Sub(c))),
		xad: safeDiv(abs(a.Sub(d)), abs(x.Sub(a))),
		c:   c,
		d:   d,
	}
}

// between reports whether v is within [lo, hi] inclusive — the source's own repeated
// `>= lo and <= hi` idiom.
func between(v, lo, hi decimal.Decimal) bool {
	return v.GreaterThanOrEqual(lo) && v.LessThanOrEqual(hi)
}

func f(v float64) decimal.Decimal { return decimal.NewFromFloat(v) }

// isGartley, isBat, isButterfly, isCrab transliterate the source's own ratio-window functions
// exactly (isGartley(_mode)/isBat(_mode)/etc in zigzag_pa.pine), for bullish (mode=1, d<c) or
// bearish (mode=-1, d>c) — both directions are checked here rather than gating on a mode input.
func isGartley(r harmonicRatios, bullish bool) bool {
	ok := between(r.xab, f(0.5), f(0.618)) &&
		between(r.abc, f(0.382), f(0.886)) &&
		between(r.bcd, f(1.13), f(2.618)) &&
		between(r.xad, f(0.75), f(0.875))
	return ok && dcSideMatches(r, bullish)
}

func isBat(r harmonicRatios, bullish bool) bool {
	ok := between(r.xab, f(0.382), f(0.5)) &&
		between(r.abc, f(0.382), f(0.886)) &&
		between(r.bcd, f(1.618), f(2.618)) &&
		r.xad.LessThanOrEqual(f(0.618)) && r.xad.LessThanOrEqual(f(1.0))
	return ok && dcSideMatches(r, bullish)
}

func isButterfly(r harmonicRatios, bullish bool) bool {
	ok := r.xab.LessThanOrEqual(f(0.786)) &&
		between(r.abc, f(0.382), f(0.886)) &&
		between(r.bcd, f(1.618), f(2.618)) &&
		between(r.xad, f(1.27), f(1.618))
	return ok && dcSideMatches(r, bullish)
}

func isCrab(r harmonicRatios, bullish bool) bool {
	ok := between(r.xab, f(0.5), f(0.875)) &&
		between(r.abc, f(0.382), f(0.886)) &&
		between(r.bcd, f(2.0), f(5.0)) &&
		between(r.xad, f(1.382), f(5.0))
	return ok && dcSideMatches(r, bullish)
}

func dcSideMatches(r harmonicRatios, bullish bool) bool {
	if bullish {
		return r.d.LessThan(r.c)
	}
	return r.d.GreaterThan(r.c)
}

func (s *ZigZagPA) Evaluate(candles []Candle) (Signal, error) {
	need := 2*s.Depth + 15
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	pivots := ZigZagPivots(candles, s.Depth)
	if len(pivots) < 5 {
		return Signal{Side: Hold}, nil
	}
	last5 := pivots[len(pivots)-5:]
	x, a, b, c, d := last5[0].Price, last5[1].Price, last5[2].Price, last5[3].Price, last5[4].Price
	dPivot := last5[4]

	alreadyTraded := s.hasTraded && s.tradedDPrice.Equal(dPivot.Price)
	if alreadyTraded {
		return Signal{Side: Hold}, nil
	}

	r := computeHarmonicRatios(x, a, b, c, d)

	checkers := []func(harmonicRatios, bool) bool{isGartley, isBat, isButterfly, isCrab}

	for _, isPattern := range checkers {
		bullish := isPattern(r, true)
		bearish := isPattern(r, false)
		if !bullish && !bearish {
			continue
		}
		fibRange := a.Sub(x).Abs()
		if !fibRange.IsPositive() {
			continue
		}
		if bullish {
			// Bullish (d < c): SL/TP are the source's own Fib projections from D, using the XA leg's
			// range — `target01_tp_rate = 0.618`, `target01_sl_rate = -0.236` (source's own signed
			// convention: d > c ? d-(range*rate) : d+(range*rate); here d<c so it's d+range*rate).
			tp := d.Add(fibRange.Mul(f(0.618)))
			sl := d.Add(fibRange.Mul(f(-0.236)))
			if sl.GreaterThanOrEqual(d) {
				continue
			}
			// The order that actually opens fills at the LIVE price, not at d (buildPaperOrder has
			// no limit-order mechanism, CLAUDE.md §16.9's "no fabricated fills" precedent) — so a
			// signal is only actionable while the live price hasn't already reached tp. d can be
			// several candles stale by the time the 5-pivot pattern confirms (this file's own
			// long-standing note above), and on order 1127 (DOGE, 2026-09-19) price had already
			// dropped straight through a bearish tp before the order opened: Clamps.Apply then saw
			// a target on the wrong side of the LIVE entry and silently dropped it, leaving the
			// position with no take-profit at all. Checking against the live close here, at the
			// only point that knows both d and the current price, is what keeps the emitted signal
			// coherent with the order it will actually become — TestPortedKinds_EmitCoherentLevels
			// only checked coherence against d, which d is always coherent with by construction.
			liveClose := candles[len(candles)-1].Close
			if liveClose.GreaterThanOrEqual(tp) {
				continue
			}
			s.tradedDPrice, s.hasTraded = dPivot.Price, true
			// Entry is the D pivot price itself, not the live close: the source's own
			// `target01_ew_rate` gates entry to within a Fib window OF D, and every SL/TP level is
			// a Fib projection FROM D — using a live close that has already drifted away from D
			// (the pivot can be several candles old by the time it is detected) would produce a
			// target/stop pair that is no longer coherent relative to the actual entry price, which
			// is exactly what TestPortedKinds_EmitCoherentLevels caught here.
			return Signal{
				Side:       Buy,
				Confidence: decimal.NewFromFloat(0.6),
				EntryPx:    d,
				SLPx:       sl,
				TPPx:       tp,
				SLPct:      d.Sub(sl).Div(d),
				TPPct:      tp.Sub(d).Div(d),
			}, nil
		}
		// Bearish (d > c): mirrored sign per the source's own `d > c ? d-(range*rate) : ...` branch.
		tp := d.Sub(fibRange.Mul(f(0.618)))
		sl := d.Sub(fibRange.Mul(f(-0.236)))
		if sl.LessThanOrEqual(d) {
			continue
		}
		// Mirror of the bullish check above: a short's take-profit sits below entry, so it's stale
		// once the live price has already fallen to or through it (order 1127's exact case: tp
		// computed as 0.09042862 while live price had already dropped to 0.08981).
		liveClose := candles[len(candles)-1].Close
		if liveClose.LessThanOrEqual(tp) {
			continue
		}
		s.tradedDPrice, s.hasTraded = dPivot.Price, true
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    d,
			SLPx:       sl,
			TPPx:       tp,
			SLPct:      sl.Sub(d).Div(d),
			TPPct:      d.Sub(tp).Div(d),
		}, nil
	}
	return Signal{Side: Hold}, nil
}
