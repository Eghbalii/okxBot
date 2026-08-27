// Package kafkastream provides a thin Kafka wrapper for publishing/consuming market data events —
// the internal event bus (CLAUDE.md §12), replacing the earlier Redis Streams implementation
// (internal/stream) so the same events flow through a real Kafka broker instead.
package kafkastream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	return &Publisher{
		writer: &kafka.Writer{
			Addr:                   kafka.TCP(brokers...),
			Topic:                  topic,
			Balancer:               &kafka.Hash{}, // keyed by Publish's key param, so ordering per key is preserved
			AllowAutoTopicCreation: true,
		},
		Topic: topic,
	}
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

// Consumer reads a Kafka topic via a consumer group, so multiple processes (or, within one
// process, one shared reader routing to several handlers) can share partitions and no messages
// are lost/reprocessed across restarts (each message's offset is committed after handling).
type Consumer struct {
	reader *kafka.Reader
	Topic  string
	Group  string
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

// Run blocks, delivering each message's value to handler and committing its offset after
// handling (regardless of whether handler errored — matching the previous Redis Streams
// unconditional-ack behavior: a bad message is not retried forever, handler is responsible for
// its own logging), until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context, handler func(ctx context.Context, data []byte) error) error {
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("fetch message from topic %s: %w", c.Topic, err)
		}

		if err := handler(ctx, msg.Value); err != nil {
			_ = err // handler is responsible for its own logging
		}

		if err := c.reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			return fmt.Errorf("commit offset for topic %s: %w", c.Topic, err)
		}
	}
}

// Close closes the underlying Kafka reader.
func (c *Consumer) Close() error {
	return c.reader.Close()
}
