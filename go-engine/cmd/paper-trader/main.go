// Command paper-trader runs the Paper Trading Engine (CLAUDE.md §8): it evaluates strategies
// against live OKX prices and tracks virtual orders through to close, independent of live
// trading. This is the primary source of training data for the RL model.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rez/okxBot/go-engine/internal/config"
	"github.com/rez/okxBot/go-engine/internal/okx/rest"
	"github.com/rez/okxBot/go-engine/internal/paperengine"
	"github.com/rez/okxBot/go-engine/internal/postgres"
	"github.com/rez/okxBot/go-engine/internal/strategy"
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

	restClient := rest.New(cfg.OKX.RESTBaseURL, cfg.OKX.APIKey, cfg.OKX.APISecret, cfg.OKX.APIPassphrase, cfg.OKX.Simulated)

	logger.Info("starting paper trader", "instIds", cfg.Trading.InstIDs)

	errCh := make(chan error, len(cfg.Trading.InstIDs))
	for _, instID := range cfg.Trading.InstIDs {
		engine := &paperengine.Engine{
			InstID:        instID,
			Bar:           cfg.PaperTrading.Bar,
			CandleLimit:   cfg.PaperTrading.CandleLimit,
			Strategies:    []strategy.Strategy{strategy.NewRSISMA(14, 50)},
			RESTClient:    restClient,
			Repo:          repo,
			NotionalUSD:   cfg.PaperTrading.NotionalUSD,
			MaxOpenOrders: cfg.PaperTrading.MaxOpenOrders,
			PollInterval:  time.Duration(cfg.Trading.PollIntervalSec) * time.Second,
			Logger:        logger,
		}
		go func() { errCh <- engine.Run(ctx) }()
	}

	<-ctx.Done()
	logger.Info("shutting down paper trader")
}
