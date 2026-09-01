package gateway

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLimits(capacity, refill int, interval time.Duration) map[EndpointClass]ClassLimit {
	return map[EndpointClass]ClassLimit{
		ClassTrade: {Capacity: capacity, Refill: refill, Interval: interval},
	}
}

func TestAcquire_SucceedsImmediatelyWithinCapacity(t *testing.T) {
	l := NewLimiter(testLimits(5, 5, time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		if err := l.Acquire(ctx, ClassTrade, "paper-trader", PriorityNormal); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
}

func TestAcquire_BlocksOnceCapacityExhausted(t *testing.T) {
	l := NewLimiter(testLimits(1, 1, 50*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := l.Acquire(ctx, ClassTrade, "paper-trader", PriorityNormal); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	start := time.Now()
	if err := l.Acquire(ctx, ClassTrade, "paper-trader", PriorityNormal); err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 30*time.Millisecond {
		t.Fatalf("expected to block for refill (~50ms), only waited %v", elapsed)
	}
}

func TestAcquire_RespectsContextCancellation(t *testing.T) {
	l := NewLimiter(testLimits(1, 1, time.Hour)) // effectively never refills within the test
	ctx := context.Background()
	if err := l.Acquire(ctx, ClassTrade, "paper-trader", PriorityNormal); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := l.Acquire(shortCtx, ClassTrade, "paper-trader", PriorityNormal)
	if err == nil {
		t.Fatal("expected context deadline error, got nil")
	}
}

func TestAcquire_ConsumersHaveIndependentBuckets(t *testing.T) {
	l := NewLimiter(testLimits(1, 1, time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := l.Acquire(ctx, ClassTrade, "paper-trader", PriorityNormal); err != nil {
		t.Fatalf("paper-trader acquire: %v", err)
	}
	// A different consumer name must have its own bucket — exhausting paper-trader's bucket must
	// never block trader's, which is the entire point of per-consumer buckets (CLAUDE.md §27.1).
	if err := l.Acquire(ctx, ClassTrade, "trader", PriorityTrader); err != nil {
		t.Fatalf("trader acquire should not be blocked by paper-trader's exhausted bucket: %v", err)
	}
}

// TestAcquire_TraderPriorityWinsUnderContention is the core priority guarantee (CLAUDE.md
// §27.1): once the trader is waiting on a class, normal-priority requests back off rather than
// racing it for the next available token. Simulated by giving the trader its own bucket that
// refills slowly, and confirming a concurrently-waiting normal request does not proceed before
// the trader's own Acquire returns, even though they're on independent buckets and could
// otherwise interleave freely.
func TestAcquire_TraderPriorityWinsUnderContention(t *testing.T) {
	l := NewLimiter(testLimits(1, 1, 80*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Drain both buckets' initial token so the next Acquire on each must wait for a refill.
	if err := l.Acquire(ctx, ClassTrade, "trader", PriorityTrader); err != nil {
		t.Fatalf("drain trader bucket: %v", err)
	}
	if err := l.Acquire(ctx, ClassTrade, "paper-trader", PriorityNormal); err != nil {
		t.Fatalf("drain normal bucket: %v", err)
	}

	var normalDone, traderDone int64
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_ = l.Acquire(ctx, ClassTrade, "trader", PriorityTrader)
		atomic.StoreInt64(&traderDone, time.Now().UnixNano())
	}()
	// Give the trader's Acquire a head start registering itself as waiting.
	time.Sleep(5 * time.Millisecond)
	go func() {
		defer wg.Done()
		_ = l.Acquire(ctx, ClassTrade, "paper-trader", PriorityNormal)
		atomic.StoreInt64(&normalDone, time.Now().UnixNano())
	}()

	wg.Wait()
	td := atomic.LoadInt64(&traderDone)
	nd := atomic.LoadInt64(&normalDone)
	if td == 0 || nd == 0 {
		t.Fatal("both acquires should have completed")
	}
	if nd < td {
		t.Fatalf("normal-priority request completed before trader's (trader=%d, normal=%d) — priority not respected", td, nd)
	}
}

func TestAcquire_UnknownClassFallsBackToOneTokenPerSecond(t *testing.T) {
	l := NewLimiter(map[EndpointClass]ClassLimit{}) // no limits configured at all
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := l.Acquire(ctx, ClassMarket, "ingestor", PriorityNormal); err != nil {
		t.Fatalf("acquire on unconfigured class should still succeed via fallback: %v", err)
	}
}

func TestDefaultLimits_CoversEveryClass(t *testing.T) {
	limits := DefaultLimits()
	for _, class := range []EndpointClass{ClassTrade, ClassLeverage, ClassAccount, ClassMarket} {
		limit, ok := limits[class]
		if !ok {
			t.Fatalf("DefaultLimits missing class %q", class)
		}
		if limit.Capacity <= 0 || limit.Refill <= 0 || limit.Interval <= 0 {
			t.Fatalf("class %q has a non-positive limit field: %+v", class, limit)
		}
	}
}
