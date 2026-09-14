package strategy

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The four kinds added 2026-09-14 must actually FIRE on real market data.
//
// §30.1 records a whole test suite passing vacuously: the fixture's candles had zero-height bodies,
// so every strategy returned Hold and the suite compared Hold against Hold from end to end while
// reporting PASS. A strategy that silently never signals is indistinguishable from a quiet market,
// and it is exactly what a new strategy does when one of its inputs is missing.

func timedSeries(n int, base float64) []Candle {
	out := make([]Candle, 0, n)
	px := base
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		px *= 1 + 0.004*float64((i%17)-8) + 0.0006*float64((i/40)%5-2)
		c := decimal.NewFromFloat(px)
		out = append(out, Candle{
			Timestamp: start.Add(time.Duration(i) * 5 * time.Minute),
			Open:      c.Mul(decimal.NewFromFloat(0.9985)),
			High:      c.Mul(decimal.NewFromFloat(1.004)),
			Low:       c.Mul(decimal.NewFromFloat(0.996)),
			Close:     c,
			Volume:    decimal.NewFromFloat(1000 + float64((i*37)%500)),
		})
	}
	return out
}

// driveBars feeds candles one closed bar at a time, the way the engine does, and counts signals.
// Passing a whole series in one call is not how these run and can hide a re-arming bug.
func driveBars(s Strategy, series []Candle, ref []Candle) (buys, sells int) {
	for i := 30; i < len(series); i++ {
		v := MarketView{Candles: series[:i], Bar: "5m", Bars: map[string][]Candle{"5m": series[:i]}}
		if ref != nil {
			v.Reference = ref[:i]
		}
		sig, err := EvaluateWith(s, v)
		if err != nil {
			continue
		}
		switch sig.Side {
		case Buy:
			buys++
		case Sell:
			sells++
		}
	}
	return buys, sells
}

func TestConfluence_Fires(t *testing.T) {
	buys, sells := driveBars(NewConfluence(), timedSeries(500, 150), nil)
	if buys+sells == 0 {
		t.Error("confluence never fired; a committee that never agrees is indistinguishable from a broken one")
	}
}

// Confluence must demand agreement — the entire premise. With MinAgree above the member count it
// can never fire, which is the check that the counting is real rather than the first member's
// opinion passed through.
func TestConfluence_RequiresAgreement(t *testing.T) {
	c := NewConfluence()
	c.MinAgree = len(c.Members) + 1
	if buys, sells := driveBars(c, timedSeries(500, 150), nil); buys+sells != 0 {
		t.Errorf("fired %d times with an unreachable agreement threshold — the count is not being enforced", buys+sells)
	}
}

func TestBTCDivergence_FiresWithAReference(t *testing.T) {
	own := timedSeries(500, 150)
	// A reference moving on its own schedule, so the two genuinely diverge rather than tracking.
	ref := timedSeries(500, 64000)
	for i := range ref {
		ref[i].Close = ref[i].Close.Mul(decimal.NewFromFloat(1 + 0.002*float64((i%23)-11)))
	}
	if buys, sells := driveBars(NewBTCDivergence(), own, ref); buys+sells == 0 {
		t.Error("btc_divergence never fired with a reference series")
	}
}

// Without its reference it must say NOTHING rather than fall back to a single-instrument rule —
// which would be a different strategy wearing this one's name and results.
func TestBTCDivergence_SilentWithoutReference(t *testing.T) {
	if buys, sells := driveBars(NewBTCDivergence(), timedSeries(500, 150), nil); buys+sells != 0 {
		t.Errorf("fired %d times with no reference series; it has nothing to compare against", buys+sells)
	}
}

func TestSessionMomentum_Fires(t *testing.T) {
	if buys, sells := driveBars(NewSessionMomentum(), timedSeries(900, 150), nil); buys+sells == 0 {
		t.Error("session_momentum never fired across multiple session opens")
	}
}

// Without timestamps it must be silent: this strategy's whole premise is the clock.
func TestSessionMomentum_SilentWithoutTimestamps(t *testing.T) {
	series := timedSeries(500, 150)
	for i := range series {
		series[i].Timestamp = time.Time{}
	}
	if buys, sells := driveBars(NewSessionMomentum(), series, nil); buys+sells != 0 {
		t.Errorf("fired %d times with no timestamps", buys+sells)
	}
}

// The filter must actually suppress. An unreachable threshold silences everything; without that
// property the wrapper would be a pass-through that merely looks like a filter.
func TestRegimeFilter_Suppresses(t *testing.T) {
	series := timedSeries(500, 150)
	inner := NewMACDMomentum()
	baseBuys, baseSells := driveBars(inner, series, nil)
	if baseBuys+baseSells == 0 {
		t.Skip("the inner strategy did not fire on this fixture")
	}

	// Efficiency is bounded by 1 by construction, so this can never pass.
	f := &RegimeFilter{Inner: NewMACDMomentum(), Window: 20, MinEfficiency: decimal.NewFromInt(2)}
	if buys, sells := driveBars(f, series, nil); buys+sells != 0 {
		t.Errorf("filter passed %d signals through an impossible threshold", buys+sells)
	}

	// And it must let some through when the threshold is reachable, or it is simply a mute.
	f2 := &RegimeFilter{Inner: NewMACDMomentum(), Window: 20, MinEfficiency: decimal.NewFromFloat(0.01)}
	if buys, sells := driveBars(f2, series, nil); buys+sells == 0 {
		t.Error("filter suppressed everything at a near-zero threshold; it is a mute, not a filter")
	}
}

func TestClassifyRegime_EfficiencyIsBounded(t *testing.T) {
	// A straight line is maximally efficient; an oscillation around one price is not.
	straight := make([]Candle, 40)
	px := 100.0
	for i := range straight {
		px *= 1.002
		c := decimal.NewFromFloat(px)
		straight[i] = Candle{Open: c, High: c, Low: c, Close: c, Volume: decimal.NewFromInt(100)}
	}
	r, err := ClassifyRegime(straight, 20)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if r.Efficiency.LessThan(decimal.NewFromFloat(0.99)) {
		t.Errorf("a straight line must be ~1.0 efficient, got %s", r.Efficiency)
	}
	if r.Direction != 1 {
		t.Errorf("a rising line must read direction +1, got %d", r.Direction)
	}

	chop := make([]Candle, 40)
	for i := range chop {
		v := 100.0
		if i%2 == 0 {
			v = 101.0
		}
		c := decimal.NewFromFloat(v)
		chop[i] = Candle{Open: c, High: c, Low: c, Close: c, Volume: decimal.NewFromInt(100)}
	}
	rc, err := ClassifyRegime(chop, 20)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if rc.Efficiency.GreaterThan(decimal.NewFromFloat(0.2)) {
		t.Errorf("an oscillation must be inefficient, got %s", rc.Efficiency)
	}
}
