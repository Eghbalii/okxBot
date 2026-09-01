// Command api runs the dashboard/reporting backend (CLAUDE.md §11): RL model status, strategy
// CRUD + assignments + per-strategy stats, and the positions panel across paper/demo/real trading
// modes. No auth in v1 — reachable only over the OpenVPN tunnel into the server's network, so
// Config.API.Addr should stay bound to a private interface, not 0.0.0.0.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/api"
	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/rest"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

// tickEvent mirrors usecase.tickEvent's decode of the raw OKX tickers payload (CLAUDE.md §12) —
// duplicated rather than exported/shared because cmd/api only needs the two fields it re-broadcasts
// below, not the full ticker shape.
type tickEvent struct {
	InstID string `json:"instId"`
	Last   string `json:"last"`
}

// priceUpdate is the panel's live-price WebSocket message (CLAUDE.md §11.4's positions panel):
// last-traded price per instrument, pushed on every tick so the panel can compute moment-to-moment
// unrealized PnL client-side from entry_px/size/leverage rather than polling REST for it. Same
// "type" discriminator convention as usecase.PaperOrderEvent so the panel can tell the two kinds of
// message on this one socket apart.
type priceUpdate struct {
	Type   string `json:"type"` // "price"
	InstID string `json:"instId"`
	Price  string `json:"price"`
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
	// Seed origin strategy rows here too (not just cmd/paper-trader) so the Strategies panel works
	// even if paper-trader has never run yet — the two services must not depend on start order.
	if err := strategy.SeedOrigins(ctx, repo); err != nil {
		logger.Error("failed to seed origin strategies", "error", err)
		os.Exit(1)
	}

	srv := &api.Server{
		Repo:               repo,
		RLBaseURL:          cfg.RLService.URL,
		GrafanaURL:         cfg.API.GrafanaURL,
		TesterBaseURL:      cfg.Tester.URL,
		PaperTraderBaseURL: cfg.PaperTrading.URL,
		ProcessMgr:         cfg.API.ProcessMgr,
		Units:              cfg.API.Units,
		Logger:             logger,
		// Must match what the trading services seed their account row with (CLAUDE.md §15.6) —
		// both read through GetAccountEquity, so a different value here would seed a balance the
		// engine never actually traded against.
		AccountInitialUSD: cfg.Account.InitialUSD,

		// Candle backfill (CLAUDE.md §15.8) needs only read access to market data, so it takes the
		// narrow HistoryCandleFetcher port rather than the full ExchangeClient — cmd/api must not
		// be able to place an order. Public market endpoints need no credentials, so this works
		// even where OKX keys aren't configured.
		Backfill: &usecase.Backfill{
			Exchange: rest.New(cfg.OKX.RESTBaseURL, cfg.OKX.APIKey, cfg.OKX.APISecret, cfg.OKX.APIPassphrase, cfg.OKX.Simulated),
			Repo:     repo,
			Logger:   logger,
		},
		BackfillInstIDs: cfg.Trading.InstIDs,
		BackfillBars:    cfg.PaperTrading.Bars,
	}
	routes := srv.Routes() // must be called before Hub() usage below so the same *wsHub backs both

	// CLAUDE.md §11.4/§12: paper-order open/close events published by cmd/paper-trader onto Kafka
	// are relayed to every connected panel WebSocket client, replacing 5s position polling for the
	// alert (open/SL/TP) path. A dedicated consumer group ("api-ws-bridge") so this never competes
	// for offsets with paper-trader/strategy-optimizer's own groups on the same topic.
	orderEventsConsumer := kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.paper-order-events", "api-ws-bridge")
	go func() {
		err := orderEventsConsumer.Run(ctx, func(_ context.Context, data []byte) error {
			srv.Broadcast(data)
			return nil
		})
		if err != nil && ctx.Err() == nil {
			logger.Error("paper order events consumer exited", "error", err)
		}
	}()

	// Live last-traded price per instrument, for the positions panel's moment-to-moment PnL
	// (CLAUDE.md §11.4) — a distinct consumer group ("api-ws-bridge-tickers") from paper-trader's
	// own "paper-trader" group on the same okx.tickers topic, so this never competes for offsets or
	// skips messages paper-trader also needs. Reshaped to {type,instId,price} rather than forwarded
	// as OKX's raw wire payload, so the panel doesn't need to know OKX's ticker JSON shape.
	pricesConsumer := kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.tickers", "api-ws-bridge-tickers")
	go func() {
		err := pricesConsumer.Run(ctx, func(_ context.Context, data []byte) error {
			var tick tickEvent
			if err := json.Unmarshal(data, &tick); err != nil {
				return nil // malformed tick: skip rather than fail the whole consumer loop
			}
			out, err := json.Marshal(priceUpdate{Type: "price", InstID: tick.InstID, Price: tick.Last})
			if err != nil {
				return nil
			}
			srv.Broadcast(out)
			return nil
		})
		if err != nil && ctx.Err() == nil {
			logger.Error("prices consumer exited", "error", err)
		}
	}()

	httpServer := &http.Server{
		Addr:    cfg.API.Addr,
		Handler: routes,
	}

	go func() {
		<-ctx.Done()
		srv.CloseWS()
		_ = orderEventsConsumer.Close()
		_ = pricesConsumer.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("starting api server", "addr", cfg.API.Addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("api server exited", "error", err)
		os.Exit(1)
	}
}
