package strategy

import "github.com/shopspring/decimal"

// Regime describes what kind of market the last N candles look like.
//
// WHY THIS EXISTS. All 36 strategies in the registry share one structural property: they trade
// whenever their pattern appears, with no notion of whether the market is a place that pattern can
// work. Screening them over 68,113 trades produced one statistical cloud — no strategy's gap from a
// coin flip survived its own sample size — which is what "the entry logic carries almost no
// information" looks like in aggregate.
//
// A pattern's edge is usually conditional, not absolute: a breakout is a real signal in a trending,
// expanding market and noise in a chopping one, and a mean-reversion entry is the reverse. A
// strategy that cannot tell them apart trades both and collects the average, which is what the
// screening measured.
//
// This is a shared vocabulary rather than a strategy, so the same classification can gate an
// existing kind (via RegimeFilter) and feed a new one.
type Regime struct {
	// Trending reports whether price is making directional progress rather than oscillating:
	// the net move over the window as a fraction of the total distance travelled. A straight line
	// scores 1.0; a round trip back to the start scores 0.
	Efficiency decimal.Decimal
	// Expanding reports whether ranges are widening — recent true range against its own longer
	// average. Above 1 means volatility is rising, which is when breakouts tend to follow through.
	Expansion decimal.Decimal
	// Direction is +1 up, -1 down, 0 flat, from the window's net move.
	Direction int
	// VolumeSurge is current volume against its average. A move on no participation is the classic
	// false breakout.
	VolumeSurge decimal.Decimal
}

// ClassifyRegime measures the market over the last `window` candles.
//
// Efficiency is the Kaufman efficiency ratio: |net move| / sum(|bar moves|). It is deliberately
// scale-free and needs no threshold calibration per instrument, which matters here because one
// shared classification has to work on BTC and PEPE alike — the same reason the observation feeds
// ratios rather than dollars (§15.6).
func ClassifyRegime(candles []Candle, window int) (Regime, error) {
	if window < 2 {
		window = 2
	}
	if len(candles) < window+1 {
		return Regime{}, errNeedMore(window+1, len(candles))
	}
	tail := candles[len(candles)-window:]

	net := tail[len(tail)-1].Close.Sub(tail[0].Close)
	path := decimal.Zero
	for i := 1; i < len(tail); i++ {
		path = path.Add(tail[i].Close.Sub(tail[i-1].Close).Abs())
	}

	r := Regime{}
	if path.IsPositive() {
		r.Efficiency = net.Abs().Div(path)
	}
	switch {
	case net.IsPositive():
		r.Direction = 1
	case net.IsNegative():
		r.Direction = -1
	}

	// Expansion compares the recent half's average true range against the whole window's. A ratio
	// rather than an absolute ATR, for the same scale-free reason as above.
	half := window / 2
	if half >= 1 {
		recent, err1 := ATR(candles, half)
		baseline, err2 := ATR(candles, window)
		if err1 == nil && err2 == nil && baseline.IsPositive() {
			r.Expansion = recent.Div(baseline)
		}
	}

	if avg, err := AvgVolume(candles, window); err == nil && avg.IsPositive() {
		r.VolumeSurge = tail[len(tail)-1].Volume.Div(avg)
	}
	return r, nil
}

// RegimeFilter wraps any strategy and suppresses its signals outside the regime it asks for.
//
// A WRAPPER rather than a change to each strategy, deliberately. The 36 existing kinds are
// unchanged and stay independently comparable (§11.3's locked-origin rule), and the same filter can
// be applied to any of them — so "does this pattern only work in a trend?" becomes a measurable
// question for every strategy at once rather than 36 separate edits.
type RegimeFilter struct {
	Inner  Strategy
	Window int

	// MinEfficiency suppresses the signal in a chopping market. Zero disables the check.
	MinEfficiency decimal.Decimal
	// MaxEfficiency suppresses it in a strong trend — what a mean-reversion strategy wants.
	MaxEfficiency decimal.Decimal
	// MinExpansion requires volatility to be rising.
	MinExpansion decimal.Decimal
	// MinVolumeSurge requires participation behind the move.
	MinVolumeSurge decimal.Decimal
	// WithTrend requires the signal's side to match the window's direction; AgainstTrend requires
	// the opposite. Both false means direction is not checked.
	WithTrend    bool
	AgainstTrend bool
}

func (f *RegimeFilter) Name() string { return "regime(" + f.Inner.Name() + ")" }

func (f *RegimeFilter) Evaluate(candles []Candle) (Signal, error) {
	sig, err := EvaluateWith(f.Inner, MarketView{Candles: candles})
	if err != nil || sig.Side == Hold {
		return sig, err
	}

	window := f.Window
	if window <= 0 {
		window = 20
	}
	reg, err := ClassifyRegime(candles, window)
	if err != nil {
		// Not enough history to classify. Suppressing is the honest answer: the filter exists to
		// say when a signal is worth taking, and "I cannot tell" is not "yes".
		return Signal{Side: Hold}, nil
	}

	if f.MinEfficiency.IsPositive() && reg.Efficiency.LessThan(f.MinEfficiency) {
		return Signal{Side: Hold}, nil
	}
	if f.MaxEfficiency.IsPositive() && reg.Efficiency.GreaterThan(f.MaxEfficiency) {
		return Signal{Side: Hold}, nil
	}
	if f.MinExpansion.IsPositive() && reg.Expansion.LessThan(f.MinExpansion) {
		return Signal{Side: Hold}, nil
	}
	if f.MinVolumeSurge.IsPositive() && reg.VolumeSurge.LessThan(f.MinVolumeSurge) {
		return Signal{Side: Hold}, nil
	}

	if f.WithTrend || f.AgainstTrend {
		want := 1
		if sig.Side == Sell {
			want = -1
		}
		if f.WithTrend && reg.Direction != want {
			return Signal{Side: Hold}, nil
		}
		if f.AgainstTrend && reg.Direction == want {
			return Signal{Side: Hold}, nil
		}
	}
	return sig, nil
}

func (f *RegimeFilter) Params() []ParamSpec {
	return append(f.Inner.Params(),
		ParamSpec{Name: "regime_window", Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(100), Default: decimal.NewFromInt(20)},
		ParamSpec{Name: "min_efficiency", Min: decimal.Zero, Max: decimal.NewFromFloat(0.9), Default: decimal.Zero},
		ParamSpec{Name: "max_efficiency", Min: decimal.Zero, Max: decimal.NewFromFloat(0.9), Default: decimal.Zero},
		ParamSpec{Name: "min_expansion", Min: decimal.Zero, Max: decimal.NewFromFloat(3), Default: decimal.Zero},
		ParamSpec{Name: "min_volume_surge", Min: decimal.Zero, Max: decimal.NewFromFloat(5), Default: decimal.Zero},
	)
}

func (f *RegimeFilter) WithParams(p map[string]decimal.Decimal) Strategy {
	out := &RegimeFilter{
		Inner:          f.Inner.WithParams(p),
		Window:         f.Window,
		MinEfficiency:  f.MinEfficiency,
		MaxEfficiency:  f.MaxEfficiency,
		MinExpansion:   f.MinExpansion,
		MinVolumeSurge: f.MinVolumeSurge,
		WithTrend:      f.WithTrend,
		AgainstTrend:   f.AgainstTrend,
	}
	if v, ok := p["regime_window"]; ok {
		out.Window = int(v.IntPart())
	}
	if v, ok := p["min_efficiency"]; ok {
		out.MinEfficiency = v
	}
	if v, ok := p["max_efficiency"]; ok {
		out.MaxEfficiency = v
	}
	if v, ok := p["min_expansion"]; ok {
		out.MinExpansion = v
	}
	if v, ok := p["min_volume_surge"]; ok {
		out.MinVolumeSurge = v
	}
	return out
}
