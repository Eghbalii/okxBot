// Package stream provides a thin Redis Streams wrapper for publishing/consuming market ticks.
package stream

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Publisher publishes JSON-encoded events to a Redis stream.
type Publisher struct {
	rdb    *redis.Client
	Stream string
}

// NewPublisher creates a Publisher for the given Redis address and stream name.
func NewPublisher(addr, stream string) *Publisher {
	return &Publisher{
		rdb:    redis.NewClient(&redis.Options{Addr: addr}),
		Stream: stream,
	}
}

// Publish adds an event to the stream, trimming to the most recent ~100k entries.
func (p *Publisher) Publish(ctx context.Context, event any) error {
	b, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.Stream,
		MaxLen: 100_000,
		Approx: true,
		Values: map[string]any{"data": b},
	}).Err()
}

// Close closes the underlying Redis connection.
func (p *Publisher) Close() error {
	return p.rdb.Close()
}
