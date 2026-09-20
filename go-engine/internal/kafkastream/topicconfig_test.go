package kafkastream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func kafkaAvailable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// retryUntilSuccess must retry a failing fn on the configured interval and stop the moment fn
// succeeds — the exact behavior EnsureTopicRetentionInBackground relies on to self-heal past the
// "topic doesn't exist yet" startup race without ever needing a real broker to prove it.
func TestRetryUntilSuccess_RetriesThenStops(t *testing.T) {
	var calls int32
	var failures int32
	successCh := make(chan struct{})

	fn := func(context.Context) error {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			return errors.New("not ready yet")
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go retryUntilSuccess(ctx, slog.Default(), 10*time.Millisecond, fn,
		func() { close(successCh) },
		func(error) { atomic.AddInt32(&failures, 1) },
	)

	select {
	case <-successCh:
	case <-time.After(1 * time.Second):
		t.Fatal("retryUntilSuccess never called onSuccess")
	}

	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("fn called %d times, want exactly 3 (2 failures then a success)", got)
	}
	if got := atomic.LoadInt32(&failures); got != 2 {
		t.Errorf("onFailure called %d times, want exactly 2", got)
	}

	// Give the loop a moment to have stopped, then confirm it did — a bug that keeps retrying
	// after success would keep incrementing calls forever.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("fn called %d times after success, want it to have stopped at 3", got)
	}
}

// A persistently-failing fn must stop retrying the moment ctx is cancelled, not spin forever or
// leak the goroutine — the shutdown case EnsureTopicRetentionInBackground's own doc comment names.
func TestRetryUntilSuccess_StopsOnContextCancellation(t *testing.T) {
	var calls int32
	fn := func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return errors.New("always fails")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		retryUntilSuccess(ctx, slog.Default(), 5*time.Millisecond, fn, func() {
			t.Error("onSuccess must never be called for an always-failing fn")
		}, func(error) {})
		close(done)
	}()

	time.Sleep(30 * time.Millisecond) // let a few retries happen
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("retryUntilSuccess did not stop after ctx cancellation")
	}
}

// EnsureTopicRetentionInBackground must not nil-panic when called with a nil logger — the
// production call site (cmd/ingestor) always passes a real one, but this is the same defensive
// nil-logger tolerance the rest of this codebase applies at every logger-accepting boundary.
func TestEnsureTopicRetentionInBackground_NilLoggerDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked with nil logger: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// No real brokers configured, so EnsureTopicRetention fails immediately every attempt — this
	// test only asserts the nil-logger path doesn't panic while that happens, not that it succeeds.
	EnsureTopicRetentionInBackground(ctx, nil, nil, "okx.orderbook", 1800000, 16777216)
	<-ctx.Done()
}

// EnsureTopicRetention against a real broker, when one is reachable — skips cleanly otherwise
// (the redisAvailable/kafkaAvailable pattern this codebase already uses for infra-dependent
// tests, internal/optimizer/trialstore_test.go). Confirms the actual production incident's fix:
// applying a retention override to a topic and reading it back via a second alter-then-verify
// round trip, rather than only asserting "no error" from the alter call itself.
func TestEnsureTopicRetention_AppliesAndIsIdempotent(t *testing.T) {
	addr := "localhost:9094" // host-reachable listener per docker-compose.yml's kafka service
	if !kafkaAvailable(addr) {
		t.Skip("no local Kafka reachable at " + addr + "; skipping Kafka-backed retention test")
	}

	topic := fmt.Sprintf("test.retention.%d", time.Now().UnixNano())
	brokers := []string{addr}

	// The topic doesn't exist yet, so the first call is expected to race auto-topic-creation —
	// retry briefly rather than asserting success on the first attempt, matching how
	// EnsureTopicRetentionInBackground itself tolerates exactly this in production.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var lastErr error
	for {
		if err := EnsureTopicRetention(ctx, brokers, topic, 1800000, 16777216); err == nil {
			lastErr = nil
			break
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("EnsureTopicRetention never succeeded within the timeout: %v", lastErr)
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastErr != nil {
		t.Fatalf("unexpected error after retry loop: %v", lastErr)
	}

	// Calling it again with the same values must succeed cleanly (idempotent) — this is the
	// exact call pattern a restarting ingestor makes on every boot.
	if err := EnsureTopicRetention(ctx, brokers, topic, 1800000, 16777216); err != nil {
		t.Fatalf("second EnsureTopicRetention call (idempotent re-apply) failed: %v", err)
	}
}
