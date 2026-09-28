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
	"strings"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
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

	// MaxMessageAge drops messages older than this before they reach handler (see Run). Zero
	// disables the check. Set by NewConsumer to defaultMaxMessageAge; overridable for a consumer
	// that legitimately wants to process history.
	MaxMessageAge time.Duration
	// lastStaleLog rate-limits the stale-drop warning to one per second. Only touched from Run's
	// single goroutine, so it needs no synchronisation.
	lastStaleLog time.Time
}

// defaultMaxMessageAge bounds how old a message may be and still be acted on. Market data's value
// is entirely in being current: this bus carries ticks and candle updates that drive live trading
// decisions, so a message from minutes ago is wrong rather than merely late. Two minutes is well
// clear of any normal processing lag (this pipeline runs at sub-second latency) while still
// catching the failure that motivated it — a consumer resuming from a far-behind committed offset
// after a broker restart, replaying hours of ticks into live decision code.
//
// This is the default for everything EXCEPT ticker topics, which get tickerMaxMessageAge instead
// (see NewConsumer) — candle topics still want the full 2 minutes, since a candle only closes once
// per bar and losing one to a tight staleness window during a brief catch-up is a real gap in the
// strategy's decision history, not just a stale display value.
const defaultMaxMessageAge = 2 * time.Minute

// tickerMaxMessageAge bounds staleness specifically for "*.tickers" topics (2026-09-28, operator
// decision after a live incident): a ticker's only purpose is "what is the price right now" — a
// price from even 10-20 seconds ago during a backlog catch-up has zero value once a fresher one is
// sitting right behind it in the same backlog, unlike a candle close or a strategy signal, which
// each represent a discrete event worth not losing. Observed directly after a Kafka reconnect: with
// the old 2-minute window, catching up through a backlog fed the panel 10-20 price "changes" per
// second, each one a real (if very recent) historical price with no decision value and a
// deliberately confusing, flickering result on screen. 3 seconds is chosen to comfortably clear
// this pipeline's normal sub-second latency while being tight enough that a backlog drains as a
// jump straight to the current price instead of a replay of everything in between.
const tickerMaxMessageAge = 3 * time.Second

// isTickerTopic reports whether topic is a "*.tickers" topic (e.g. "okx.tickers", "mexc.tickers")
// — every exchange in this codebase publishes ticks under that exact suffix (cmd/ingestor), so a
// suffix check is reliable without threading a new parameter through every NewConsumer call site.
func isTickerTopic(topic string) bool {
	return strings.HasSuffix(topic, ".tickers")
}

// isStale reports whether a message published at msgTime is too old to act on. A zero time (a
// broker that did not stamp one) is never stale: refusing to process unstamped messages would
// silently halt the pipeline, which is far worse than the staleness this guards against.
func (c *Consumer) isStale(msgTime time.Time) bool {
	if c.MaxMessageAge <= 0 || msgTime.IsZero() {
		return false
	}
	return time.Since(msgTime) > c.MaxMessageAge
}

// NewConsumer creates a Consumer for the given brokers, topic, and consumer group. GroupID alone
// (no per-instrument consumer identity) is enough — kafka-go assigns this reader whichever
// partitions the group owns; callers that need to route by instrument do so inside handler, from
// the decoded message, same as before.
//
// StartOffset is LastOffset, NOT kafka-go's FirstOffset default. It applies ONLY when the group
// has no committed offset for a partition — a group that has run before always resumes exactly
// where it committed, so this can never skip a message an existing consumer had not yet processed.
//
// Why it matters (found 2026-09-08, on real money): a brand-new group otherwise replays the whole
// retained backlog — 24h of ticks under this project's retention. Every replayed tick is fed to
// live decision code as if it were current, so a real position was closed as a take-profit against
// a price from three hours earlier, at a level the market had not traded at since (real order 3:
// SOL short, TP 102.015, closed at 102 from a 13:40 tick when the market was at 103.9 and its
// session low was 103.86). Stale market data driving a live trading decision is the failure this
// prevents; a fresh consumer wants the CURRENT state of the world, never a recording of a past one.
//
// Candle topics get the same treatment, which is safe for the same reason: PaperTrader/BotTrader
// seed their candle windows from Postgres at startup (CLAUDE.md §14), so history comes from the
// database rather than from replaying the bus.
//
// CommitInterval batches offset commits instead of the kafka-go default of committing
// synchronously on every single CommitMessages call (2026-09-28, root-cause fix): a consumer
// draining a large backlog — every dropped-as-stale message still gets committed so the reader
// advances past it, see Run — was paying one broker round-trip PER MESSAGE just to skip it, which
// under any CPU contention (a concurrent process pinning the host's cores) throttled drain
// throughput below the topic's own production rate, so lag GREW instead of shrinking and never
// recovered on its own (root-caused live: paper-trader's okx.tickers lag reached ~715k and was
// still climbing after CPU pressure from another process eased, only fixed by a manual offset
// reset). Batching commits removes that per-message broker round-trip from the hot path entirely,
// so even a CPU-starved consumer can fetch-and-drop through a backlog far faster than one message
// per commit round-trip allowed. Trade-off: on an unclean process exit, up to CommitInterval's
// worth of already-handled messages can be reprocessed after restart rather than being lost —
// accepted because every handler on this bus is already idempotent-safe against exactly that (SL/TP
// checks and PnL tracking re-derive from current state, they don't accumulate irreversibly per
// tick).
func NewConsumer(brokers []string, topic, group string) *Consumer {
	maxAge := defaultMaxMessageAge
	if isTickerTopic(topic) {
		maxAge = tickerMaxMessageAge
	}
	return &Consumer{
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:        brokers,
			Topic:          topic,
			GroupID:        group,
			StartOffset:    kafka.LastOffset,
			CommitInterval: time.Second,
		}),
		Topic:         topic,
		Group:         group,
		MaxMessageAge: maxAge,
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

		// Defence in depth behind StartOffset (see NewConsumer): drop a message old enough that
		// acting on it would be acting on a price the market has long since left behind. This
		// catches the case StartOffset cannot — a group with an ALREADY-committed but far-behind
		// offset, e.g. after the broker itself restarts and a consumer resumes from a stale commit.
		//
		// Skipped messages are still committed below, so the reader advances through a backlog
		// rather than stalling on it. Dropping is the correct action rather than processing late:
		// this bus carries market prices, whose entire value is being current — a tick from hours
		// ago is not "late data" to catch up on, it is wrong data (real order 3 was closed as a
		// take-profit against a 3-hour-old price at a level the market had not traded at since).
		if c.isStale(msg.Time) {
			metrics.KafkaStaleMessagesTotal.WithLabelValues(c.Topic, c.Group).Inc()
			// Logged at most once per second: draining a backlog drops thousands of messages, and
			// a line each would bury everything else in the log.
			if time.Since(c.lastStaleLog) > time.Second {
				c.lastStaleLog = time.Now()
				slog.Default().Warn("dropping stale kafka message", "topic", c.Topic, "group", c.Group,
					"age", time.Since(msg.Time).Round(time.Second), "max", c.MaxMessageAge)
			}
		} else if err := handler(ctx, msg.Value); err != nil {
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
