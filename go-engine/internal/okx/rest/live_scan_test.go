package rest

import (
	"os"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// These tests hit OKX's real public market API. No credentials are needed (/market/tickers is
// unauthenticated), but they do need network and skip cleanly without it — the same posture as
// internal/mexc/rest's own live tests.
//
// The host is my.okx.com, not www.okx.com: this project's server is EEA-hosted and www rejects
// those requests with a misleading "API key doesn't exist" (CLAUDE.md §33.1/§35.6). Public
// endpoints work from either host, so this is consistency with production rather than a necessity
// here — worth not having a second convention for.
func liveOKXClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("OKX_LIVE_TESTS") == "0" {
		t.Skip("OKX_LIVE_TESTS=0")
	}
	base := os.Getenv("OKX_BASE_URL")
	if base == "" {
		base = "https://my.okx.com"
	}
	return New(base, "", "", "", false)
}

// TestLive_GetAllTickers verifies the discovery endpoint, and specifically that USD volume is
// derived from volCcy24h (base-currency units) times price rather than from vol24h.
//
// The distinction is invisible on any instrument whose contract multiplier is 1 and silently wrong
// everywhere else — live-verified on EDGE-USDT-SWAP, where the two fields read 8,559,200 and
// 855,920. A ranking built on the wrong one orders the market by contract size.
func TestLive_GetAllTickers(t *testing.T) {
	c := liveOKXClient(t)
	for _, instType := range []string{"SWAP", "FUTURES"} {
		toks, err := c.GetAllTickers(instType)
		if err != nil {
			t.Skipf("OKX unreachable: %v", err)
		}
		if len(toks) < 50 {
			t.Fatalf("%s: got %d tickers — OKX lists hundreds, so this is a decode failure rather than a quiet market", instType, len(toks))
		}

		var withVol, withChange int
		var btc *decimal.Decimal
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
			// The plain BTC perpetual on SWAP; on FUTURES it is the X-Perp product real trading
			// executes against (CLAUDE.md §33.2), whose id carries a rolling expiry — matched on the
			// _XPERP marker rather than a "BTC-USD" prefix, which also matches the eleven DATED
			// BTC futures OKX lists alongside it (live-checked). Matching loosely picked up a
			// near-dead dated contract at $1,540 and read as a volume-math bug that was not one.
			if k.InstID == "BTC-USDT-SWAP" || (instType == "FUTURES" && strings.HasPrefix(k.InstID, "BTC-USD") && strings.Contains(k.InstID, "_XPERP")) {
				v := k.Vol24hUSD
				btc = &v
			}
		}
		if withVol == 0 {
			t.Errorf("%s: no ticker carried a positive USD volume — the volume field is wrong", instType)
		}
		if withChange == 0 {
			t.Errorf("%s: no ticker carried a nonzero 24h change — open24h is not being read", instType)
		}
		if btc == nil {
			t.Errorf("%s: no BTC instrument found with positive volume", instType)
			continue
		}
		if btc.LessThan(decimal.NewFromInt(1_000_000)) {
			t.Errorf("%s: BTC 24h volume = %s USD — implausibly small, suggests volCcy24h is not being multiplied by price", instType, btc)
		}
		t.Logf("%s: %d tickers, %d with volume, %d with change; BTC 24h = %s USD", instType, len(toks), withVol, withChange, btc)
	}
}
