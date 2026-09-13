package okx

import (
	"encoding/json"
	"testing"
)

// TestMarketTicker_EmptyPriceFieldsDecodeAsZero pins the bug that made this type necessary
// (2026-09-13): OKX returns "" for every price field of an instrument that has never traded, and
// because /market/tickers is decoded as one array, that single row failed the decode for ALL 207
// FUTURES instruments — the instType real trading executes against. The whole market scan returned
// nothing, from one dead instrument.
//
// The bytes below are the real TEST002-USD_UM_XPERP-310822 row from the live response, not a
// fixture written from my own assumptions about what OKX might send.
func TestMarketTicker_EmptyPriceFieldsDecodeAsZero(t *testing.T) {
	raw := []byte(`[
		{"instType":"FUTURES","instId":"TEST002-USD_UM_XPERP-310822","last":"","open24h":"","high24h":"","low24h":"","volCcy24h":"0","ts":"1789313498071"},
		{"instType":"FUTURES","instId":"BTC-USD_UM_XPERP-310404","last":"77053.4","open24h":"77500","high24h":"78000","low24h":"76900","volCcy24h":"899.8877","ts":"1789313498071"}
	]`)

	var got []MarketTicker
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("an empty price field must not fail the whole array's decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("decoded %d tickers, want 2", len(got))
	}

	dead := got[0].ToDomain()
	if !dead.Last.IsZero() || !dead.Vol24hUSD.IsZero() || !dead.Change24hPct.IsZero() {
		t.Errorf("never-traded instrument: want all zeros, got last=%s vol=%s change=%s",
			dead.Last, dead.Vol24hUSD, dead.Change24hPct)
	}

	// The real row alongside it must survive intact — the point is tolerance for the dead row, not
	// tolerance that quietly degrades the live ones.
	live := got[1].ToDomain()
	if live.Last.String() != "77053.4" {
		t.Errorf("Last = %s, want 77053.4", live.Last)
	}
	// 899.8877 base units x 77053.4 = the USD notional. Asserting the multiplication happened at
	// all: the un-multiplied value would be 899.8877, three orders of magnitude out.
	if want := "69339406.90318"; live.Vol24hUSD.String() != want {
		t.Errorf("Vol24hUSD = %s, want %s (volCcy24h x last)", live.Vol24hUSD, want)
	}
	// (77053.4 - 77500) / 77500 x 100
	if got := live.Change24hPct.Round(4).String(); got != "-0.5763" {
		t.Errorf("Change24hPct = %s, want -0.5763", got)
	}
}

// TestLooseDecimal_AcceptsNullAndNumber covers the two remaining shapes OKX has been seen to use for
// a numeric field, so the type is not narrowly fitted to the one case that prompted it.
func TestLooseDecimal_AcceptsNullAndNumber(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"null", `null`, "0"},
		{"empty string", `""`, "0"},
		{"quoted number", `"12.5"`, "12.5"},
		{"bare number", `12.5`, "12.5"},
	} {
		var d LooseDecimal
		if err := json.Unmarshal([]byte(tc.in), &d); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if d.String() != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, d.String(), tc.want)
		}
	}
}
