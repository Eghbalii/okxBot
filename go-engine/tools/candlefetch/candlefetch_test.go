package candlefetch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/gateway"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/shopspring/decimal"
)

// fakeRangeReader is a hand-rolled port.RangeReader for tests, matching this project's own
// established "fake, not mock" testing convention (CLAUDE.md §17's own precedent).
type fakeRangeReader struct {
	ranges map[string]struct{ oldest, newest time.Time } // key: exchange/instID/bar
}

func (f *fakeRangeReader) CandleRange(ctx context.Context, exchange, instID, bar string) (time.Time, time.Time, error) {
	r, ok := f.ranges[key(exchange, instID, bar)]
	if !ok {
		return time.Time{}, time.Time{}, nil
	}
	return r.oldest, r.newest, nil
}

func key(exchange, instID, bar string) string { return exchange + "/" + instID + "/" + bar }

func fiveMinBarDuration(bar string) (time.Duration, bool) {
	switch bar {
	case "5m":
		return 5 * time.Minute, true
	case "15m":
		return 15 * time.Minute, true
	case "1H":
		return time.Hour, true
	}
	return 0, false
}

func TestResolveJobs_AnchorsBackwardFromEarliestStoredCandle(t *testing.T) {
	oldest := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	repo := &fakeRangeReader{ranges: map[string]struct{ oldest, newest time.Time }{
		key("mexc", "BTC", "5m"): {oldest: oldest, newest: oldest.Add(time.Hour)},
	}}
	target := Target{
		Exchange: "mexc", InstIDs: []string{"BTC"}, ExecInstIDs: map[string]string{"BTC": "BTC_USDT"},
		Bars:        []string{"5m"},
		CandlesBack: map[string]int{"5m": 50},
	}
	jobs, skipped, err := ResolveJobs(context.Background(), repo, fiveMinBarDuration, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("expected no skips, got %v", skipped)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].ExecInstID != "BTC_USDT" {
		t.Errorf("ExecInstID = %q, want %q", jobs[0].ExecInstID, "BTC_USDT")
	}
	wantStart := oldest.Add(-50 * 5 * time.Minute)
	if !jobs[0].Start.Equal(wantStart) {
		t.Errorf("start = %s, want %s", jobs[0].Start, wantStart)
	}
	if !jobs[0].End.Equal(oldest) {
		t.Errorf("end = %s, want %s (the existing earliest candle)", jobs[0].End, oldest)
	}
}

// TestResolveJobs_SkipsSymbolWithNoExecInstID reproduces the exact failure found running this tool
// against the real MEXC API for the first time: sending the bare short symbol ("BTC") instead of
// the exchange's wire-format id ("BTC_USDT") fails every request with "Contract does not exist".
// ResolveJobs must catch this before any exchange call is ever made, not after.
func TestResolveJobs_SkipsSymbolWithNoExecInstID(t *testing.T) {
	oldest := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	repo := &fakeRangeReader{ranges: map[string]struct{ oldest, newest time.Time }{
		key("mexc", "UNMAPPED", "5m"): {oldest: oldest, newest: oldest},
	}}
	target := Target{
		Exchange: "mexc", InstIDs: []string{"UNMAPPED"}, ExecInstIDs: map[string]string{}, // deliberately empty
		Bars:        []string{"5m"},
		CandlesBack: map[string]int{"5m": 50},
	}
	jobs, skipped, err := ResolveJobs(context.Background(), repo, fiveMinBarDuration, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs for a symbol with no exec instID, got %d", len(jobs))
	}
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skip reported, got %d: %v", len(skipped), skipped)
	}
}

// TestResolveJobs_PerBarOverrideAppliesIndependently reproduces the operator's own first request
// exactly: 50 candles back on most bars, 20 back specifically on 15m, in the SAME run.
func TestResolveJobs_PerBarOverrideAppliesIndependently(t *testing.T) {
	oldest5m := time.Date(2026, 9, 22, 14, 25, 0, 0, time.UTC)
	oldest15m := time.Date(2026, 9, 22, 14, 15, 0, 0, time.UTC)
	repo := &fakeRangeReader{ranges: map[string]struct{ oldest, newest time.Time }{
		key("mexc", "BTC", "5m"):  {oldest: oldest5m, newest: oldest5m},
		key("mexc", "BTC", "15m"): {oldest: oldest15m, newest: oldest15m},
	}}
	target := Target{
		Exchange: "mexc", InstIDs: []string{"BTC"}, ExecInstIDs: map[string]string{"BTC": "BTC_USDT"},
		Bars:        []string{"5m", "15m"},
		CandlesBack: map[string]int{"5m": 50, "15m": 20},
	}
	jobs, _, err := ResolveJobs(context.Background(), repo, fiveMinBarDuration, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
	byBar := map[string]Job{}
	for _, j := range jobs {
		byBar[j.Bar] = j
	}
	if want := oldest5m.Add(-50 * 5 * time.Minute); !byBar["5m"].Start.Equal(want) {
		t.Errorf("5m start = %s, want %s", byBar["5m"].Start, want)
	}
	if want := oldest15m.Add(-20 * 15 * time.Minute); !byBar["15m"].Start.Equal(want) {
		t.Errorf("15m start = %s, want %s", byBar["15m"].Start, want)
	}
}

func TestResolveJobs_SkipsInstrumentWithNoExistingCandle(t *testing.T) {
	repo := &fakeRangeReader{ranges: map[string]struct{ oldest, newest time.Time }{}}
	target := Target{
		Exchange: "mexc", InstIDs: []string{"BRAND_NEW"}, ExecInstIDs: map[string]string{"BRAND_NEW": "BRAND_NEW_USDT"},
		Bars:        []string{"5m"},
		CandlesBack: map[string]int{"5m": 50},
	}
	jobs, skipped, err := ResolveJobs(context.Background(), repo, fiveMinBarDuration, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs (nothing to anchor from), got %d", len(jobs))
	}
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skip reported, got %d: %v", len(skipped), skipped)
	}
}

func TestResolveJobs_BarWithNoCandlesBackEntryIsOmitted(t *testing.T) {
	oldest := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	repo := &fakeRangeReader{ranges: map[string]struct{ oldest, newest time.Time }{
		key("mexc", "BTC", "5m"):  {oldest: oldest, newest: oldest},
		key("mexc", "BTC", "15m"): {oldest: oldest, newest: oldest},
	}}
	target := Target{
		Exchange: "mexc", InstIDs: []string{"BTC"}, ExecInstIDs: map[string]string{"BTC": "BTC_USDT"},
		Bars:        []string{"5m", "15m"},
		CandlesBack: map[string]int{"5m": 50}, // 15m deliberately absent
	}
	jobs, skipped, err := ResolveJobs(context.Background(), repo, fiveMinBarDuration, target)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Bar != "5m" {
		t.Fatalf("expected exactly one 5m job, got %+v", jobs)
	}
	if len(skipped) != 0 {
		t.Fatalf("a bar with no CandlesBack entry is an intentional omission, not a skip: got %v", skipped)
	}
}

func TestResolveJobs_UnsupportedBarErrors(t *testing.T) {
	oldest := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	repo := &fakeRangeReader{ranges: map[string]struct{ oldest, newest time.Time }{
		key("mexc", "BTC", "1W"): {oldest: oldest, newest: oldest},
	}}
	target := Target{
		Exchange: "mexc", InstIDs: []string{"BTC"}, ExecInstIDs: map[string]string{"BTC": "BTC_USDT"},
		Bars:        []string{"1W"},
		CandlesBack: map[string]int{"1W": 10},
	}
	_, _, err := ResolveJobs(context.Background(), repo, fiveMinBarDuration, target)
	if err == nil {
		t.Fatal("expected an error for a bar the duration function does not know")
	}
}

// fakeFetcher hands back synthetic candles for a window, splitting into PAGES so pagination is
// actually exercised rather than assumed — a fetcher that always returns everything in one call
// would never prove fetchAndSaveOne's cursor-advance logic does anything.
type fakeFetcher struct {
	mu         sync.Mutex
	calls      int
	pageSize   int
	err        error
	errAfter   int // fail on call number errAfter (1-indexed); 0 = never fail
	granularity time.Duration
}

func (f *fakeFetcher) GetCandlesRange(instID, bar string, start, end time.Time) ([]domain.Candle, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()

	if f.errAfter > 0 && call == f.errAfter {
		return nil, f.err
	}

	gran := f.granularity
	if gran <= 0 {
		gran = time.Minute
	}
	// Grid-align the starting point, the same way a real exchange's candle timestamps are always
	// bar-aligned rather than falling wherever a caller's cursor happens to land — this matters
	// because fetchAndSaveOne advances its cursor to "last saved timestamp + 1ns" between pages, and
	// a fixture that echoed that non-aligned instant back as a new candle's timestamp would generate
	// a spurious extra candle every page boundary instead of correctly rounding forward to the next
	// real grid point.
	t := start.Truncate(gran)
	if t.Before(start) {
		t = t.Add(gran)
	}
	var out []domain.Candle
	for t.Before(end) && len(out) < f.pageSize {
		out = append(out, domain.Candle{
			Timestamp: t,
			Open:      decimal.NewFromInt(1), High: decimal.NewFromInt(1),
			Low: decimal.NewFromInt(1), Close: decimal.NewFromInt(1), Volume: decimal.NewFromInt(1),
		})
		t = t.Add(gran)
	}
	return out, nil
}

// fakeWriter is an in-memory port.Repository.SaveCandle stand-in.
type fakeWriter struct {
	mu    sync.Mutex
	saved []port.Candle
	err   error
}

func (w *fakeWriter) SaveCandle(ctx context.Context, c port.Candle) error {
	if w.err != nil {
		return w.err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.saved = append(w.saved, c)
	return nil
}

func (w *fakeWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.saved)
}

func drain(ch <-chan Progress) []Progress {
	var out []Progress
	for p := range ch {
		out = append(out, p)
	}
	return out
}

func TestFetch_PagesUntilWindowExhausted(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(100 * time.Minute) // 100 one-minute candles total
	job := Job{InstID: "BTC", Bar: "5m", Start: start, End: end}

	fetcher := &fakeFetcher{pageSize: 30, granularity: time.Minute} // forces >=4 pages
	writer := &fakeWriter{}

	progress := drain(Fetch(context.Background(), fetcher, writer, "mexc", []Job{job}, Options{
		Concurrency: 1,
		Limits:      &gateway.ClassLimit{Capacity: 1000, Refill: 1000, Interval: time.Second},
		PageDelay:   time.Millisecond,
	}))

	if writer.count() != 100 {
		t.Fatalf("expected 100 candles saved across pages, got %d", writer.count())
	}
	if fetcher.calls < 4 {
		t.Fatalf("expected pagination to require multiple calls (pageSize=30 over 100 candles), got %d calls", fetcher.calls)
	}
	if len(progress) != 1 {
		t.Fatalf("expected exactly 1 progress snapshot for 1 job, got %d", len(progress))
	}
	if progress[0].CandlesSaved != 100 {
		t.Errorf("progress.CandlesSaved = %d, want 100", progress[0].CandlesSaved)
	}
	if progress[0].Percent() != 100 {
		t.Errorf("progress.Percent() = %v, want 100", progress[0].Percent())
	}
}

func TestFetch_OneJobFailureDoesNotAbandonOthers(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	jobs := []Job{
		{InstID: "BAD", Bar: "5m", Start: start, End: end},
		{InstID: "GOOD", Bar: "5m", Start: start, End: end},
	}
	fetcher := &fakeFetcher{pageSize: 20, granularity: time.Minute, errAfter: 1, err: errors.New("exchange 500")}
	writer := &fakeWriter{}

	progress := drain(Fetch(context.Background(), fetcher, writer, "mexc", jobs, Options{
		Concurrency: 1, // deterministic order: BAD (job[0]) fails on call 1, GOOD succeeds after
		Limits:      &gateway.ClassLimit{Capacity: 1000, Refill: 1000, Interval: time.Second},
		PageDelay:   time.Millisecond,
	}))

	if len(progress) != 2 {
		t.Fatalf("expected 2 progress snapshots, got %d", len(progress))
	}
	last := progress[len(progress)-1]
	if last.Errors != 1 {
		t.Errorf("expected exactly 1 recorded error, got %d", last.Errors)
	}
	if last.Completed != 2 {
		t.Errorf("a failed job must still count as completed (no longer pending), got Completed=%d", last.Completed)
	}
	if writer.count() == 0 {
		t.Error("the GOOD job's candles must still be saved despite BAD's failure")
	}
}

func TestFetch_EmptyJobListClosesChannelImmediately(t *testing.T) {
	fetcher := &fakeFetcher{}
	writer := &fakeWriter{}
	progress := drain(Fetch(context.Background(), fetcher, writer, "mexc", nil, Options{}))
	if len(progress) != 0 {
		t.Fatalf("expected no progress events for an empty job list, got %d", len(progress))
	}
}

// TestFetch_RespectsRateLimit is the property this whole package exists for: a tight rate limit
// must make the run take at least as long as the limiter mandates, proving requests are genuinely
// throttled rather than firing as fast as goroutines can schedule them.
func TestFetch_RespectsRateLimit(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// 5 jobs, each needing exactly 1 page (pageSize covers the whole window in one call).
	var jobs []Job
	for i := 0; i < 5; i++ {
		jobs = append(jobs, Job{
			InstID: fmt.Sprintf("T%d", i), Bar: "5m",
			Start: start, End: start.Add(time.Minute),
		})
	}
	fetcher := &fakeFetcher{pageSize: 10, granularity: time.Minute}
	writer := &fakeWriter{}

	// Capacity 1, refilling 1 token every 40ms — 5 jobs therefore cannot all complete faster than
	// roughly 4*40ms = 160ms (first is free, the other 4 each wait one refill).
	limit := &gateway.ClassLimit{Capacity: 1, Refill: 1, Interval: 40 * time.Millisecond}

	begin := time.Now()
	drain(Fetch(context.Background(), fetcher, writer, "mexc", jobs, Options{
		Concurrency: 5, // high concurrency — the limiter, not goroutine scheduling, must be the bottleneck
		Limits:      limit,
		PageDelay:   0,
	}))
	elapsed := time.Since(begin)

	const wantMin = 150 * time.Millisecond // a little under the ideal 160ms to absorb scheduling jitter
	if elapsed < wantMin {
		t.Errorf("5 jobs at 1 token/40ms completed in %s, want >= %s — the rate limiter did not throttle requests", elapsed, wantMin)
	}
}

func TestFetch_SaveFailureIsReportedAsJobError(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	job := Job{InstID: "BTC", Bar: "5m", Start: start, End: start.Add(5 * time.Minute)}
	fetcher := &fakeFetcher{pageSize: 10, granularity: time.Minute}
	writer := &fakeWriter{err: errors.New("db unavailable")}

	progress := drain(Fetch(context.Background(), fetcher, writer, "mexc", []Job{job}, Options{
		Concurrency: 1,
		Limits:      &gateway.ClassLimit{Capacity: 100, Refill: 100, Interval: time.Second},
		PageDelay:   time.Millisecond,
	}))
	if len(progress) != 1 {
		t.Fatalf("expected 1 progress snapshot, got %d", len(progress))
	}
	if progress[0].Errors != 1 {
		t.Errorf("a save failure must be reported as a job error, got Errors=%d", progress[0].Errors)
	}
	if progress[0].LastErr == nil {
		t.Error("expected LastErr to be set on the failing snapshot")
	}
}

func TestProgress_PercentIsZeroWhenTotalIsZero(t *testing.T) {
	p := Progress{Completed: 0, Total: 0}
	if p.Percent() != 0 {
		t.Errorf("Percent() with Total=0 = %v, want 0", p.Percent())
	}
}

func TestOptions_ApplyDefaultsFillsUnsetFields(t *testing.T) {
	var o Options
	o.applyDefaults()
	if o.Concurrency <= 0 {
		t.Error("expected a positive default Concurrency")
	}
	if o.Limits == nil {
		t.Error("expected a default Limits")
	}
	if o.PageDelay <= 0 {
		t.Error("expected a positive default PageDelay")
	}
	if o.consumer == "" {
		t.Error("expected a non-empty default consumer identity")
	}
}
