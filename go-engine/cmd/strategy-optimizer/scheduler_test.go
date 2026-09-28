package main

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
)

// TestConductorClamps_MapsEveryField is the same regression guard §23 established for
// cmd/paper-trader's buildRLClamps (a field silently dropped from that struct literal is what
// left every real position uncapped once already) — a second, independently maintained mapping
// of the SAME config struct into conductor.Clamps, so a bug in this one can never silently mirror
// a bug in that one, and vice versa.
func TestConductorClamps_MapsEveryField(t *testing.T) {
	cfg := &config.Config{}
	cfg.PaperTrading.RLClamps.MinSLDistPct = decimal.NewFromFloat(0.005)
	cfg.PaperTrading.RLClamps.MaxSLDistPct = decimal.NewFromFloat(0.05)
	cfg.PaperTrading.RLClamps.MaxLossPct = decimal.NewFromFloat(0.15)
	cfg.PaperTrading.RLClamps.MinTPSLRatio = decimal.NewFromFloat(1.5)
	cfg.PaperTrading.RLClamps.MaxTPSLRatio = decimal.NewFromFloat(3)

	got := conductorClamps(cfg)

	if !got.MinSLDistPct.Equal(cfg.PaperTrading.RLClamps.MinSLDistPct) {
		t.Errorf("MinSLDistPct not mapped: got %s", got.MinSLDistPct)
	}
	if !got.MaxSLDistPct.Equal(cfg.PaperTrading.RLClamps.MaxSLDistPct) {
		t.Errorf("MaxSLDistPct not mapped: got %s", got.MaxSLDistPct)
	}
	if !got.MaxLossPct.Equal(cfg.PaperTrading.RLClamps.MaxLossPct) {
		t.Errorf("MaxLossPct not mapped: got %s, want %s", got.MaxLossPct, cfg.PaperTrading.RLClamps.MaxLossPct)
	}
	if !got.MinTPSLRatio.Equal(cfg.PaperTrading.RLClamps.MinTPSLRatio) {
		t.Errorf("MinTPSLRatio not mapped: got %s", got.MinTPSLRatio)
	}
	if !got.MaxTPSLRatio.Equal(cfg.PaperTrading.RLClamps.MaxTPSLRatio) {
		t.Errorf("MaxTPSLRatio not mapped: got %s", got.MaxTPSLRatio)
	}
}

func TestNormalizeLookback_ConvertsDaySuffix(t *testing.T) {
	got := normalizeLookback("30d")
	want := "720h0m0s"
	if got != want {
		t.Errorf("normalizeLookback(30d) = %q, want %q", got, want)
	}
}

func TestNormalizeLookback_PassesThroughNonDayValues(t *testing.T) {
	got := normalizeLookback("4h")
	if got != "4h" {
		t.Errorf("normalizeLookback(4h) = %q, want unchanged", got)
	}
}
