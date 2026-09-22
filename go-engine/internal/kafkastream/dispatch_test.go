package kafkastream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
)

// btcMsg builds a single fetchResult carrying instId="BTC", repeated forever by fakeReader once
// exhausted — enough for Run's dispatch loop to deliver at least one message before the test's
// context is cancelled.
func btcMsg() kafka.Message {
	return kafka.Message{Value: []byte(`{"instId":"BTC"}`)}
}

// TestDispatcher_TwoHandlersForTheSameInstIDBothReceiveTheMessage is the actual production fix
// (2026-09-22): a trading engine and usecase.BTCReference can both register for "BTC" on the same
// Dispatcher and both must see every message — neither silently displaces the other. Before this
// fix, the second Register call for an instID already registered OVERWROTE the first (a plain map
// assignment), which is exactly what happened live: cmd/paper-trader/main.go registers both BTC's
// own trading-engine candle consumer and BTCReference's independent one against the same
// dispatcher whenever BTC is in the traded roster, and whichever of the two goroutines called
// Register last (a startup race, both starting within engineStartStagger of each other) silently
// won — on paper-trader-mexc's 2-instrument roster this consistently went the wrong way, and BTC's
// trading engine never received a single candle.
func TestDispatcher_TwoHandlersForTheSameInstIDBothReceiveTheMessage(t *testing.T) {
	reader := &fakeReader{fetchResults: []fetchResult{{msg: btcMsg()}}}
	d := NewDispatcher(&Consumer{reader: reader, Topic: "t", Group: "g"})

	var mu sync.Mutex
	var firstCalls, secondCalls int
	d.Register("BTC", func(ctx context.Context, data []byte) error {
		mu.Lock()
		firstCalls++
		mu.Unlock()
		return nil
	})
	d.Register("BTC", func(ctx context.Context, data []byte) error {
		mu.Lock()
		secondCalls++
		mu.Unlock()
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if firstCalls == 0 {
		t.Error("first handler for \"BTC\" was never called — the second Register call silently displaced it")
	}
	if secondCalls == 0 {
		t.Error("second handler for \"BTC\" was never called")
	}
}

// TestDispatcher_RegisteredConsumerRunFansOutTheSameWay proves the fix holds through the actual
// call path cmd/paper-trader/main.go uses (Dispatcher.ForInstrument(...).Run(...) — see
// RegisteredConsumer.Run — not a direct Dispatcher.Register call), since that indirection is what
// production code goes through and a fix that only worked via the direct call would not have
// caught the real bug.
func TestDispatcher_RegisteredConsumerRunFansOutTheSameWay(t *testing.T) {
	reader := &fakeReader{fetchResults: []fetchResult{{msg: btcMsg()}}}
	d := NewDispatcher(&Consumer{reader: reader, Topic: "t", Group: "g"})

	var mu sync.Mutex
	engineCalls, refCalls := 0, 0

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// Mirrors cmd/paper-trader/main.go exactly: the trading engine's own candle consumer and
	// BTCReference's independent one, both obtained via ForInstrument("BTC") against the SAME
	// dispatcher, both started as their own goroutine (matching the real engineStartStagger race).
	engine := d.ForInstrument("BTC")
	ref := d.ForInstrument("BTC")
	go func() {
		_ = engine.Run(ctx, func(ctx context.Context, data []byte) error {
			mu.Lock()
			engineCalls++
			mu.Unlock()
			return nil
		})
	}()
	go func() {
		_ = ref.Run(ctx, func(ctx context.Context, data []byte) error {
			mu.Lock()
			refCalls++
			mu.Unlock()
			return nil
		})
	}()

	// Give both registration goroutines a moment to run before the dispatcher starts reading —
	// mirrors real startup ordering closely enough without needing the real 300ms stagger.
	time.Sleep(20 * time.Millisecond)
	_ = d.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if engineCalls == 0 {
		t.Error("the trading engine's own consumer never received \"BTC\" candles — BTCReference's registration silently displaced it")
	}
	if refCalls == 0 {
		t.Error("BTCReference's consumer never received \"BTC\" candles")
	}
}

// TestDispatcher_UnregisteredInstIDIsDroppedSilently guards the pre-existing, still-correct
// behavior this fix must not change: a message for an instID nobody registered for is simply
// dropped, not an error — the shared topic legitimately carries other instruments' data.
func TestDispatcher_UnregisteredInstIDIsDroppedSilently(t *testing.T) {
	reader := &fakeReader{fetchResults: []fetchResult{{msg: kafka.Message{Value: []byte(`{"instId":"ETH"}`)}}}}
	d := NewDispatcher(&Consumer{reader: reader, Topic: "t", Group: "g"})

	called := false
	d.Register("BTC", func(ctx context.Context, data []byte) error {
		called = true
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := d.Run(ctx); err != nil && err != context.DeadlineExceeded {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Error("handler registered for \"BTC\" was called for an \"ETH\" message")
	}
}
