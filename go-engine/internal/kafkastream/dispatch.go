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
type Dispatcher struct {
	consumer *Consumer

	mu       sync.RWMutex
	handlers map[string]func(ctx context.Context, data []byte) error
}

// NewDispatcher wraps consumer, dispatching each message to the handler registered for its
// instId (see Register). Messages for an unregistered instId are dropped silently — the shared
// topic can carry other instruments' data this process doesn't care about.
func NewDispatcher(consumer *Consumer) *Dispatcher {
	return &Dispatcher{consumer: consumer, handlers: make(map[string]func(ctx context.Context, data []byte) error)}
}

// Register assigns instID's messages to handler. Safe to call concurrently with Run.
func (d *Dispatcher) Register(instID string, handler func(ctx context.Context, data []byte) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[instID] = handler
}

// Run blocks, reading the underlying consumer and routing each message by instId, until ctx is
// cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	return d.consumer.Run(ctx, func(ctx context.Context, data []byte) error {
		var envelope struct {
			InstID string `json:"instId"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return fmt.Errorf("decode instId envelope: %w", err)
		}
		d.mu.RLock()
		handler, ok := d.handlers[envelope.InstID]
		d.mu.RUnlock()
		if !ok {
			return nil // shared topic across instruments; this process doesn't handle this one
		}
		return handler(ctx, data)
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
