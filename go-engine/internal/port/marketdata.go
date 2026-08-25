package port

import "context"

// MarketDataConsumer is the port use-cases depend on to consume events from the internal event
// bus (CLAUDE.md §12). Implemented by internal/stream.Consumer (Redis Streams).
type MarketDataConsumer interface {
	Run(ctx context.Context, handler func(ctx context.Context, data []byte) error) error
}

// MarketDataPublisher is the port used to publish events onto the internal event bus. Implemented
// by internal/stream.Publisher (Redis Streams).
type MarketDataPublisher interface {
	Publish(ctx context.Context, event any) error
}
