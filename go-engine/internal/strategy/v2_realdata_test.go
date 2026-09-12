package strategy

import (
	"encoding/csv"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Real OKX 5m candles, pulled from the production TimescaleDB on 2026-09-12. Three instruments
// spanning the volatility range this bot actually trades: BTC's median 5m candle is 0.081% and
// ZEC's is 0.453% — 5.6x apart, which is exactly the spread that made V1's fixed-percentage levels
// wrong on both ends (CLAUDE.md §45).
//
// Synthetic fixtures are kept for the other V2 tests because they can be shaped to contain a
// specific pattern on demand, but "does this strategy fire in the real market" can only honestly be
// answered against real candles — a strategy tuned until it fires on a fixture that was itself
// tuned until it fires is measuring nothing.
func loadRealCandles(t *testing.T, symbol string) []Candle {
	t.Helper()
	f, err := os.Open("testdata/" + symbol + "5m.csv")
	if err != nil {
		t.Fatalf("open testdata for %s: %v", symbol, err)
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read testdata for %s: %v", symbol, err)
	}
	out := make([]Candle, 0, len(rows))
	// The dump is newest-first; strategies need oldest-first.
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		sec, err := strconv.ParseInt(r[0], 10, 64)
		if err != nil {
			t.Fatalf("bad timestamp %q: %v", r[0], err)
		}
		dec := func(s string) decimal.Decimal {
			d, err := decimal.NewFromString(s)
			if err != nil {
				t.Fatalf("bad decimal %q: %v", s, err)
			}
			return d
		}
		out = append(out, Candle{
			Timestamp: time.Unix(sec, 0).UTC(),
			Open:      dec(r[1]),
			High:      dec(r[2]),
			Low:       dec(r[3]),
			Close:     dec(r[4]),
			Volume:    dec(r[5]),
		})
	}
	return out
}

// TestV2_FireOnRealMarketData is the honest version of TestV2_AllFire. Every V2 strategy must
// produce at least one signal across ~100 hours of real 5m candles on at least one of three
// instruments — a strategy that cannot is either inert (the pmax failure, CLAUDE.md §16.8:
// registered, assignable, and silently returning Hold forever) or so over-filtered that assigning
// it would waste a slot.
func TestV2_FireOnRealMarketData(t *testing.T) {
	symbols := []string{"BTC", "SOL", "ZEC"}
	markets := make(map[string][]Candle, len(symbols))
	for _, s := range symbols {
		markets[s] = loadRealCandles(t, s)
	}

	for _, kind := range v2Kinds() {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			total := 0
			for _, sym := range symbols {
				cs := markets[sym]
				sigs := driveV2(t, Factories[kind](), cs, 250)
				total += len(sigs)
				t.Logf("  %s on %s: %d signals over %d candles", kind, sym, len(sigs), len(cs))

				// Every real-market signal must also be a coherent, reachable trade — the same
				// bounds as the synthetic tests, asserted here against prices that actually traded.
				for i, sig := range sigs {
					risk := sig.EntryPx.Sub(sig.SLPx).Abs()
					reward := sig.TPPx.Sub(sig.EntryPx).Abs()
					if !risk.IsPositive() {
						t.Fatalf("%s/%s signal %d: zero risk", kind, sym, i)
					}
					if rr := reward.Div(risk); rr.LessThan(decimal.NewFromFloat(0.9999)) || rr.GreaterThan(decimal.NewFromInt(4)) {
						t.Fatalf("%s/%s signal %d: R:R %s out of the [1,4] band", kind, sym, i, rr)
					}
					if marginPct := sig.TPPct.Mul(decimal.NewFromInt(1000)); marginPct.GreaterThan(decimal.NewFromInt(30)) {
						t.Fatalf("%s/%s signal %d: target %s%% of margin at 10x — unreachable",
							kind, sym, i, marginPct.StringFixed(1))
					}
				}
			}
			if total == 0 {
				t.Errorf("%s never fired across %d real instruments — inert or over-filtered", kind, len(symbols))
			}
		})
	}
}
