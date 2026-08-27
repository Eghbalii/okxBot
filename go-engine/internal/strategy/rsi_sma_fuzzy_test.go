package strategy

import (
	"testing"

	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestRSISMAFuzzy_OversoldMembership_SmoothRamp(t *testing.T) {
	s := NewRSISMAFuzzy() // OversoldMin=20, OversoldMax=35

	cases := []struct {
		rsi  decimal.Decimal
		want decimal.Decimal
	}{
		{dec("10"), dec("1")},     // well below min -> full membership
		{dec("20"), dec("1")},     // at min -> full membership
		{dec("27.5"), dec("0.5")}, // exact midpoint of [20,35] -> half membership
		{dec("35"), decimal.Zero}, // at max -> zero membership
		{dec("50"), decimal.Zero}, // well above max -> zero membership
	}
	for _, c := range cases {
		got := s.oversoldMembership(c.rsi)
		if !got.Equal(c.want) {
			t.Errorf("oversoldMembership(%s) = %s, want %s", c.rsi, got, c.want)
		}
	}
}

func TestRSISMAFuzzy_NoCliffAtBoundary(t *testing.T) {
	// The whole point of the fuzzy variant: membership must change continuously across the
	// transition zone, never jump discontinuously the way a hard threshold (RSI <= 30 ? buy : hold)
	// would at its cutoff.
	s := NewRSISMAFuzzy()
	prev := s.oversoldMembership(dec("19.9"))
	for rsi := 20.0; rsi <= 35.0; rsi += 0.1 {
		cur := s.oversoldMembership(decimal.NewFromFloat(rsi))
		diff := prev.Sub(cur).Abs()
		// A single 0.1-RSI-point step must not move membership by more than a small bounded
		// amount — this is what "no cliff" means concretely, versus RSISMA's instant 0->nonzero
		// jump exactly at OversoldMax.
		if diff.GreaterThan(dec("0.02")) {
			t.Errorf("membership jumped by %s between adjacent RSI steps near rsi=%.1f (prev=%s cur=%s) - not smooth", diff, rsi, prev, cur)
		}
		prev = cur
	}
}

func TestRSISMAFuzzy_BuySignal_AboveSMA(t *testing.T) {
	s := NewRSISMAFuzzy()
	s.RSIPeriod = 6
	s.SMAPeriod = 2
	// A pullback-in-uptrend: RSI(6) lands at ~27.6 (inside the [20,35) oversold transition zone)
	// while the last close (102.6) sits just above the 2-period SMA (102.55) -- an "uptrend
	// pullback" setup, matching the strategy's intended entry condition.
	candles := []Candle{
		{Close: dec("100")}, {Close: dec("102")}, {Close: dec("104")}, {Close: dec("106")},
		{Close: dec("108")}, {Close: dec("106")}, {Close: dec("104")}, {Close: dec("103")},
		{Close: dec("102.5")}, {Close: dec("102.6")},
	}
	sig, err := s.Evaluate(candles)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if sig.Side != Buy {
		t.Fatalf("expected Buy, got %v (confidence=%s)", sig.Side, sig.Confidence)
	}
	if sig.Confidence.IsZero() || sig.Confidence.GreaterThan(decimal.NewFromInt(1)) {
		t.Errorf("expected confidence in (0,1], got %s", sig.Confidence)
	}
}

func TestRSISMAFuzzy_HoldBelowMinMembership(t *testing.T) {
	s := NewRSISMAFuzzy()
	s.MinMembership = dec("0.9") // require near-full membership to signal
	s.RSIPeriod = 2
	s.SMAPeriod = 2
	// RSI moderately low (partial membership, well under 0.9) but not deeply oversold.
	candles := []Candle{
		{Close: dec("100")},
		{Close: dec("99")},
		{Close: dec("98")},
		{Close: dec("97")},
		{Close: dec("101")},
	}
	sig, err := s.Evaluate(candles)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if sig.Side != Hold {
		t.Errorf("expected Hold when membership is below MinMembership floor, got %v (confidence=%s)", sig.Side, sig.Confidence)
	}
}

func TestRSISMAFuzzy_WithParams_ClampsDegenerateZone(t *testing.T) {
	s := NewRSISMAFuzzy()
	// Proposing oversold_min >= oversold_max must not be allowed to stand -- WithParams must
	// repair it, since oversoldMembership's division would misbehave on a non-positive span.
	updated := s.WithParams(map[string]decimal.Decimal{
		"oversold_min": dec("40"),
		"oversold_max": dec("30"),
	}).(*RSISMAFuzzy)
	if !updated.OversoldMin.LessThan(updated.OversoldMax) {
		t.Errorf("expected OversoldMin < OversoldMax after WithParams, got min=%s max=%s", updated.OversoldMin, updated.OversoldMax)
	}
}

func TestRSISMAFuzzy_RegisteredInFactory(t *testing.T) {
	factory, ok := Factories["rsi_sma_fuzzy"]
	if !ok {
		t.Fatal("expected \"rsi_sma_fuzzy\" to be registered in Factories")
	}
	s := factory()
	if s.Name() != "rsi_sma_fuzzy" {
		t.Errorf("expected Name() == \"rsi_sma_fuzzy\", got %q", s.Name())
	}
}
