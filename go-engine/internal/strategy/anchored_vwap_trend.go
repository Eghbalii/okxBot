package strategy

import "github.com/shopspring/decimal"

// AnchoredVWAPTrend is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/anchored_vwap_trend.pine, @version=6 "Golden Trident |
// Swing-Anchored VWAP Trend System"), replacing the first pass's from-description implementation
// (2026-09-16), which anchored VWAP at a fractal swing pivot and used an EMA-SLOPE trend context —
// the real source anchors VWAP at a `highestbars`/`lowestbars` structural flip (not a fractal
// pivot), is LONG ONLY by default, and exits purely on structure reversal rather than a
// risk:reward target.
//
// Real parameters, read directly from the source:
//   - Swing Period 30 (`swingLen`) — direction (`dir`) flips to +1 when the current bar IS the
//     highest high of the trailing 30 bars (`highestbars(high, swingLen) == 0`), to -1 when it is
//     the lowest low. This is a genuinely different mechanism from a confirmed fractal pivot: it
//     flips on breakout bars themselves, with no confirmation delay.
//   - `allowShort` = false by default — long only.
//   - EMA Filter length 200 (`emaLen`), required by default (`useEMA` = true).
//   - Chop filter: ATR(20) range filter, `rangeMult` = 0.8 — entries blocked when the recent
//     `highest(high,20)-lowest(low,20)` range is not at least 0.8x ATR(20) (i.e. too narrow/choppy).
//   - Backstop: ATR(14) x 8.0 — a wide, rarely-hit catastrophe stop; the source's own comment states
//     "the strategy is designed to exit on structure reversal well before this level is reached."
//
// Signal logic, exactly the source's own construction: the anchored VWAP itself
// (cumulative-since-last-flip volume-weighted HLC3) is used only for the source's own plot/fill —
// it plays NO role in `longCond`/`shortCond`, which the source derives purely from `dir`, the EMA
// filter, and the chop filter. `longCond = dir==1 AND close>EMA200 AND rangeOK`, triggered on the
// dir-flip edge (`longTrigger = longCond and not longCond[1]`). Exit on the mirrored structure flip
// (`dir==-1 while long -> close`), modeled as this package's opposite-side close convention
// (CLAUDE.md §27.3). SL uses the source's own wide ATR(14)x8 backstop — genuinely a disaster-only
// level per the source's own documentation, not the primary exit mechanism.
type AnchoredVWAPTrend struct {
	SwingLen        int
	EMALen          int
	AllowShort      bool
	ChopATRLen      int
	ChopRangeMult   decimal.Decimal
	BackstopATRLen  int
	BackstopATRMult decimal.Decimal

	dir          int // 1 = up, -1 = down, 0 = undetermined
	wasLongCond  bool
	wasShortCond bool
}

func NewAnchoredVWAPTrend() *AnchoredVWAPTrend {
	return &AnchoredVWAPTrend{
		SwingLen:        30,
		EMALen:          200,
		AllowShort:      false,
		ChopATRLen:      20,
		ChopRangeMult:   decimal.NewFromFloat(0.8),
		BackstopATRLen:  14,
		BackstopATRMult: decimal.NewFromInt(8),
	}
}

func (s *AnchoredVWAPTrend) Name() string { return "anchored_vwap_trend" }

func (s *AnchoredVWAPTrend) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "swing_len", Default: decimal.NewFromInt(int64(s.SwingLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(200)},
		{Name: "ema_len", Default: decimal.NewFromInt(int64(s.EMALen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(400)},
		{Name: "chop_range_mult", Default: s.ChopRangeMult, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromInt(5)},
		{Name: "backstop_atr_mult", Default: s.BackstopATRMult, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(20)},
	}
}

func (s *AnchoredVWAPTrend) resetState() {
	s.dir = 0
	s.wasLongCond, s.wasShortCond = false, false
}

func (s *AnchoredVWAPTrend) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["swing_len"]; ok {
		cp.SwingLen = int(ClampParam(spec["swing_len"], v).IntPart())
	}
	if v, ok := values["ema_len"]; ok {
		cp.EMALen = int(ClampParam(spec["ema_len"], v).IntPart())
	}
	if v, ok := values["chop_range_mult"]; ok {
		cp.ChopRangeMult = ClampParam(spec["chop_range_mult"], v)
	}
	if v, ok := values["backstop_atr_mult"]; ok {
		cp.BackstopATRMult = ClampParam(spec["backstop_atr_mult"], v)
	}
	cp.resetState()
	return &cp
}

func (s *AnchoredVWAPTrend) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.SwingLen+1, s.EMALen+1, s.ChopATRLen+1, s.BackstopATRLen+1)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	last := len(candles) - 1
	window := candles[last-s.SwingLen+1 : last+1]
	highestHigh, lowestLow := window[0].High, window[0].Low
	for _, c := range window[1:] {
		if c.High.GreaterThan(highestHigh) {
			highestHigh = c.High
		}
		if c.Low.LessThan(lowestLow) {
			lowestLow = c.Low
		}
	}
	if candles[last].High.GreaterThanOrEqual(highestHigh) {
		s.dir = 1
	} else if candles[last].Low.LessThanOrEqual(lowestLow) {
		s.dir = -1
	}

	ema, err := EMA(candles, s.EMALen)
	if err != nil {
		return Signal{}, err
	}
	chopHigh, err := Highest(candles, s.ChopATRLen)
	if err != nil {
		return Signal{}, err
	}
	chopLow, err := Lowest(candles, s.ChopATRLen)
	if err != nil {
		return Signal{}, err
	}
	chopATR, err := ATR(candles, s.ChopATRLen)
	if err != nil {
		return Signal{}, err
	}
	backstopATR, err := ATR(candles, s.BackstopATRLen)
	if err != nil {
		return Signal{}, err
	}
	close := candles[last].Close

	rangeOK := chopHigh.Sub(chopLow).GreaterThan(chopATR.Mul(s.ChopRangeMult))

	longCond := s.dir == 1 && close.GreaterThan(ema) && rangeOK
	shortCond := s.AllowShort && s.dir == -1 && close.LessThan(ema) && rangeOK

	longTrigger := longCond && !s.wasLongCond
	shortTrigger := shortCond && !s.wasShortCond
	s.wasLongCond, s.wasShortCond = longCond, shortCond

	switch {
	case longTrigger:
		backstop := close.Sub(backstopATR.Mul(s.BackstopATRMult))
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    close,
			SLPx:       backstop,
			SLPct:      close.Sub(backstop).Div(close),
		}, nil
	case shortTrigger:
		backstop := close.Add(backstopATR.Mul(s.BackstopATRMult))
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    close,
			SLPx:       backstop,
			SLPct:      backstop.Sub(close).Div(close),
		}, nil
	case s.dir == -1 && s.wasLongCond:
		// Structure flip against an open long — the source's own `strategy.close("Long")`,
		// modeled as this package's opposite-side close convention (CLAUDE.md §27.3).
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.5)}, nil
	case s.dir == 1 && s.wasShortCond:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.5)}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
