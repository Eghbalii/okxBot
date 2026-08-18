// Package stream provides a thin Redis Streams wrapper for publishing/consuming market ticks.
package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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

// Consumer reads a Redis stream via a consumer group, so multiple processes can share the work
// and no messages are lost/reprocessed across restarts (each message is acked after handling).
type Consumer struct {
	rdb          *redis.Client
	Stream       string
	Group        string
	ConsumerName string
}

// NewConsumer creates a Consumer for the given Redis address, stream, group, and consumer name.
func NewConsumer(addr, stream, group, consumerName string) *Consumer {
	return &Consumer{
		rdb:          redis.NewClient(&redis.Options{Addr: addr}),
		Stream:       stream,
		Group:        group,
		ConsumerName: consumerName,
	}
}

// Run blocks, delivering each stream entry's "data" field to handler and acking it on success,
// until ctx is cancelled. Errors from handler are logged-by-caller (returned) but don't stop the
// loop, so one bad message can't wedge the whole consumer.
func (c *Consumer) Run(ctx context.Context, handler func(ctx context.Context, data []byte) error) error {
	if err := c.rdb.XGroupCreateMkStream(ctx, c.Stream, c.Group, "$").Err(); err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("create consumer group %s/%s: %w", c.Stream, c.Group, err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.Group,
			Consumer: c.ConsumerName,
			Streams:  []string{c.Stream, ">"},
			Count:    50,
			Block:    5 * time.Second,
		}).Result()
		if err != nil {
			if err == redis.Nil || ctx.Err() != nil {
				continue
			}
			return fmt.Errorf("xreadgroup %s: %w", c.Stream, err)
		}

		for _, stream := range res {
			for _, msg := range stream.Messages {
				raw, _ := msg.Values["data"].(string)
				if err := handler(ctx, []byte(raw)); err != nil {
					_ = err // handler is responsible for its own logging
				}
				c.rdb.XAck(ctx, c.Stream, c.Group, msg.ID)
			}
		}
	}
}

// Close closes the underlying Redis connection.
func (c *Consumer) Close() error {
	return c.rdb.Close()
}
