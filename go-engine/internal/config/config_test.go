package config

import (
	"strings"
	"testing"
)

func TestValidatePaperTradingBars(t *testing.T) {
	cases := []struct {
		name       string
		ingestion  []string
		paperTrade []string
		wantErr    bool
	}{
		{
			name:       "proper subset passes",
			ingestion:  []string{"1m", "3m", "5m", "1H", "4H", "1D"},
			paperTrade: []string{"1m"},
			wantErr:    false,
		},
		{
			name:       "equal sets pass",
			ingestion:  []string{"1m", "1H"},
			paperTrade: []string{"1m", "1H"},
			wantErr:    false,
		},
		{
			name:       "empty paper trading bars passes trivially",
			ingestion:  []string{"1m"},
			paperTrade: nil,
			wantErr:    false,
		},
		{
			name:       "bar missing from ingestion fails",
			ingestion:  []string{"1m", "1H"},
			paperTrade: []string{"1m", "15m"},
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Ingestion.Bars = tc.ingestion
			cfg.PaperTrading.Bars = tc.paperTrade

			err := cfg.ValidatePaperTradingBars()
			if tc.wantErr && err == nil {
				t.Errorf("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
		})
	}
}

// TestValidateBarNames_RejectsWrongCasing guards a silent-failure mode: OKX's WS channel names are
// case-sensitive ("candle1H", not "candle1h"), so a mis-cased bar subscribes to a channel that
// never pushes anything — the pipeline looks healthy while that timeframe produces no candles.
// Catching it at startup turns that into an immediate, obvious error.
func TestValidateBarNames_RejectsWrongCasing(t *testing.T) {
	for _, tc := range []struct{ bar, want string }{
		{"1h", "1H"}, {"4h", "4H"}, {"1d", "1D"}, {"15M", "15m"},
	} {
		err := validateBarNames([]string{tc.bar})
		if err == nil {
			t.Errorf("%q: expected rejection, got nil", tc.bar)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: error should suggest %q, got: %v", tc.bar, tc.want, err)
		}
	}
}

func TestValidateBarNames_AcceptsOKXCasing(t *testing.T) {
	if err := validateBarNames([]string{"5m", "15m", "1H", "4H", "1D"}); err != nil {
		t.Errorf("valid OKX bars rejected: %v", err)
	}
}

func TestValidateBarNames_RejectsUnknown(t *testing.T) {
	if err := validateBarNames([]string{"7x"}); err == nil {
		t.Error("expected a nonsense bar to be rejected")
	}
}

// The tick-driven RL decision context must be a timeframe this process actually maintains a window
// for, or the observation would be built from an empty candle series.
func TestValidatePaperTradingBars_RLDecisionBarMustBeMaintained(t *testing.T) {
	c := &Config{}
	c.Ingestion.Bars = []string{"5m", "15m", "1H"}
	c.PaperTrading.Bars = []string{"5m", "15m"}
	c.PaperTrading.RLDecisionBar = "1H" // ingested, but not maintained by paper-trader

	if err := c.ValidatePaperTradingBars(); err == nil {
		t.Fatal("expected rejection of an rl_decision_bar outside paper_trading.bars")
	}

	c.PaperTrading.RLDecisionBar = "5m"
	if err := c.ValidatePaperTradingBars(); err != nil {
		t.Errorf("valid rl_decision_bar rejected: %v", err)
	}
}

// TestAllowRealMoney_DefaultsToFalse guards the paper -> demo -> real progression (CLAUDE.md
// §15.6): reaching real trading must require saying so, not merely forgetting to set
// OKX_SIMULATED_TRADING. cmd/trader refuses to start when this is false and the credentials are
// non-demo.
func TestAllowRealMoney_DefaultsToFalse(t *testing.T) {
	c := &Config{}
	if c.Trading.AllowRealMoney {
		t.Error("allow_real_money must default to false — going live should be deliberate")
	}
}
