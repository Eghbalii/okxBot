// Command ingestor connects to OKX's public/business WebSockets and streams ticker + candle
// data into Redis (the internal event bus, CLAUDE.md §12) for downstream consumption by the
// Paper Trading Engine, the trading engine, and research/feature building.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rez/okxBot/go-engine/internal/config"
	"github.com/rez/okxBot/go-engine/internal/metrics"
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

	metrics.Serve(envOr("METRICS_ADDR", ":9101"), logger)

	tickerPub := stream.NewPublisher(cfg.Redis.Addr, "okx:tickers")
	defer tickerPub.Close()
	candlePub := stream.NewPublisher(cfg.Redis.Addr, "okx:candles")
	defer candlePub.Close()

	tickerClient := &ws.PublicClient{
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
				if err := tickerPub.Publish(ctx, json.RawMessage(r)); err != nil {
					logger.Warn("failed to publish tick to redis", "error", err)
					continue
				}
				metrics.IngestorEventsTotal.WithLabelValues("tick", msg.Arg.InstID).Inc()
			}
		},
	}

	// Candlesticks live on the business WS endpoint in OKX v5, separate from public tickers.
	candleChannel := "candle" + cfg.PaperTrading.Bar
	candleClient := &ws.PublicClient{
		URL:     cfg.OKX.BusinessWSURL,
		Channel: candleChannel,
		InstIDs: cfg.Trading.InstIDs,
		Logger:  logger,
		Handler: func(msg ws.Message) {
			var bars [][]string
			if err := json.Unmarshal(msg.Data, &bars); err != nil {
				logger.Warn("failed to decode candle payload", "error", err)
				return
			}
			for _, bar := range bars {
				event := candleEvent{InstID: msg.Arg.InstID, Bar: cfg.PaperTrading.Bar, Candle: bar}
				if err := candlePub.Publish(ctx, event); err != nil {
					logger.Warn("failed to publish candle to redis", "error", err)
					continue
				}
				metrics.IngestorEventsTotal.WithLabelValues("candle", msg.Arg.InstID).Inc()
			}
		},
	}

	logger.Info("starting okx ingestor", "instIds", cfg.Trading.InstIDs, "candleChannel", candleChannel)

	errCh := make(chan error, 2)
	go func() { errCh <- tickerClient.Run(ctx) }()
	go func() { errCh <- candleClient.Run(ctx) }()

	select {
	case <-ctx.Done():
		logger.Info("shutting down ingestor")
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			logger.Error("ingestor stopped with error", "error", err)
			os.Exit(1)
		}
	}
}

// candleEvent wraps a raw OKX candle array with the instrument id it belongs to, since the
// candle array itself (unlike ticker payloads) doesn't carry instId.
type candleEvent struct {
	InstID string   `json:"instId"`
	Bar    string   `json:"bar"`
	Candle []string `json:"candle"`
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
