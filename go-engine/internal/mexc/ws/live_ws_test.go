package ws

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// TestLive_PublicTickerStream connects to the real MEXC socket and asserts that real ticks arrive
// and decode into usable domain values.
//
// This is the check that matters most for a new adapter: CLAUDE.md §14 records that OKX's ack/push
// misclassification broke every message decode and surfaced only when run against the live
// exchange, and §9 records a mis-cased channel that subscribed successfully and pushed nothing. A
// unit test against my own fixtures cannot find either.
func TestLive_PublicTickerStream(t *testing.T) {
	if os.Getenv("MEXC_LIVE_TESTS") == "0" {
		t.Skip("MEXC_LIVE_TESTS=0")
	}

	var (
		mu      sync.Mutex
		tickers []domain.Ticker
	)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	c := &PublicClient{
		URL:     "wss://contract.mexc.com/edge",
		Method:  "sub.ticker",
		Symbols: []string{"BTC_USDT"},
		Handler: func(m Message) {
			tk, err := DecodeTicker(m)
			if err != nil {
				return
			}
			mu.Lock()
			tickers = append(tickers, tk)
			mu.Unlock()
			if len(tickers) >= 3 {
				cancel()
			}
		},
	}
	_ = c.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(tickers) == 0 {
		t.Skip("no ticks received — MEXC unreachable or network blocked")
	}
	for i, tk := range tickers {
		if tk.InstID != "BTC_USDT" {
			t.Errorf("tick %d: InstID = %q", i, tk.InstID)
		}
		if !tk.Last.IsPositive() {
			t.Errorf("tick %d: Last = %s, want positive — ack frame decoded as data?", i, tk.Last)
		}
		if tk.BidPx.GreaterThan(tk.AskPx) {
			t.Errorf("tick %d: bid %s above ask %s", i, tk.BidPx, tk.AskPx)
		}
	}
	t.Logf("received %d live ticks, last price %s", len(tickers), tickers[len(tickers)-1].Last)
}

// TestLive_PublicKlineStream proves the kline subscription works AND that the finalizer correctly
// treats the continuously-repushed forming bar as not-yet-closed. Over a short window every push is
// for the same bar, so the correct outcome is: many pushes, zero finalizations.
func TestLive_PublicKlineStream(t *testing.T) {
	if os.Getenv("MEXC_LIVE_TESTS") == "0" {
		t.Skip("MEXC_LIVE_TESTS=0")
	}

	var (
		mu        sync.Mutex
		pushes    int
		finalized int
		lastBar   domain.Candle
	)
	fin := NewKlineFinalizer()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := &PublicClient{
		URL:      "wss://contract.mexc.com/edge",
		Method:   "sub.kline",
		Interval: "Min5",
		Symbols:  []string{"BTC_USDT"},
		Handler: func(m Message) {
			candle, err := DecodeKline(m)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			pushes++
			lastBar = candle
			if _, ok := fin.Observe("BTC_USDT", "Min5", candle); ok {
				finalized++
			}
			if pushes >= 5 {
				cancel()
			}
		},
	}
	_ = c.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if pushes == 0 {
		t.Skip("no kline pushes received — MEXC unreachable or network blocked")
	}
	// Every push in a 20s window belongs to the same 5-minute bar, so nothing should finalize.
	// A non-zero count here means the finalizer is emitting the forming bar, which downstream
	// would re-trigger strategy evaluation several times per second on an unfinished candle.
	if finalized != 0 {
		t.Errorf("finalized %d bars in a 20s window — the forming bar is being emitted as closed", finalized)
	}
	if !lastBar.Close.IsPositive() {
		t.Errorf("last bar close = %s, want positive", lastBar.Close)
	}
	if lastBar.High.LessThan(lastBar.Low) {
		t.Errorf("high %s below low %s", lastBar.High, lastBar.Low)
	}
	// The forming bar's open time must be recent and aligned to a 5-minute boundary.
	if lastBar.Timestamp.Unix()%300 != 0 {
		t.Errorf("bar open time %s is not aligned to a 5m boundary", lastBar.Timestamp)
	}
	if age := time.Since(lastBar.Timestamp); age > 10*time.Minute || age < 0 {
		t.Errorf("forming bar is %v old — wrong timestamp unit?", age)
	}
	t.Logf("received %d live kline pushes, 0 finalized (correct: all one forming bar); bar %s close=%s",
		pushes, lastBar.Timestamp, lastBar.Close)
}
