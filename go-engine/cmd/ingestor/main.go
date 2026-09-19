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
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/ws"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
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

	// The instrument roster now comes from the DATABASE, not config.yaml (2026-09-13, migration
	// 000031). That is what lets the token-discovery scan put a newly-found token to work: while the
	// roster was trading.inst_ids plus a hand-maintained trading.symbol_map, a scanned token had
	// neither an entry nor an exec instId and could never be subscribed to.
	//
	// config.yaml's own list is still the seed for a database that has never held a roster — the
	// state of every deployment the moment this lands — so this deploy changes nothing about what is
	// collected until a scan or an operator says otherwise. See usecase.RosterFor.
	repo, err := postgres.New(ctx, cfg.Postgres.DSN)
	if err != nil {
		logger.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		logger.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}

	// CLAUDE.md §27, 2026-09-04 design: symbols are short internal identities ("BTC"), never OKX's
	// own wire-format instId — this is the ONE place in the whole pipeline that talks OKX's wire
	// format at all, so it resolves each symbol to a real instId for the WS subscription, then
	// translates every inbound message's instId back to the short symbol before anything is published
	// to Kafka.
	seedExecIDs, err := usecase.SeedExecIDs(cfg.Trading.SymbolMap, cfg.Trading.InstIDs)
	if err != nil {
		logger.Error("failed to resolve trading.inst_ids against trading.symbol_map", "error", err)
		os.Exit(1)
	}
	roster, err := usecase.RosterFor(ctx, repo, "okx", "ingest",
		cfg.Trading.InstIDs, seedExecIDs, cfg.Trading.ExecInstType, logger)
	if err != nil {
		logger.Error("failed to load instrument roster", "error", err)
		os.Exit(1)
	}
	if len(roster.Symbols) == 0 {
		// Starting with nothing to subscribe to would leave a process that looks healthy and
		// collects no data at all — the silent-data-gap failure §9 exists to prevent.
		logger.Error("instrument roster is empty for the ingest consumer — nothing to subscribe to")
		os.Exit(1)
	}

	wsInstIDs := make([]string, 0, len(roster.Symbols))
	symbolFor := make(map[string]string, len(roster.Symbols))
	for _, sym := range roster.Symbols {
		wireID := roster.ExecInstID[sym]
		wsInstIDs = append(wsInstIDs, wireID)
		symbolFor[wireID] = sym
	}
	resolveSymbol := func(wireInstID string) (string, error) {
		sym, ok := symbolFor[wireInstID]
		if !ok {
			return "", fmt.Errorf("received data for OKX instId %q with no roster entry", wireInstID)
		}
		return sym, nil
	}

	// Watch for the roster changing underneath us and restart when it does. A WS subscription is
	// fixed for the life of its socket, so adding an instrument means re-subscribing; exiting and
	// letting the restart policy relaunch re-derives every piece of state from the database, where a
	// partial in-place reload is how a service ends up half-subscribed with nothing reporting it
	// (the same mechanism cmd/strategy-tester §18 and cmd/paper-trader §22 use for config changes).
	go (&usecase.RosterWatcher{
		Repo: repo, Exchange: "okx", Consumer: "ingest",
		Interval: time.Minute, Logger: logger, Baseline: roster.Symbols,
		OnChange: func(reason string) {
			logger.Info("restarting to pick up the new instrument roster", "reason", reason)
			stop()
		},
	}).Run(ctx)

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
				// downstream consumer (paper-trader, BotTrader, the panel) only ever sees the
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

	logger.Info("starting okx ingestor", "instIds", roster.Symbols, "bars", cfg.Ingestion.Bars)

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
