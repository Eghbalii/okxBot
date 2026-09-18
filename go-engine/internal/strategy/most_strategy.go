package strategy

import "github.com/shopspring/decimal"

// MostStrategy is a Go port of the widely-known TradingView "MOST" (Moving Stop Loss Trailing)
// indicator/strategy family originated by Anıl Özekşi (raw PineScript source not extractable via
// automated fetch; implemented from MOST's own standard, well-documented percentage-band
// construction).
//
// Standard/default parameters, per MOST's own well-known published defaults:
//   - EMA period 9 (MOST's own classic default length).
//   - Percentage offset 2% ("Percentage" input, MOST's own classic default).
//
// Algorithm: a band is built at EMA * (1 ± Percentage/100) — below the EMA in an uptrend, above it
// in a downtrend — and, like UT Bot/PMax, the band RATCHETS (a rising lower band can only rise, a
// falling upper band can only fall) rather than recomputing freely every bar, which is what gives
// it a trailing-stop character rather than a simple envelope. A close crossing through the band
// flips the trend. This is the same structural family as pmax.go/ut_bot.go but keyed off a
// percentage-of-EMA offset rather than an ATR multiple, per MOST's own published construction.
type MostStrategy struct {
	EMALen    int
	PctOffset decimal.Decimal // e.g. 0.02 for 2%

	prevMOST    decimal.Decimal
	prevTrendUp bool
	hasPrev     bool
}

func NewMostStrategy() *MostStrategy {
	return &MostStrategy{
		EMALen:    9,
		PctOffset: decimal.NewFromFloat(0.02),
	}
}

func (s *MostStrategy) Name() string { return "most_strategy" }

func (s *MostStrategy) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "ema_len", Default: decimal.NewFromInt(int64(s.EMALen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(200)},
		{Name: "pct_offset", Default: s.PctOffset, Min: decimal.NewFromFloat(0.002), Max: decimal.NewFromFloat(0.2)},
	}
}

func (s *MostStrategy) resetState() {
	s.prevMOST, s.prevTrendUp, s.hasPrev = decimal.Zero, false, false
}

func (s *MostStrategy) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["ema_len"]; ok {
		cp.EMALen = int(ClampParam(spec["ema_len"], v).IntPart())
	}
	if v, ok := values["pct_offset"]; ok {
		cp.PctOffset = ClampParam(spec["pct_offset"], v)
	}
	cp.resetState()
	return &cp
}

func (s *MostStrategy) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.EMALen+2 {
		return Signal{Side: Hold}, nil
	}
	ema, err := EMA(candles, s.EMALen)
	if err != nil {
		return Signal{}, err
	}
	close := candles[len(candles)-1].Close
	offset := ema.Mul(s.PctOffset)
	lowerBand := ema.Sub(offset)
	upperBand := ema.Add(offset)

	if !s.hasPrev {
		// Seed as uptrend with the lower band, matching UT Bot/PMax's own first-bar convention.
		s.prevMOST = lowerBand
		s.prevTrendUp = true
		s.hasPrev = true
		return Signal{Side: Hold}, nil
	}

	var most decimal.Decimal
	trendUp := s.prevTrendUp
	if trendUp {
		most = decimal.Max(s.prevMOST, lowerBand)
		if close.LessThan(most) {
			trendUp = false
			most = upperBand
		}
	} else {
		most = decimal.Min(s.prevMOST, upperBand)
		if close.GreaterThan(most) {
			trendUp = true
			most = lowerBand
		}
	}

	flippedUp := !s.prevTrendUp && trendUp
	flippedDown := s.prevTrendUp && !trendUp
	s.prevMOST, s.prevTrendUp = most, trendUp

	switch {
	case flippedUp:
		risk := close.Sub(most)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    close,
			SLPx:       most,
			TPPx:       close.Add(risk.Mul(decimal.NewFromFloat(1.5))),
			SLPct:      risk.Div(close),
			TPPct:      risk.Div(close).Mul(decimal.NewFromFloat(1.5)),
		}, nil
	case flippedDown:
		risk := most.Sub(close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    close,
			SLPx:       most,
			TPPx:       close.Sub(risk.Mul(decimal.NewFromFloat(1.5))),
			SLPct:      risk.Div(close),
			TPPct:      risk.Div(close).Mul(decimal.NewFromFloat(1.5)),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
