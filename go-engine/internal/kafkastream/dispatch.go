package kafkastream

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// Dispatcher fans out one shared Kafka consumer-group reader's messages to several per-instrument
// handlers, keyed by each message's own instId field. This exists because a single Kafka
// consumer-group reader owns whole partitions (there is no per-instrument consumer identity the
// way Redis Streams' XREADGROUP had) — services that used to run one Consumer per instrument on a
// shared group now run one Consumer per topic and register each instrument's handler here instead.
//
// Register and Run's internal lookups can happen concurrently (e.g. a RegisteredConsumer.Run
// registering from its own goroutine while the dispatcher's Run loop is already reading), so the
// handler map is mutex-guarded rather than assuming all Register calls happen before Run starts.
//
// More than one handler CAN be registered for the same instID (2026-09-22 fix) — every message for
// that instID is delivered to all of them. Before this, a second Register call for an instID
// already registered silently OVERWROTE the first, which is exactly what happened in production:
// cmd/paper-trader/main.go registers BOTH the per-instrument trading engine's own candle consumer
// AND usecase.BTCReference's independent consumer against "BTC" on the same dispatcher whenever
// BTC is in the traded roster (BTCReference's own doc comment explains why it deliberately does
// NOT borrow the trading engine's window instead — it needs its own event-bus-sourced one). Which
// registration "won" the map write was a startup race between two goroutines starting within
// engineStartStagger (300ms) of each other, and on a small roster (2 instruments, the MEXC
// comparison profile) it consistently went the wrong way: BTCReference silently stole BTC's whole
// candle stream, so the BTC trading engine itself never received a single candle. The same
// unguarded race exists on the larger OKX/bot-trading rosters (cmd/trader has the identical
// pattern) — it happened not to manifest there, which is a reason to fix the dispatcher itself
// rather than special-case around one caller.
type Dispatcher struct {
	consumer *Consumer

	mu       sync.RWMutex
	handlers map[string][]func(ctx context.Context, data []byte) error
}

// NewDispatcher wraps consumer, dispatching each message to every handler registered for its
// instId (see Register). Messages for an unregistered instId are dropped silently — the shared
// topic can carry other instruments' data this process doesn't care about.
func NewDispatcher(consumer *Consumer) *Dispatcher {
	return &Dispatcher{consumer: consumer, handlers: make(map[string][]func(ctx context.Context, data []byte) error)}
}

// Register ADDS handler to instID's list — it does not replace an earlier registration for the
// same instID, so two independent consumers of one instrument (e.g. a trading engine and
// usecase.BTCReference both wanting "BTC") both receive every message. Safe to call concurrently
// with Run and with other Register calls.
func (d *Dispatcher) Register(instID string, handler func(ctx context.Context, data []byte) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[instID] = append(d.handlers[instID], handler)
}

// Run blocks, reading the underlying consumer and routing each message by instId to every handler
// registered for it, until ctx is cancelled.
//
// Handlers run sequentially, not concurrently, and the first error stops delivery to any
// handlers after it for that message (matching the original single-handler contract's error
// semantics exactly — a message either fully succeeds or the whole Run call returns the error,
// same as before this fixed-fan-out change; there was never more than one handler to run
// concurrently against before now).
func (d *Dispatcher) Run(ctx context.Context) error {
	return d.consumer.Run(ctx, func(ctx context.Context, data []byte) error {
		var envelope struct {
			InstID string `json:"instId"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return fmt.Errorf("decode instId envelope: %w", err)
		}
		d.mu.RLock()
		handlers := d.handlers[envelope.InstID]
		d.mu.RUnlock()
		if len(handlers) == 0 {
			return nil // shared topic across instruments; this process doesn't handle this one
		}
		for _, handler := range handlers {
			if err := handler(ctx, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close closes the underlying consumer.
func (d *Dispatcher) Close() error {
	return d.consumer.Close()
}

// RegisteredConsumer is a port.MarketDataConsumer-shaped handle for one instrument's slice of a
// Dispatcher. Its Run registers the handler with the dispatcher and then blocks until ctx is
// cancelled — the dispatcher's own Run (started once per topic, not per instrument) is what
// actually reads Kafka. This lets callers built around "one MarketDataConsumer per instrument"
// (e.g. usecase.PaperTrader's TickConsumer/CandleConsumers fields) keep that shape unchanged
// while the real consume loop is now shared across instruments, matching Kafka's
// consumer-group-owns-partitions model instead of Redis Streams' per-consumer-identity model.
type RegisteredConsumer struct {
	dispatcher *Dispatcher
	instID     string
}

// ForInstrument returns a RegisteredConsumer that, once Run, routes instID's messages (as read by
// d's underlying topic consumer) to the handler passed to Run.
func (d *Dispatcher) ForInstrument(instID string) *RegisteredConsumer {
	return &RegisteredConsumer{dispatcher: d, instID: instID}
}

// Run registers handler for this instrument and blocks until ctx is done.
func (c *RegisteredConsumer) Run(ctx context.Context, handler func(ctx context.Context, data []byte) error) error {
	c.dispatcher.Register(c.instID, handler)
	<-ctx.Done()
	return ctx.Err()
}
