// Command ingestor connects to OKX's public/business WebSockets and streams ticker + candle
// data into Kafka (the internal event bus, CLAUDE.md §12) for downstream consumption by the
// Paper Trading Engine, the trading engine, and research/feature building.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/ws"
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

	tickerPub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.tickers")
	defer tickerPub.Close()

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
				if err := tickerPub.Publish(ctx, msg.Arg.InstID, json.RawMessage(r)); err != nil {
					logger.Warn("failed to publish tick to kafka", "error", err)
					continue
				}
				metrics.IngestorEventsTotal.WithLabelValues("tick", msg.Arg.InstID).Inc()
			}
		},
	}

	// Candlesticks live on the business WS endpoint in OKX v5, separate from public tickers. One
	// WS connection + one Kafka topic per configured timeframe, so consumers only ever see the
	// bar they subscribed to and don't need to filter out other timeframes themselves. Each
	// topic's messages are keyed by instId (internal/kafkastream.Publisher), so one instrument's
	// candles stay strictly ordered within its own partition.
	candlePubs := make(map[string]*kafkastream.Publisher, len(cfg.Ingestion.Bars))
	candleClients := make([]*ws.PublicClient, 0, len(cfg.Ingestion.Bars))
	for _, bar := range cfg.Ingestion.Bars {
		bar := bar
		pub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.candles."+bar)
		candlePubs[bar] = pub
		defer pub.Close()

		candleClients = append(candleClients, &ws.PublicClient{
			URL:     cfg.OKX.BusinessWSURL,
			Channel: "candle" + bar,
			InstIDs: cfg.Trading.InstIDs,
			Logger:  logger,
			Handler: func(msg ws.Message) {
				var bars [][]string
				if err := json.Unmarshal(msg.Data, &bars); err != nil {
					logger.Warn("failed to decode candle payload", "bar", bar, "error", err)
					return
				}
				for _, row := range bars {
					event := candleEvent{InstID: msg.Arg.InstID, Bar: bar, Candle: row}
					if err := pub.Publish(ctx, msg.Arg.InstID, event); err != nil {
						logger.Warn("failed to publish candle to kafka", "bar", bar, "error", err)
						continue
					}
					metrics.IngestorEventsTotal.WithLabelValues("candle", msg.Arg.InstID).Inc()
				}
			},
		})
	}

	logger.Info("starting okx ingestor", "instIds", cfg.Trading.InstIDs, "bars", cfg.Ingestion.Bars)

	errCh := make(chan error, 1+len(candleClients))
	go func() { errCh <- tickerClient.Run(ctx) }()
	for _, c := range candleClients {
		c := c
		go func() { errCh <- c.Run(ctx) }()
	}

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

// candleEvent wraps a raw OKX candle array with the instrument id and bar it belongs to, since
// the candle array itself (unlike ticker payloads) doesn't carry instId, and each bar publishes
// to its own Redis stream but shares this same event shape.
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
