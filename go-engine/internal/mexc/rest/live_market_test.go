package rest

import (
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// These tests hit MEXC's real public API. They need no credentials (every endpoint here is
// unauthenticated) but they do need network, so they skip cleanly when it is unavailable or when
// MEXC_LIVE_TESTS is explicitly disabled — the same posture as this project's Redis-backed
// optimizer tests (CLAUDE.md §16.11).
//
// They exist because the alternative is testing the adapter against fixtures I wrote from the same
// assumptions the adapter encodes, which proves the two agree and nothing else. Every wire format
// here — column-array klines, seconds-vs-milliseconds timestamps, numeric-not-string prices — was
// discovered by calling the real API, and these keep that honest.
func liveClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("MEXC_LIVE_TESTS") == "0" {
		t.Skip("MEXC_LIVE_TESTS=0")
	}
	return New("https://contract.mexc.com", "", "")
}

func TestLive_GetTicker(t *testing.T) {
	c := liveClient(t)
	tk, err := c.GetTicker("BTC_USDT")
	if err != nil {
		t.Skipf("MEXC unreachable: %v", err)
	}
	if tk.InstID != "BTC_USDT" {
		t.Errorf("InstID = %q, want BTC_USDT", tk.InstID)
	}
	// A real BTC price is a large positive number. Asserting a plausible RANGE rather than
	// exact equality: this is a live price, so the only honest assertion is that the conversion
	// produced something real rather than a zero from a silently-failed parse.
	if !tk.Last.IsPositive() {
		t.Fatalf("Last = %s, want a positive price", tk.Last)
	}
	if tk.Last.LessThan(decimal.NewFromInt(1000)) {
		t.Errorf("Last = %s — implausible for BTC, suggests a parse or field-name error", tk.Last)
	}
	if !tk.BidPx.IsPositive() || !tk.AskPx.IsPositive() {
		t.Errorf("bid/ask = %s/%s, want both positive", tk.BidPx, tk.AskPx)
	}
	if tk.BidPx.GreaterThan(tk.AskPx) {
		t.Errorf("bid %s above ask %s — fields are crossed", tk.BidPx, tk.AskPx)
	}
	t.Logf("live BTC_USDT: last=%s bid=%s ask=%s", tk.Last, tk.BidPx, tk.AskPx)
}

// TestLive_GetCandles is the important one: it proves the column-array decode works against the
// real response, that candles come back oldest-first, and that the SECONDS timestamp was read as
// seconds. A milliseconds misread would place these candles ~56,000 years in the future.
func TestLive_GetCandles(t *testing.T) {
	c := liveClient(t)
	candles, err := c.GetCandles("BTC_USDT", "5m", 20)
	if err != nil {
		t.Skipf("MEXC unreachable: %v", err)
	}
	if len(candles) == 0 {
		t.Fatal("no candles returned")
	}
	if len(candles) > 20 {
		t.Errorf("got %d candles, want at most the requested 20", len(candles))
	}

	for i, c := range candles {
		if !c.Open.IsPositive() || !c.High.IsPositive() || !c.Low.IsPositive() || !c.Close.IsPositive() {
			t.Fatalf("candle %d has a non-positive price: %+v", i, c)
		}
		// OHLC coherence catches a column mix-up, which is exactly the failure the parallel-array
		// format makes possible and which price-positivity alone would not notice.
		if c.High.LessThan(c.Low) {
			t.Errorf("candle %d: high %s below low %s — columns are crossed", i, c.High, c.Low)
		}
		if c.Open.GreaterThan(c.High) || c.Open.LessThan(c.Low) {
			t.Errorf("candle %d: open %s outside [%s, %s]", i, c.Open, c.Low, c.High)
		}
		if c.Close.GreaterThan(c.High) || c.Close.LessThan(c.Low) {
			t.Errorf("candle %d: close %s outside [%s, %s]", i, c.Close, c.Low, c.High)
		}
	}

	// Oldest-first, matching the port's contract and the OKX adapter.
	for i := 1; i < len(candles); i++ {
		if !candles[i].Timestamp.After(candles[i-1].Timestamp) {
			t.Fatalf("candles not oldest-first: [%d]=%s then [%d]=%s",
				i-1, candles[i-1].Timestamp, i, candles[i].Timestamp)
		}
	}

	// The timestamp must be recent. This is what catches a seconds/milliseconds confusion, which
	// no amount of price validation would reveal.
	newest := candles[len(candles)-1].Timestamp
	if age := time.Since(newest); age > 2*time.Hour || age < -time.Hour {
		t.Errorf("newest candle timestamp %s is %v from now — seconds/milliseconds confusion?", newest, age)
	}

	// And consecutive 5m candles must actually be 5 minutes apart.
	if len(candles) >= 2 {
		gap := candles[1].Timestamp.Sub(candles[0].Timestamp)
		if gap != 5*time.Minute {
			t.Errorf("gap between 5m candles = %v, want 5m", gap)
		}
	}
	t.Logf("live candles: %d bars, newest %s close=%s", len(candles), newest, candles[len(candles)-1].Close)
}

// TestLive_GetInstrument guards the contract-shape values that order sizing depends on. CLAUDE.md
// §33.3 records that assuming a multiplier of 1 would have sized real orders ~10,000x too large.
func TestLive_GetInstrument(t *testing.T) {
	c := liveClient(t)
	inst, err := c.GetInstrument("", "BTC_USDT")
	if err != nil {
		t.Skipf("MEXC unreachable: %v", err)
	}
	if inst.InstID != "BTC_USDT" {
		t.Errorf("InstID = %q", inst.InstID)
	}
	if !inst.CtVal.IsPositive() {
		t.Fatalf("CtVal = %s — order sizing divides by this", inst.CtVal)
	}
	if !inst.LotSz.IsPositive() {
		t.Errorf("LotSz = %s, want positive", inst.LotSz)
	}
	if !inst.TickSz.IsPositive() {
		t.Errorf("TickSz = %s — price rounding needs this (§38)", inst.TickSz)
	}
	if inst.CtValCcy != "BTC" {
		t.Errorf("CtValCcy = %q, want BTC", inst.CtValCcy)
	}
	t.Logf("live BTC_USDT contract: ctVal=%s lotSz=%s minSz=%s tickSz=%s",
		inst.CtVal, inst.LotSz, inst.MinSz, inst.TickSz)
}

func TestLive_GetFundingRateHistory(t *testing.T) {
	c := liveClient(t)
	rates, err := c.GetFundingRateHistory("BTC_USDT", 5)
	if err != nil {
		t.Skipf("MEXC unreachable: %v", err)
	}
	if len(rates) == 0 {
		t.Fatal("no funding rates returned")
	}
	// Oldest-first, reversed from MEXC's own newest-first order — the port's contract.
	for i := 1; i < len(rates); i++ {
		if !rates[i].FundingTime.After(rates[i-1].FundingTime) {
			t.Fatalf("funding rates not oldest-first: [%d]=%s then [%d]=%s",
				i-1, rates[i-1].FundingTime, i, rates[i].FundingTime)
		}
	}
	// Milliseconds here (unlike klines' seconds) — a misread would be centuries off.
	if age := time.Since(rates[len(rates)-1].FundingTime); age > 48*time.Hour || age < -24*time.Hour {
		t.Errorf("newest funding time %s is %v from now — milliseconds misread?",
			rates[len(rates)-1].FundingTime, age)
	}
	t.Logf("live funding: %d periods, newest %s rate=%s",
		len(rates), rates[len(rates)-1].FundingTime, rates[len(rates)-1].FundingRate)
}
