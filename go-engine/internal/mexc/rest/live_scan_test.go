package rest

import (
	"testing"

	"github.com/shopspring/decimal"
)

// TestLive_GetAllTickers verifies the discovery endpoint against MEXC's real API (2026-09-13).
//
// The two fields this asserts on are the two that were chosen over more obvious-sounding
// neighbours, and both would fail silently if wrong: amount24 (dollars) over volume24 (a contract
// count), and riseFallRate over a change derived from an open price MEXC does not report. A wrong
// field here yields plausible-looking numbers in the wrong unit, which ranks the whole market by
// noise rather than erroring.
func TestLive_GetAllTickers(t *testing.T) {
	c := liveClient(t)
	toks, err := c.GetAllTickers("")
	if err != nil {
		t.Skipf("MEXC unreachable: %v", err)
	}
	if len(toks) < 100 {
		t.Fatalf("got %d tickers — MEXC lists over a thousand futures contracts, so this is a decode failure, not a quiet market", len(toks))
	}

	var btc *decimal.Decimal
	var withVol, withChange int
	for i := range toks {
		k := toks[i]
		if k.Vol24hUSD.IsPositive() {
			withVol++
		}
		if !k.Change24hPct.IsZero() {
			withChange++
		}
		if k.High24h.IsPositive() && k.Low24h.IsPositive() && k.High24h.LessThan(k.Low24h) {
			t.Errorf("%s: high %s below low %s", k.InstID, k.High24h, k.Low24h)
		}
		if k.InstID == "BTC_USDT" {
			v := k.Vol24hUSD
			btc = &v
		}
	}
	if withVol == 0 {
		t.Error("no ticker carried a positive USD volume — the volume field is wrong")
	}
	if withChange == 0 {
		t.Error("no ticker carried a nonzero 24h change — the change field is wrong")
	}

	// BTC's 24h notional on MEXC runs in the billions. A contract COUNT read as dollars would land
	// orders of magnitude off, so a plausible-magnitude check here is what actually distinguishes
	// amount24 from volume24 — the specific confusion this test exists for.
	if btc == nil {
		t.Fatal("BTC_USDT missing from the all-tickers response")
	}
	if btc.LessThan(decimal.NewFromInt(10_000_000)) {
		t.Errorf("BTC_USDT 24h volume = %s USD — implausibly small, suggests volume24 (contracts) is being read instead of amount24 (dollars)", btc)
	}
	t.Logf("%d tickers, %d with volume, %d with change; BTC 24h = %s USD", len(toks), withVol, withChange, btc)
}
