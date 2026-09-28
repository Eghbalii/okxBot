package kafkastream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// fakeReader is a hand-rolled kafkaReader that can inject a scripted sequence of FetchMessage
// outcomes — this is what lets Run's retry behavior be tested without a real Kafka broker or the
// timing games needed to reliably provoke a transient failure against one.
type fakeReader struct {
	fetchResults []fetchResult // consumed in order; the last is repeated once exhausted
	fetchCalls   int32
	commitCalls  int32
	commitErr    error // if set (and commitFailTimes is 0), EVERY CommitMessages call fails with this
	// commitFailTimes, if > 0, fails exactly the first N CommitMessages calls with commitErr (or a
	// default error if commitErr is unset), then succeeds — for testing "retries then recovers"
	// rather than "retries forever".
	commitFailTimes int32
}

type fetchResult struct {
	msg kafka.Message
	err error
}

func (f *fakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	// Honour cancellation the way a real reader does — without this, a test whose Run loop is
	// bounded by a context deadline spins on the repeated last message instead of returning.
	if err := ctx.Err(); err != nil {
		return kafka.Message{}, err
	}
	i := int(atomic.AddInt32(&f.fetchCalls, 1)) - 1
	if i >= len(f.fetchResults) {
		i = len(f.fetchResults) - 1
	}
	r := f.fetchResults[i]
	return r.msg, r.err
}

func (f *fakeReader) CommitMessages(ctx context.Context, msgs ...kafka.Message) error {
	n := atomic.AddInt32(&f.commitCalls, 1)
	if f.commitFailTimes > 0 {
		if n <= f.commitFailTimes {
			return errors.New("simulated transient commit failure")
		}
		return nil
	}
	return f.commitErr
}

func (f *fakeReader) Close() error { return nil }

func TestClassifyRunError_StopsOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := classifyRunError(ctx, errors.New("some transient error")); got != outcomeStop {
		t.Fatalf("expected outcomeStop when ctx is done, got %v", got)
	}
}

func TestClassifyRunError_StopsOnIOEOF(t *testing.T) {
	if got := classifyRunError(context.Background(), io.EOF); got != outcomeStop {
		t.Fatalf("expected outcomeStop on io.EOF (reader explicitly closed), got %v", got)
	}
}

func TestClassifyRunError_StopsOnWrappedIOEOF(t *testing.T) {
	wrapped := fmt.Errorf("fetch message: %w", io.EOF)
	if got := classifyRunError(context.Background(), wrapped); got != outcomeStop {
		t.Fatalf("expected outcomeStop on a wrapped io.EOF, got %v", got)
	}
}

func TestClassifyRunError_RetriesOnTransientError(t *testing.T) {
	got := classifyRunError(context.Background(), errors.New("connection reset by peer"))
	if got != outcomeRetry {
		t.Fatalf("expected outcomeRetry on a plain transient error, got %v", got)
	}
}

func TestNextBackoff_DoublesUpToCap(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{16 * time.Second, 30 * time.Second},     // would double to 32s, capped to 30s
		{consumerMaxBackoff, consumerMaxBackoff}, // already at cap, stays there
	}
	for _, c := range cases {
		if got := nextBackoff(c.in); got != c.want {
			t.Errorf("nextBackoff(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSleepOrDone_ReturnsTrueAfterDuration(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	if !sleepOrDone(ctx, 10*time.Millisecond) {
		t.Fatal("expected true (waited the full duration)")
	}
	if time.Since(start) < 5*time.Millisecond {
		t.Fatal("returned too early")
	}
}

func TestSleepOrDone_ReturnsFalseOnContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepOrDone(ctx, time.Hour) {
		t.Fatal("expected false when ctx is already done")
	}
}

// TestRun_RetriesTransientFetchErrorThenSucceeds is the actual regression test for the bug this
// commit fixes: before it, ANY fetch error (not just io.EOF/ctx-done) made Run return immediately,
// which is what left the panel's live-price consumer goroutine permanently dead after Kafka's own
// restart. This proves Run instead retries and eventually delivers the message once fetches start
// succeeding again.
func TestRun_RetriesTransientFetchErrorThenSucceeds(t *testing.T) {
	reader := &fakeReader{
		fetchResults: []fetchResult{
			{err: errors.New("connection refused")},
			{err: errors.New("connection refused")},
			{msg: kafka.Message{Value: []byte(`{"instId":"BTC-USDT-SWAP"}`)}},
		},
	}
	c := &Consumer{reader: reader, Topic: "okx.tickers", Group: "test", initialBackoff: time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	handled := make(chan []byte, 1)
	go func() {
		_ = c.Run(ctx, func(_ context.Context, data []byte) error {
			handled <- data
			return nil
		})
	}()

	select {
	case data := <-handled:
		if string(data) != `{"instId":"BTC-USDT-SWAP"}` {
			t.Fatalf("unexpected message delivered: %s", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler was never called — Run did not retry past the transient fetch errors")
	}

	if got := atomic.LoadInt32(&reader.fetchCalls); got < 3 {
		t.Fatalf("expected at least 3 FetchMessage calls (2 failures + 1 success), got %d", got)
	}
}

// TestRun_RetriesTransientCommitErrorThenSucceeds mirrors the fetch-error test for the second
// failure point Run has: CommitMessages, which had the identical "any error kills the loop" bug.
// Uses a reader that fails the first two commits then succeeds, rather than failing forever —
// CommitMessages retries THIS message's commit in its own inner loop and deliberately never calls
// FetchMessage again until it succeeds (real kafka-go's FetchMessage always returns the NEXT
// message regardless of whether the previous one committed, so retrying via a fresh fetch would
// silently abandon the failed commit rather than actually retrying it).
func TestRun_RetriesTransientCommitErrorThenSucceeds(t *testing.T) {
	// blockingFakeReader delivers exactly one message and then blocks forever on any further
	// FetchMessage call (until ctx is cancelled) — this pins Run inside the commit-retry loop for
	// that ONE message, so the test can observe "exactly N commit calls, exactly 1 fetch, handler
	// called exactly once" without racing Run looping around to a second message the moment the
	// first commit succeeds (which fakeReader's "repeat forever" behavior would otherwise allow).
	// The assertion runs INSIDE onCommitSuccess, synchronously, before Run's own goroutine gets
	// control back and can loop around to a second FetchMessage — any check done from the test
	// goroutine after merely observing "commit succeeded" would race that next loop iteration,
	// since Run resumes immediately once CommitMessages returns.
	type snapshot struct{ fetchCalls, handledAtCommit int32 }
	result := make(chan snapshot, 1)
	var handledCount int32
	reader := &blockingFakeReader{
		msg:             kafka.Message{Value: []byte("hello")},
		commitFailTimes: 2,
	}
	reader.onCommitSuccess = func() {
		result <- snapshot{
			fetchCalls:      atomic.LoadInt32(&reader.fetchCalls),
			handledAtCommit: atomic.LoadInt32(&handledCount),
		}
	}
	c := &Consumer{reader: reader, Topic: "okx.tickers", Group: "test", initialBackoff: time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = c.Run(ctx, func(_ context.Context, data []byte) error {
			atomic.AddInt32(&handledCount, 1)
			return nil
		})
	}()

	select {
	case snap := <-result:
		if snap.fetchCalls != 1 {
			t.Fatalf("expected exactly 1 FetchMessage call at the moment the commit succeeded (commit retries must not re-fetch), got %d", snap.fetchCalls)
		}
		if snap.handledAtCommit != 1 {
			t.Fatalf("expected handler called exactly once by the time the commit succeeded, got %d", snap.handledAtCommit)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("commit never succeeded")
	}
}

// blockingFakeReader delivers its one message on the first FetchMessage call, then blocks every
// subsequent call until ctx is done — so a test exercising the commit-retry path for that one
// message never has to race Run looping around to fetch (and handle) a second one.
type blockingFakeReader struct {
	msg             kafka.Message
	fetchCalls      int32
	commitCalls     int32
	commitFailTimes int32
	onCommitSuccess func() // called exactly once, right after the commit that finally succeeds
}

func (f *blockingFakeReader) FetchMessage(ctx context.Context) (kafka.Message, error) {
	if atomic.AddInt32(&f.fetchCalls, 1) == 1 {
		return f.msg, nil
	}
	<-ctx.Done()
	return kafka.Message{}, ctx.Err()
}

func (f *blockingFakeReader) CommitMessages(ctx context.Context, msgs ...kafka.Message) error {
	n := atomic.AddInt32(&f.commitCalls, 1)
	if n <= f.commitFailTimes {
		return errors.New("simulated transient commit failure")
	}
	if n == f.commitFailTimes+1 && f.onCommitSuccess != nil {
		f.onCommitSuccess()
	}
	return nil
}

func (f *blockingFakeReader) Close() error { return nil }

// TestRun_StopsOnContextCancellation confirms Run still returns promptly when ctx is cancelled
// mid-retry-loop, rather than the retry logic accidentally making it un-cancellable.
func TestRun_StopsOnContextCancellation(t *testing.T) {
	reader := &fakeReader{
		fetchResults: []fetchResult{{err: errors.New("connection refused")}},
	}
	c := &Consumer{reader: reader, Topic: "okx.tickers", Group: "test", initialBackoff: time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, func(context.Context, []byte) error { return nil }) }()

	time.Sleep(5 * time.Millisecond) // let it enter the retry backoff at least once
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop within 2s of context cancellation")
	}
}

// TestRun_StopsOnReaderClosed confirms Run treats io.EOF from FetchMessage (kafka-go's signal
// that Close() was called) as a real stop, not a transient error to retry forever.
func TestRun_StopsOnReaderClosed(t *testing.T) {
	reader := &fakeReader{
		fetchResults: []fetchResult{{err: io.EOF}},
	}
	c := &Consumer{reader: reader, Topic: "okx.tickers", Group: "test"}

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background(), func(context.Context, []byte) error { return nil }) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a non-nil error when the reader reports io.EOF")
		}
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected the returned error to wrap io.EOF, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not stop on io.EOF — it retried a closed reader forever")
	}
}

// Real order 3 (SOL short, 2026-09-08) was closed as a take-profit at 102 — a price the market had
// traded at three hours earlier and nowhere near since (its session low was 103.86). Cause: a
// consumer replaying the retained backlog fed hours-old ticks into live decision code, which read
// them as the current price. Market data's whole value is being current, so an old tick is wrong
// data, not late data.
func TestRun_DropsStaleMessagesWithoutCallingHandler(t *testing.T) {
	fresh := kafka.Message{Value: []byte(`{"instId":"SOL","last":"103.90"}`), Time: time.Now()}
	stale := kafka.Message{Value: []byte(`{"instId":"SOL","last":"102.00"}`), Time: time.Now().Add(-3 * time.Hour)}

	r := &fakeReader{fetchResults: []fetchResult{{msg: stale}, {msg: fresh}}}
	c := &Consumer{reader: r, Topic: "okx.tickers", Group: "trader", MaxMessageAge: 2 * time.Minute}

	// The fake repeats its last message once exhausted, so bound the run by time rather than by
	// cancelling from inside the handler: the point under test is which messages reach the handler
	// at all, and the stale one is fetched first.
	var mu sync.Mutex
	var seen [][]byte
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = c.Run(ctx, func(_ context.Context, v []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			seen = append(seen, v)
		}
		return nil
	})
	mu.Lock()
	defer mu.Unlock()

	if len(seen) != 1 {
		t.Fatalf("expected only the fresh message to reach the handler, got %d: %s", len(seen), seen)
	}
	if string(seen[0]) != string(fresh.Value) {
		t.Fatalf("wrong message delivered: %s", seen[0])
	}
	// The stale message must still be COMMITTED, or the reader would stall on a backlog forever
	// instead of draining through it.
	if got := atomic.LoadInt32(&r.commitCalls); got < 2 {
		t.Fatalf("expected the stale message to be committed too (skipped, not stalled on), got %d commits", got)
	}
}

func TestIsStale(t *testing.T) {
	c := &Consumer{MaxMessageAge: time.Minute}
	if c.isStale(time.Now()) {
		t.Error("a just-published message must not be stale")
	}
	if !c.isStale(time.Now().Add(-2 * time.Minute)) {
		t.Error("a 2-minute-old message must be stale at a 1-minute bound")
	}
	// A broker that stamps no time must never stall the pipeline.
	if c.isStale(time.Time{}) {
		t.Error("an unstamped message must not be treated as stale")
	}
	if (&Consumer{}).isStale(time.Now().Add(-999 * time.Hour)) {
		t.Error("a zero MaxMessageAge disables the check entirely")
	}
}

// A fresh consumer group must start at the END of the topic, not replay the retained backlog.
func TestNewConsumer_StartsAtLastOffset(t *testing.T) {
	c := NewConsumer([]string{"localhost:9092"}, "okx.candles.5m", "trader")
	if got := c.reader.(*kafka.Reader).Config().StartOffset; got != kafka.LastOffset {
		t.Fatalf("StartOffset: want LastOffset (%d) so a new group does not replay history, got %d", kafka.LastOffset, got)
	}
	if c.MaxMessageAge != defaultMaxMessageAge {
		t.Fatalf("MaxMessageAge: want the default %v for a non-ticker topic, got %v", defaultMaxMessageAge, c.MaxMessageAge)
	}
}

func TestNewConsumer_TickerTopicsGetTheShorterStalenessWindow(t *testing.T) {
	for _, topic := range []string{"okx.tickers", "mexc.tickers"} {
		c := NewConsumer([]string{"localhost:9092"}, topic, "some-group")
		if c.MaxMessageAge != tickerMaxMessageAge {
			t.Fatalf("topic %s: MaxMessageAge: want %v, got %v", topic, tickerMaxMessageAge, c.MaxMessageAge)
		}
	}
}

func TestNewConsumer_NonTickerTopicsKeepTheDefaultStalenessWindow(t *testing.T) {
	for _, topic := range []string{"okx.candles.1H", "okx.paper-order-events", "okx.orderbook"} {
		c := NewConsumer([]string{"localhost:9092"}, topic, "some-group")
		if c.MaxMessageAge != defaultMaxMessageAge {
			t.Fatalf("topic %s: MaxMessageAge: want the default %v, got %v", topic, defaultMaxMessageAge, c.MaxMessageAge)
		}
	}
}

func TestNewConsumer_BatchesCommits(t *testing.T) {
	c := NewConsumer([]string{"localhost:9092"}, "okx.tickers", "trader")
	if got := c.reader.(*kafka.Reader).Config().CommitInterval; got != time.Second {
		t.Fatalf("CommitInterval: want 1s so a backlog drain does not pay one broker round-trip per message, got %v", got)
	}
}
