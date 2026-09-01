// Package kafkastream provides a thin Kafka wrapper for publishing/consuming market data events —
// the internal event bus (CLAUDE.md §12), replacing the earlier Redis Streams implementation
// (internal/stream) so the same events flow through a real Kafka broker instead.
package kafkastream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
)

// Publisher publishes JSON-encoded events to a Kafka topic, keyed for per-key ordering (e.g. by
// instId, so one instrument's events always land in the same partition and are never reordered
// relative to each other).
type Publisher struct {
	writer *kafka.Writer
	Topic  string
}

// NewPublisher creates a Publisher for the given brokers and topic.
func NewPublisher(brokers []string, topic string) *Publisher {
	w := &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.Hash{}, // keyed by Publish's key param, so ordering per key is preserved
		AllowAutoTopicCreation: true,
		// Async batches writes internally instead of round-tripping to the broker synchronously
		// on every call — live-verified as a real bottleneck: at 10 liquid instruments' worth of
		// ticker volume, synchronous WriteMessages couldn't keep up even with WS message handling
		// already moved off the socket read loop (internal/okx/ws), backing up that loop's own
		// dispatch queue instead. Publish's own error return becomes unreliable once Async is on
		// (it usually returns nil immediately, before the write is actually attempted), so
		// failures are logged from Completion instead — every publisher in this codebase already
		// treats a publish failure as "log a warning and continue" (CLAUDE.md §12), so this keeps
		// that same reliability model, just moved to where Async actually surfaces the error.
		Async: true,
	}
	w.Completion = func(messages []kafka.Message, err error) {
		if err != nil {
			slog.Default().Warn("kafka async publish failed", "topic", topic, "count", len(messages), "error", err)
		}
	}
	return &Publisher{writer: w, Topic: topic}
}

// Publish marshals event to JSON and writes it to the topic, partitioned by key.
func (p *Publisher) Publish(ctx context.Context, key string, event any) error {
	b, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return p.writer.WriteMessages(ctx, kafka.Message{Key: []byte(key), Value: b})
}

// Close closes the underlying Kafka writer.
func (p *Publisher) Close() error {
	return p.writer.Close()
}

// kafkaReader is the slice of *kafka.Reader that Run actually needs — narrowed to an interface so
// Run's retry/backoff decision logic is unit-testable against a fake that can inject transient
// errors, a genuine Close()-shaped io.EOF, and context cancellation on demand, none of which are
// practical to provoke reliably against a real broker in a unit test.
type kafkaReader interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
	Close() error
}

// Consumer reads a Kafka topic via a consumer group, so multiple processes (or, within one
// process, one shared reader routing to several handlers) can share partitions and no messages
// are lost/reprocessed across restarts (each message's offset is committed after handling).
type Consumer struct {
	reader kafkaReader
	Topic  string
	Group  string

	// initialBackoff is Run's starting retry delay on a transient error, defaulting to 1s
	// (consumerMaxBackoff's own doubling then applies from there) when zero. Unexported and only
	// ever set directly by this package's own tests — production code always gets the 1s default
	// via NewConsumer; this exists purely so the retry tests don't have to wait through real
	// multi-second backoffs to prove the retry COUNT/behavior is correct.
	initialBackoff time.Duration
}

// NewConsumer creates a Consumer for the given brokers, topic, and consumer group. GroupID alone
// (no per-instrument consumer identity) is enough — kafka-go assigns this reader whichever
// partitions the group owns; callers that need to route by instrument do so inside handler, from
// the decoded message, same as before.
func NewConsumer(brokers []string, topic, group string) *Consumer {
	return &Consumer{
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers: brokers,
			Topic:   topic,
			GroupID: group,
		}),
		Topic: topic,
		Group: group,
	}
}

// consumerMaxBackoff caps the retry delay in Run's transient-error loop below, same shape as the
// OKX WS reconnect backoff (internal/okx/ws/public.go): starts at 1s, doubles, caps at 30s.
const consumerMaxBackoff = 30 * time.Second

// runOutcome is what the retry loop below does after a fetch/commit error: stop (ctx done, or the
// reader was explicitly Close()d — kafka-go signals that with io.EOF) or retry after backing off.
type runOutcome int

const (
	outcomeStop runOutcome = iota
	outcomeRetry
)

// classifyRunError decides runOutcome for an error from FetchMessage/CommitMessages. A pure
// function, deliberately separate from Run's IO, so the "which errors are fatal vs. transient"
// decision — the actual bug fixed here — is unit-testable without a real Kafka broker or timers.
func classifyRunError(ctx context.Context, err error) runOutcome {
	if ctx.Err() != nil {
		return outcomeStop
	}
	if errors.Is(err, io.EOF) {
		return outcomeStop
	}
	return outcomeRetry
}

// Run blocks, delivering each message's value to handler and committing its offset after
// handling (regardless of whether handler errored — matching the previous Redis Streams
// unconditional-ack behavior: a bad message is not retried forever, handler is responsible for
// its own logging), until ctx is cancelled or the reader is explicitly Close()d.
//
// Transient fetch/commit errors (a broker restart, a leader election, a momentary network blip —
// exactly what happens during Kafka's own crash-recovery, CLAUDE.md §16.10's precedent) are
// retried with backoff rather than returned. Before this, ANY such error made Run return, and
// every caller in this codebase (cmd/api, cmd/paper-trader, cmd/strategy-tester,
// cmd/strategy-optimizer) only logs that return and lets the goroutine exit — the outer process
// keeps running and looks completely healthy while that one data path (e.g. the panel's live
// price stream) is silently dead forever. This was the root cause of live prices never recovering
// after a Kafka restart: the consumer goroutine that produces price broadcasts had already died,
// permanently, on the first transient error it hit during Kafka's own recovery window.
//
// Only two things legitimately stop this loop (classifyRunError): ctx being done, or the reader
// having been explicitly closed (io.EOF — kafka-go's own signal that Close() was called, e.g. on
// process shutdown). Every other error is assumed transient and retried.
func (c *Consumer) Run(ctx context.Context, handler func(ctx context.Context, data []byte) error) error {
	initialBackoff := c.initialBackoff
	if initialBackoff <= 0 {
		initialBackoff = time.Second
	}
	backoff := initialBackoff
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if classifyRunError(ctx, err) == outcomeStop {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("kafka reader closed for topic %s: %w", c.Topic, err)
			}
			slog.Default().Warn("kafka fetch failed, retrying", "topic", c.Topic, "group", c.Group, "error", err, "backoff", backoff)
			if !sleepOrDone(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = initialBackoff // reset after any successful fetch, so one blip doesn't keep the next unrelated one waiting longer than necessary

		if err := handler(ctx, msg.Value); err != nil {
			_ = err // handler is responsible for its own logging
		}

		// Retries THIS message's commit specifically, in its own inner loop — unlike the fetch
		// retry above, this must not fall through to the outer loop's FetchMessage on failure:
		// FetchMessage always returns the next message from the reader's internal channel
		// regardless of whether the previous one was committed (confirmed against kafka-go's own
		// implementation), so re-fetching here would silently skip retrying this exact commit and
		// move on to a different message instead, defeating the retry's own purpose.
		for {
			err := c.reader.CommitMessages(ctx, msg)
			if err == nil {
				break
			}
			if classifyRunError(ctx, err) == outcomeStop {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("kafka reader closed for topic %s: %w", c.Topic, err)
			}
			slog.Default().Warn("kafka commit failed, retrying", "topic", c.Topic, "group", c.Group, "error", err, "backoff", backoff)
			if !sleepOrDone(ctx, backoff) {
				return ctx.Err()
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// nextBackoff doubles d, capped at consumerMaxBackoff. A pure function so the doubling/cap
// behavior is unit-testable without a real Kafka connection or a live timer.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > consumerMaxBackoff {
		d = consumerMaxBackoff
	}
	return d
}

// sleepOrDone waits for d or ctx being done, whichever comes first. Returns false if ctx ended
// the wait, so the caller can distinguish "waited the full backoff" from "should stop now".
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// Close closes the underlying Kafka reader.
func (c *Consumer) Close() error {
	return c.reader.Close()
}
