package domain

import (
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestRoundPriceToTick_RealInstruments(t *testing.T) {
	cases := []struct{ tick, in, want string }{
		{"0.01", "100.41424", "100.41"},                         // SOL — the price that was rejected
		{"0.01", "2417.217721831957720594", "2417.22"},          // ETH
		{"0.00001", "0.087038955068923203516", "0.08704"},       // DOGE
		{"0.000000001", "0.0000035276488262721", "0.000003528"}, // PEPE — a 1e-9 tick keeps 9 decimals
		{"0", "100.41424", "100.41424"},                         // no tick reported: unchanged
	}
	for _, c := range cases {
		got := Instrument{TickSz: d(c.tick)}.RoundPriceToTick(d(c.in))
		if !got.Equal(d(c.want)) {
			t.Errorf("tick %s: %s -> got %v, want %s", c.tick, c.in, got, c.want)
		}
	}
}
