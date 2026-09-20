package kafkastream

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
)

// EnsureTopicRetention sets a per-topic retention.ms/segment.bytes override, idempotently, on
// every call — the same "restart re-derives state" pattern this codebase already uses for the
// roster and strategy assignments (CLAUDE.md §53.1), rather than a one-off manual fix that a fresh
// deployment would silently lose.
//
// Needed because the broker-wide defaults in docker-compose.yml (6h retention, 128MB segments,
// tuned for okx.tickers/okx.candles.*) are wrong for a high-frequency topic like okx.orderbook:
// order-book depth is meaningless to retain beyond a very short window (nothing ever reads a stale
// snapshot), and 6h of a topic rolling a full 128MB segment every ~11 minutes accumulates to
// several GB on disk for no reason — found 2026-09-20 when okx.orderbook alone measured 3.9GB, the
// single largest consumer of the "why is the disk full" investigation that also produced the
// Resources page's rewritten cleanup button.
//
// segmentBytes is set alongside retentionMs deliberately, not left at the broker default: at a
// short retention window, a segment has to actually CLOSE before retention can delete it, so an
// oversized segment can outlive the window it's supposed to be bounded by (128MB segments take
// ~11 minutes to fill on okx.orderbook's real volume — over half of a 30-minute retention window
// before the segment is even eligible for deletion).
//
// This talks to the Kafka Admin API directly (kafka.Client.AlterConfigs) rather than shelling out
// to kafka-configs.sh — the same "no host tools inside the container" reasoning as
// internal/api/diskcleanup.go's own comment about apt-get/journalctl, and it avoids the JVM
// tooling's memory footprint, which OOM-killed a manual kafka-topics.sh invocation on this box
// during the same investigation.
func EnsureTopicRetention(ctx context.Context, brokers []string, topic string, retentionMs, segmentBytes int64) error {
	if len(brokers) == 0 {
		return fmt.Errorf("no kafka brokers configured")
	}
	client := &kafka.Client{Addr: kafka.TCP(brokers...)}
	resp, err := client.AlterConfigs(ctx, &kafka.AlterConfigsRequest{
		Addr: kafka.TCP(brokers...),
		Resources: []kafka.AlterConfigRequestResource{{
			ResourceType: kafka.ResourceTypeTopic,
			ResourceName: topic,
			Configs: []kafka.AlterConfigRequestConfig{
				{Name: "retention.ms", Value: fmt.Sprintf("%d", retentionMs)},
				{Name: "segment.bytes", Value: fmt.Sprintf("%d", segmentBytes)},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("alter configs for topic %s: %w", topic, err)
	}
	for resource, rerr := range resp.Errors {
		if rerr != nil {
			return fmt.Errorf("alter configs for topic %s (resource %s): %w", topic, resource.Name, rerr)
		}
	}
	return nil
}

// EnsureTopicRetentionInBackground retries EnsureTopicRetention on a fixed interval until it
// succeeds once, then stops — self-healing for the ordinary startup race where this runs before
// AllowAutoTopicCreation has ever fired for a brand-new topic (the config target doesn't exist yet
// to alter), matching CLAUDE.md §12's existing "first publish to a new topic can transiently fail,
// self-resolving" precedent rather than treating it as fatal. Logs once on first success and once
// if ctx is cancelled before that ever happens (e.g. process shutdown) so a persistent problem
// doesn't just go silent — a topic config that never actually applies is exactly the kind of "looks
// healthy, isn't" gap CLAUDE.md's own incident history (§16.10, §40) keeps calling out.
//
// A nil logger is tolerated (falls back to slog.Default()) rather than nil-panicking on the first
// success log line — matching the defensive-nil-logger pattern used elsewhere in this codebase.
func EnsureTopicRetentionInBackground(ctx context.Context, logger *slog.Logger, brokers []string, topic string, retentionMs, segmentBytes int64) {
	if logger == nil {
		logger = slog.Default()
	}
	apply := func(ctx context.Context) error { return EnsureTopicRetention(ctx, brokers, topic, retentionMs, segmentBytes) }
	go retryUntilSuccess(ctx, logger, 10*time.Second, apply, func() {
		logger.Info("kafka topic retention override applied", "topic", topic, "retentionMs", retentionMs, "segmentBytes", segmentBytes)
	}, func(err error) {
		logger.Warn("kafka topic retention override not applied yet, will retry", "topic", topic, "error", err)
	})
}

// retryUntilSuccess calls fn on the given interval until it returns nil, then calls onSuccess once
// and stops; onFailure is called (not fatally) after each unsuccessful attempt. Extracted from
// EnsureTopicRetentionInBackground so the retry/backoff-timing behavior is unit-testable against a
// fake fn instead of a real Kafka broker — the same reasoning kafka_test.go's fakeReader exists for
// Consumer.Run's own retry logic.
func retryUntilSuccess(ctx context.Context, logger *slog.Logger, interval time.Duration, fn func(context.Context) error, onSuccess func(), onFailure func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := fn(ctx); err == nil {
			onSuccess()
			return
		} else {
			onFailure(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
