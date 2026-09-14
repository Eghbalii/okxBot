package strategy

import (
	"testing"

	"github.com/shopspring/decimal"
)

// The three TradingView ports must FIRE, and their filters must actually gate.
//
// Both halves are needed. §30.1 records a suite passing vacuously because every strategy returned
// Hold on a degenerate fixture, and confluence was caught firing zero times in 470 bars because its
// members never agreed on the same candle — an idea that would have been recorded as a failure
// having never run. A filter that never blocks is equally useless in the other direction: it looks
// like a gate and is a pass-through.

func TestTrendShift_Fires(t *testing.T) {
	// The fixture's ADX peaks around 21, below the default 25 entry threshold — measured, not
	// assumed: with suppression on it produces zero signals while the band flips 79 times, because
	// the regime gate is doing exactly its job on a market that never trends by ADX's measure.
	//
	// So the firing test lowers the threshold to something this fixture can reach. Loosening a
	// threshold to make a test pass is usually how a test stops meaning anything; here the property
	// under test is "the Supertrend flip produces a signal", and the gate has its own test below.
	ts := NewTrendShift()
	ts.EnterTrend = decimal.NewFromInt(15)
	ts.ExitTrend = decimal.NewFromInt(10)
	if b, s := driveBars(ts, timedSeries(600, 150), nil); b+s == 0 {
		t.Error("trendshift never fired even with a reachable trend threshold")
	}
}

// The regime gate must actually suppress. With the entry threshold set beyond any reachable ADX the
// strategy can never declare a trend, so SuppressInChop must silence it entirely.
func TestTrendShift_RegimeGateSuppresses(t *testing.T) {
	base := NewTrendShift()
	base.EnterTrend = decimal.NewFromInt(15)
	base.ExitTrend = decimal.NewFromInt(10)
	baseB, baseS := driveBars(base, timedSeries(600, 150), nil)
	if baseB+baseS == 0 {
		t.Skip("no signals to suppress on this fixture")
	}

	gated := NewTrendShift()
	gated.EnterTrend = decimal.NewFromInt(200) // ADX is bounded by 100; unreachable by construction
	gated.ExitTrend = decimal.NewFromInt(150)
	if b, s := driveBars(gated, timedSeries(600, 150), nil); b+s != 0 {
		t.Errorf("fired %d times with an unreachable trend threshold — the regime gate is not gating", b+s)
	}
}

// With suppression OFF the same unreachable threshold must NOT silence it: that separates "the gate
// is working" from "the strategy broke".
func TestTrendShift_SuppressionIsWhatSilencesIt(t *testing.T) {
	g := NewTrendShift()
	g.EnterTrend = decimal.NewFromInt(200)
	g.ExitTrend = decimal.NewFromInt(150)
	g.SuppressInChop = false
	if b, s := driveBars(g, timedSeries(600, 150), nil); b+s == 0 {
		t.Error("with suppression off the strategy must still trade; it is silent for another reason")
	}
}

// Hysteresis: the exit threshold must sit below the entry one, or the regime state can never settle.
// WithParams repairs a degenerate pair rather than rejecting it.
func TestTrendShift_RepairsInvertedHysteresis(t *testing.T) {
	out := NewTrendShift().WithParams(map[string]decimal.Decimal{
		"enter_trend": decimal.NewFromInt(20),
		"exit_trend":  decimal.NewFromInt(30), // above enter — degenerate
	}).(*TrendShift)
	if !out.ExitTrend.LessThan(out.EnterTrend) {
		t.Errorf("exit %s must be repaired below enter %s", out.ExitTrend, out.EnterTrend)
	}
}

func TestSweepReverse_Fires(t *testing.T) {
	// Filters off isolates the pattern itself: if it cannot find a sweep at all, no filter setting
	// will help and the failure is in the detection rather than the gating.
	s := NewSweepReverse()
	s.RequireVolume, s.RequireWick, s.RequireNextBar = false, false, false
	if b, sl := driveBars(s, timedSeries(800, 150), nil); b+sl == 0 {
		t.Error("sweep_reverse never detected a sweep with all filters disabled")
	}
}

// Each filter must reduce signal count on its own, or it is not filtering.
func TestSweepReverse_FiltersReduceSignals(t *testing.T) {
	series := timedSeries(800, 150)

	open := NewSweepReverse()
	open.RequireVolume, open.RequireWick, open.RequireNextBar = false, false, false
	baseB, baseS := driveBars(open, series, nil)
	if baseB+baseS == 0 {
		t.Skip("no sweeps detected on this fixture")
	}

	strict := NewSweepReverse()
	strict.RequireVolume, strict.RequireWick, strict.RequireNextBar = true, true, true
	strict.MinVolumeRatio = decimal.NewFromInt(10) // unreachable
	if b, s := driveBars(strict, series, nil); b+s >= baseB+baseS {
		t.Errorf("filters did not reduce signals: %d filtered vs %d unfiltered", b+s, baseB+baseS)
	}
}

func TestGradientRibbon_Fires(t *testing.T) {
	if b, s := driveBars(NewGradientRibbon(), timedSeries(800, 150), nil); b+s == 0 {
		t.Error("gradient_ribbon never fired; a fan never opened in 800 bars")
	}
}

// The fan threshold must gate: an unreachable spread requirement silences it.
func TestGradientRibbon_FanThresholdGates(t *testing.T) {
	g := NewGradientRibbon()
	g.MinFanSpread = decimal.NewFromInt(1) // a slope spread of 100% of price is impossible
	if b, s := driveBars(g, timedSeries(800, 150), nil); b+s != 0 {
		t.Errorf("fired %d times with an impossible fan threshold", b+s)
	}
}

// An inverted RSI band can never be satisfied and would silence the strategy permanently — repaired
// rather than rejected, the same call rsi_sma_fuzzy makes for a degenerate zone.
func TestGradientRibbon_RepairsInvertedRSIBand(t *testing.T) {
	out := NewGradientRibbon().WithParams(map[string]decimal.Decimal{
		"min_rsi": decimal.NewFromInt(70),
		"max_rsi": decimal.NewFromInt(60),
	}).(*GradientRibbon)
	if !out.MaxRSI.GreaterThan(out.MinRSI) {
		t.Errorf("max %s must be repaired above min %s", out.MaxRSI, out.MinRSI)
	}
}

// Every signal any of the three emits must be a coherent trade: stop on the losing side of entry,
// target on the winning side. §16.9 records a real position opened with a target past entry, which
// realizes a loss under the name take-profit.
func TestTVPorts_EmitCoherentLevels(t *testing.T) {
	series := timedSeries(800, 150)
	// Configured so each one can actually reach a signal on this fixture — the coherence property
	// is about the levels a signal carries, and a strategy that emits none checks nothing. The
	// gating behaviour has its own tests above.
	ts := NewTrendShift()
	ts.EnterTrend, ts.ExitTrend = decimal.NewFromInt(15), decimal.NewFromInt(10)
	sr := NewSweepReverse()
	sr.RequireVolume, sr.RequireWick, sr.RequireNextBar = false, false, false

	for _, s := range []Strategy{ts, sr, NewGradientRibbon()} {
		checked := 0
		for i := 120; i < len(series); i++ {
			sig, err := s.Evaluate(series[:i])
			if err != nil || sig.Side == Hold {
				continue
			}
			if !sig.EntryPx.IsPositive() || !sig.SLPx.IsPositive() || !sig.TPPx.IsPositive() {
				continue // percentage-only levels are legitimate; nothing to check here
			}
			checked++
			if sig.Side == Buy {
				if !sig.SLPx.LessThan(sig.EntryPx) || !sig.TPPx.GreaterThan(sig.EntryPx) {
					t.Fatalf("%s long: entry %s sl %s tp %s", s.Name(), sig.EntryPx, sig.SLPx, sig.TPPx)
				}
			} else {
				if !sig.SLPx.GreaterThan(sig.EntryPx) || !sig.TPPx.LessThan(sig.EntryPx) {
					t.Fatalf("%s short: entry %s sl %s tp %s", s.Name(), sig.EntryPx, sig.SLPx, sig.TPPx)
				}
			}
		}
		if checked == 0 {
			t.Errorf("%s emitted no priced signal — the check passed vacuously", s.Name())
		}
	}
}
