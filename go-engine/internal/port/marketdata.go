package port

import "context"

// MarketDataConsumer is the port use-cases depend on to consume events from the internal event
// bus (CLAUDE.md §12). Implemented by internal/kafkastream.Consumer (Kafka).
type MarketDataConsumer interface {
	Run(ctx context.Context, handler func(ctx context.Context, data []byte) error) error
}

// MarketDataPublisher is the port used to publish events onto the internal event bus. Implemented
// by internal/kafkastream.Publisher (Kafka). key selects the partition (e.g. an instrument's
// instId), so a single instrument's events stay strictly ordered relative to each other.
type MarketDataPublisher interface {
	Publish(ctx context.Context, key string, event any) error
}
