// Command ingestor connects to OKX's public WebSocket and streams ticker data into Redis for
// downstream consumption (feature building, research, monitoring).
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rez/okxBot/go-engine/internal/config"
	"github.com/rez/okxBot/go-engine/internal/okx/ws"
	"github.com/rez/okxBot/go-engine/internal/stream"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pub := stream.NewPublisher(cfg.Redis.Addr, "okx:tickers")
	defer pub.Close()

	client := &ws.PublicClient{
		URL:     cfg.OKX.PublicWSURL,
		Channel: "tickers",
		InstIDs: cfg.Trading.InstIDs,
		Logger:  logger,
		Handler: func(msg ws.Message) {
			var raw []json.RawMessage
			if err := json.Unmarshal(msg.Data, &raw); err != nil {
				logger.Warn("failed to decode ticker payload", "error", err)
				return
			}
			for _, r := range raw {
				if err := pub.Publish(ctx, json.RawMessage(r)); err != nil {
					logger.Warn("failed to publish tick to redis", "error", err)
				}
			}
		},
	}

	logger.Info("starting okx ingestor", "instIds", cfg.Trading.InstIDs)
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("ingestor stopped with error", "error", err)
		os.Exit(1)
	}
}
