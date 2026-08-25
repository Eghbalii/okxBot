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

	"github.com/rez/okxBot/go-engine/internal/config"
	"github.com/rez/okxBot/go-engine/internal/metrics"
	"github.com/rez/okxBot/go-engine/internal/okx/rest"
	"github.com/rez/okxBot/go-engine/internal/port"
	"github.com/rez/okxBot/go-engine/internal/postgres"
	"github.com/rez/okxBot/go-engine/internal/strategy"
	"github.com/rez/okxBot/go-engine/internal/stream"
	"github.com/rez/okxBot/go-engine/internal/usecase"
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

	metrics.Serve(envOr("METRICS_ADDR", ":9102"), logger)

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

	// Strategy assignments: which strategy evaluates on which timeframe's candle close. Only
	// "1m" is populated today (matching the pre-multi-timeframe behavior exactly); the other
	// configured bars (e.g. "15m", "1h") are ingested and persisted but have no strategy attached
	// yet — add more StrategyAssignment entries here as strategies for those timeframes exist.
	strategies := []usecase.StrategyAssignment{
		{Bar: "1m", Strategy: strategy.NewRSISMA(14, 50)},
	}

	errCh := make(chan error, len(cfg.Trading.InstIDs))
	for _, instID := range cfg.Trading.InstIDs {
		candleConsumers := make(map[string]port.MarketDataConsumer, len(cfg.PaperTrading.Bars))
		for _, bar := range cfg.PaperTrading.Bars {
			candleConsumers[bar] = stream.NewConsumer(cfg.Redis.Addr, "okx:candles:"+bar, "paper-trader", instID)
		}

		engine := &usecase.PaperTrader{
			InstID:          instID,
			Bars:            cfg.PaperTrading.Bars,
			CandleWindow:    cfg.PaperTrading.CandleLimit,
			Strategies:      strategies,
			Exchange:        restClient,
			TickConsumer:    stream.NewConsumer(cfg.Redis.Addr, "okx:tickers", "paper-trader", instID),
			CandleConsumers: candleConsumers,
			Repo:            repo,
			NotionalUSD:     cfg.PaperTrading.NotionalUSD,
			MaxOpenOrders:   cfg.PaperTrading.MaxOpenOrders,
			Logger:          logger,
		}
		go func() { errCh <- engine.Run(ctx) }()
	}

	<-ctx.Done()
	logger.Info("shutting down paper trader")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
