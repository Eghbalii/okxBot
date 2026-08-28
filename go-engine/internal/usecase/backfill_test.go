package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// fakeHistoryFetcher serves a finite synthetic history, paging backwards the way OKX does.
type fakeHistoryFetcher struct {
	// total candles available per (instId+bar); requests past this return empty.
	available int
	pageSize  int
	calls     int
	err       error
	// stall makes every page return the same window, simulating a cursor that never advances.
	stall bool
}

func (f *fakeHistoryFetcher) GetHistoryCandles(instID, bar string, before time.Time, limit int) ([]domain.Candle, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	size := f.pageSize
	if size == 0 {
		size = 100
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := 0
	if !before.IsZero() && !f.stall {
		// Candles are one minute apart; page backwards from `before`.
		start = int(before.Sub(base).Minutes())
		start = f.available - start
		if start < 0 {
			start = 0
		}
	}

	out := make([]domain.Candle, 0, size)
	for i := 0; i < size; i++ {
		idx := start + i
		if idx >= f.available {
			break
		}
		// Newest-first within the page, matching OKX.
		ts := base.Add(time.Duration(f.available-idx) * time.Minute)
		out = append(out, domain.Candle{
			Timestamp: ts,
			Open:      decimal.NewFromInt(100),
			High:      decimal.NewFromInt(101),
			Low:       decimal.NewFromInt(99),
			Close:     decimal.NewFromInt(100),
			Volume:    decimal.NewFromInt(10),
		})
	}
	return out, nil
}

func newBackfill(f *fakeHistoryFetcher, repo port.Repository) *Backfill {
	return &Backfill{
		Exchange: f,
		Repo:     repo,
		Logger:   testLogger(),
		// No sleeping in tests: the real delay exists to stay inside OKX's rate limit, which a
		// fake has none of, and pacing here would just make the suite slow.
		PageDelay: time.Nanosecond,
	}
}

func TestBackfill_PagesUntilTargetReached(t *testing.T) {
	repo := newFakeRepository()
	f := &fakeHistoryFetcher{available: 1000, pageSize: 100}
	b := newBackfill(f, repo)

	results, err := b.Run(context.Background(), BackfillRequest{
		InstIDs: []string{"BTC-USDT-SWAP"}, Bars: []string{"1m"}, TargetCandles: 500,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Fetched < 500 {
		t.Errorf("fetched %d, want at least the 500 target", results[0].Fetched)
	}
	if results[0].Stored != results[0].Fetched {
		t.Errorf("stored %d of %d fetched; every candle should reach the repository",
			results[0].Stored, results[0].Fetched)
	}
	// A single page is 100, so reaching 500 must have taken several requests — this is what
	// distinguishes a paginating backfill from one that silently takes only the first page.
	if f.calls < 5 {
		t.Errorf("only %d requests issued; pagination did not happen", f.calls)
	}
}

func TestBackfill_StopsAtEndOfAvailableHistory(t *testing.T) {
	// A recently-listed instrument, or a long timeframe, simply has less history than requested.
	// That is a normal end condition, not an error — the run should report what it got.
	repo := newFakeRepository()
	f := &fakeHistoryFetcher{available: 120, pageSize: 100}
	b := newBackfill(f, repo)

	results, err := b.Run(context.Background(), BackfillRequest{
		InstIDs: []string{"BTC-USDT-SWAP"}, Bars: []string{"1m"}, TargetCandles: 5000,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if results[0].Error != "" {
		t.Errorf("running out of history is not an error, got %q", results[0].Error)
	}
	if results[0].Fetched == 0 {
		t.Fatal("should have fetched the available history")
	}
	if results[0].Fetched > 120 {
		t.Errorf("fetched %d, but only 120 candles exist", results[0].Fetched)
	}
}

func TestBackfill_StopsWhenCursorStalls(t *testing.T) {
	// If the endpoint keeps returning the same window, the cursor never advances. Without a guard
	// this loops forever issuing requests against a rate-limited API.
	repo := newFakeRepository()
	f := &fakeHistoryFetcher{available: 1000, pageSize: 100, stall: true}
	b := newBackfill(f, repo)

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Run(context.Background(), BackfillRequest{
			InstIDs: []string{"BTC-USDT-SWAP"}, Bars: []string{"1m"}, TargetCandles: 100000,
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill did not terminate on a stalled cursor")
	}
	if f.calls > 10 {
		t.Errorf("issued %d requests against a stalled cursor; should stop almost immediately", f.calls)
	}
}

func TestBackfill_PairErrorDoesNotAbortOtherPairs(t *testing.T) {
	// One delisted instrument or one unavailable timeframe must not discard everything else the
	// run already fetched.
	repo := newFakeRepository()
	f := &fakeHistoryFetcher{err: errors.New("instrument not found")}
	b := newBackfill(f, repo)

	results, err := b.Run(context.Background(), BackfillRequest{
		InstIDs: []string{"BAD-INST", "ALSO-BAD"}, Bars: []string{"1m"}, TargetCandles: 100,
	})
	if err != nil {
		t.Fatalf("a failing pair should not fail the whole run: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected a result per pair even on failure, got %d", len(results))
	}
	for _, r := range results {
		if r.Error == "" {
			t.Errorf("pair %s/%s should carry its own error", r.InstID, r.Bar)
		}
	}
}

func TestBackfill_IsIdempotent(t *testing.T) {
	// Re-running must overwrite identical rows rather than duplicating them — this is what makes
	// an interrupted backfill safe to simply run again.
	repo := newFakeRepository()
	f := &fakeHistoryFetcher{available: 300, pageSize: 100}
	b := newBackfill(f, repo)

	req := BackfillRequest{InstIDs: []string{"BTC-USDT-SWAP"}, Bars: []string{"1m"}, TargetCandles: 200}
	if _, err := b.Run(context.Background(), req); err != nil {
		t.Fatalf("first run: %v", err)
	}
	afterFirst, err := repo.ListCandles(context.Background(), "BTC-USDT-SWAP", "1m", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if _, err := b.Run(context.Background(), req); err != nil {
		t.Fatalf("second run: %v", err)
	}
	afterSecond, err := repo.ListCandles(context.Background(), "BTC-USDT-SWAP", "1m", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(afterSecond) != len(afterFirst) {
		t.Errorf("re-running changed the row count from %d to %d; the write is not idempotent",
			len(afterFirst), len(afterSecond))
	}
}

func TestBackfill_CoversEveryInstrumentBarPair(t *testing.T) {
	repo := newFakeRepository()
	f := &fakeHistoryFetcher{available: 500, pageSize: 100}
	b := newBackfill(f, repo)

	results, err := b.Run(context.Background(), BackfillRequest{
		InstIDs:       []string{"BTC-USDT-SWAP", "ETH-USDT-SWAP"},
		Bars:          []string{"5m", "15m", "1H"},
		TargetCandles: 100,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(results) != 6 {
		t.Fatalf("expected 2 instruments x 3 bars = 6 results, got %d", len(results))
	}
}

func TestBackfill_RejectsEmptyRequest(t *testing.T) {
	b := newBackfill(&fakeHistoryFetcher{}, newFakeRepository())
	if _, err := b.Run(context.Background(), BackfillRequest{}); err == nil {
		t.Error("a request with no instruments or bars should be rejected, not silently do nothing")
	}
}

func TestBackfill_CancellationStopsButKeepsWhatWasFetched(t *testing.T) {
	repo := newFakeRepository()
	f := &fakeHistoryFetcher{available: 10000, pageSize: 100}
	b := newBackfill(f, repo)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the run starts

	results, err := b.Run(ctx, BackfillRequest{
		InstIDs: []string{"BTC-USDT-SWAP"}, Bars: []string{"1m"}, TargetCandles: 5000,
	})
	if err == nil {
		t.Error("a cancelled run should report the cancellation")
	}
	// Results are still returned so the caller knows what landed before the stop.
	if results == nil {
		t.Error("cancellation should still return the partial results")
	}
}
