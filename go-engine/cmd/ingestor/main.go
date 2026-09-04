// Command ingestor connects to OKX's public/business WebSockets and streams ticker + candle
// data into Kafka (the internal event bus, CLAUDE.md §12) for downstream consumption by the
// Paper Trading Engine, the trading engine, and research/feature building.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/ws"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// CLAUDE.md §27, 2026-09-04 design: trading.inst_ids are short internal symbols ("BTC"), never
	// OKX's own wire-format instId — this is the ONE place in the whole pipeline that talks OKX's
	// wire format at all, so it resolves each symbol to a real instId for the WS subscription, then
	// translates every inbound message's instId back to the short symbol before anything is
	// published to Kafka. Resolved once at startup, failing loudly (not subscribing to nothing) if
	// any configured symbol has no map entry.
	symbolMap := okx.SymbolMap(cfg.Trading.SymbolMap)
	wsInstIDs, err := symbolMap.ResolveAll(cfg.Trading.InstIDs)
	if err != nil {
		logger.Error("failed to resolve trading.inst_ids against trading.symbol_map", "error", err)
		os.Exit(1)
	}
	symbolFor := reverseSymbolMap(cfg.Trading.InstIDs, wsInstIDs)
	resolveSymbol := func(wireInstID string) (string, error) {
		sym, ok := symbolFor[wireInstID]
		if !ok {
			return "", fmt.Errorf("received data for OKX instId %q with no configured symbol_map entry", wireInstID)
		}
		return sym, nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metrics.Serve(envOr("METRICS_ADDR", ":9101"), logger)

	tickerPub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.tickers")
	defer tickerPub.Close()

	tickerClient := &ws.PublicClient{
		URL:     cfg.OKX.PublicWSURL,
		Channel: "tickers",
		InstIDs: wsInstIDs,
		Logger:  logger,
		Handler: func(msg ws.Message) {
			sym, err := resolveSymbol(msg.Arg.InstID)
			if err != nil {
				logger.Warn("failed to resolve inbound ticker instId to a symbol", "error", err)
				return
			}
			var raw []json.RawMessage
			if err := json.Unmarshal(msg.Data, &raw); err != nil {
				logger.Warn("failed to decode ticker payload", "error", err)
				return
			}
			for _, r := range raw {
				// Rewrite the wire instId field to the short symbol before publishing, so every
				// downstream consumer (paper-trader, RealTrader, the panel) only ever sees the
				// internal identity, never OKX's own wire format.
				rewritten, err := rewriteInstID(r, sym)
				if err != nil {
					logger.Warn("failed to rewrite ticker instId", "error", err)
					continue
				}
				if err := tickerPub.Publish(ctx, sym, rewritten); err != nil {
					logger.Warn("failed to publish tick to kafka", "error", err)
					continue
				}
				metrics.IngestorEventsTotal.WithLabelValues("tick", sym).Inc()
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
			InstIDs: wsInstIDs,
			Logger:  logger,
			Handler: func(msg ws.Message) {
				sym, err := resolveSymbol(msg.Arg.InstID)
				if err != nil {
					logger.Warn("failed to resolve inbound candle instId to a symbol", "bar", bar, "error", err)
					return
				}
				var bars [][]string
				if err := json.Unmarshal(msg.Data, &bars); err != nil {
					logger.Warn("failed to decode candle payload", "bar", bar, "error", err)
					return
				}
				for _, row := range bars {
					event := candleEvent{InstID: sym, Bar: bar, Candle: row}
					if err := pub.Publish(ctx, sym, event); err != nil {
						logger.Warn("failed to publish candle to kafka", "bar", bar, "error", err)
						continue
					}
					metrics.IngestorEventsTotal.WithLabelValues("candle", sym).Inc()
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

// reverseSymbolMap builds the real-OKX-instId -> short-symbol lookup used to translate every
// inbound WS message back to the internal identity. symbols and resolvedInstIDs must be the same
// length and in the same order (as SymbolMap.ResolveAll guarantees).
func reverseSymbolMap(symbols, resolvedInstIDs []string) map[string]string {
	out := make(map[string]string, len(symbols))
	for i, sym := range symbols {
		out[resolvedInstIDs[i]] = sym
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// rewriteInstID re-marshals a raw OKX ticker payload with its "instId" field replaced by the
// short internal symbol, so a downstream consumer reading this Kafka message never sees OKX's own
// wire-format instId at all (CLAUDE.md §27, 2026-09-04 design). The candle path doesn't need this
// helper — candleEvent is a struct this code controls directly, not a raw JSON passthrough.
func rewriteInstID(raw json.RawMessage, symbol string) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode ticker payload for instId rewrite: %w", err)
	}
	m["instId"] = symbol
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("re-encode ticker payload after instId rewrite: %w", err)
	}
	return out, nil
}
