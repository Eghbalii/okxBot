// Command paper-trader runs the Paper Trading Engine (CLAUDE.md §8): it evaluates strategies
// against live OKX prices and tracks virtual orders through to close, independent of live
// trading. This is the primary source of training data for the RL model.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/rest"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/rlclient"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	if err := cfg.ValidatePaperTradingBars(); err != nil {
		logger.Error("invalid config", "error", err)
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

	if err := strategy.SeedOrigins(ctx, repo); err != nil {
		logger.Error("failed to seed origin strategies", "error", err)
		os.Exit(1)
	}
	if err := ensureDefaultAssignment(ctx, repo, cfg.Trading.InstIDs); err != nil {
		logger.Error("failed to ensure default strategy assignment", "error", err)
		os.Exit(1)
	}

	// CLAUDE.md §15.4: nil unless explicitly enabled, so PaperTrader's SL/TP-adjustment pass is a
	// strict no-op wherever operators haven't opted in — same "additive, never required" posture
	// as the rest of §15's rollout.
	var model port.ModelClient
	if cfg.PaperTrading.RLSLTPAdjust {
		model = rlclient.New(cfg.RLService.URL)
	}

	// One shared Kafka consumer-group reader per topic (tickers + each configured bar), fanned out
	// to each instrument's PaperTrader by instId via kafkastream.Dispatcher — Kafka consumer
	// groups own whole partitions, unlike Redis Streams' per-instrument consumer identity, so the
	// per-instrument routing that used to happen via N separate consumers now happens in-process
	// via one dispatcher per topic (CLAUDE.md §12).
	tickDispatcher := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.tickers", "paper-trader"))
	defer tickDispatcher.Close()
	candleDispatchers := make(map[string]*kafkastream.Dispatcher, len(cfg.PaperTrading.Bars))
	for _, bar := range cfg.PaperTrading.Bars {
		d := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.candles."+bar, "paper-trader"))
		candleDispatchers[bar] = d
		defer d.Close()
	}

	// Paper-order open/close events, for the panel's real-time WebSocket bridge (cmd/api).
	orderEventsPub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.paper-order-events")
	defer orderEventsPub.Close()

	errCh := make(chan error, len(cfg.Trading.InstIDs)+1+len(candleDispatchers))
	for _, instID := range cfg.Trading.InstIDs {
		// Strategy assignments are durable (strategy_assignments table, CLAUDE.md §11.3): loaded
		// fresh from Postgres on every start, so a crash/restart resumes with exactly the same
		// token/timeframe->strategy bindings the panel last configured, not whatever was hardcoded
		// here in Go.
		strategies, err := loadStrategyAssignments(ctx, repo, instID)
		if err != nil {
			logger.Error("failed to load strategy assignments", "instId", instID, "error", err)
			os.Exit(1)
		}

		candleConsumers := make(map[string]port.MarketDataConsumer, len(cfg.PaperTrading.Bars))
		for bar, d := range candleDispatchers {
			candleConsumers[bar] = d.ForInstrument(instID)
		}

		engine := &usecase.PaperTrader{
			InstID:          instID,
			Bars:            cfg.PaperTrading.Bars,
			CandleWindow:    cfg.PaperTrading.CandleLimit,
			Strategies:      strategies,
			Exchange:        restClient,
			TickConsumer:    tickDispatcher.ForInstrument(instID),
			CandleConsumers: candleConsumers,
			Repo:            repo,
			NotionalUSD:     cfg.PaperTrading.NotionalUSD,
			MaxOpenOrders:   cfg.PaperTrading.MaxOpenOrders,
			Logger:          logger,
			Model:           model,
			ActiveTokens:    cfg.Trading.InstIDs,
			TokenBudgetUSD:  cfg.PaperTrading.TokenBudgetUSD,
			OrderEvents:     orderEventsPub,
		}
		go func() { errCh <- engine.Run(ctx) }()
	}

	go func() { errCh <- tickDispatcher.Run(ctx) }()
	for _, d := range candleDispatchers {
		d := d
		go func() { errCh <- d.Run(ctx) }()
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down paper trader")
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			logger.Error("paper trader stopped with error", "error", err)
			os.Exit(1)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ensureDefaultAssignment guarantees at least one strategy is assigned on the "1m" bar for every
// configured instrument, matching the previous hardcoded behavior, so a fresh install still
// trades out of the box. Once the panel (CLAUDE.md §11) is used to manage assignments, this is a
// no-op for any instrument that already has one.
func ensureDefaultAssignment(ctx context.Context, repo *postgres.Repository, instIDs []string) error {
	origins, err := repo.ListStrategies(ctx, "", false)
	if err != nil {
		return err
	}
	var defaultOriginID int64
	for _, s := range origins {
		if s.IsOrigin && s.Kind == "rsi_sma" {
			defaultOriginID = s.ID
			break
		}
	}
	if defaultOriginID == 0 {
		return fmt.Errorf("default origin strategy %q not found after seeding", "rsi_sma")
	}

	for _, instID := range instIDs {
		assignments, err := repo.ListAssignments(ctx, instID, false)
		if err != nil {
			return err
		}
		if len(assignments) > 0 {
			continue
		}
		if _, err := repo.CreateAssignment(ctx, port.StrategyAssignment{
			StrategyID: defaultOriginID,
			InstID:     instID,
			Bar:        "1m",
			Enabled:    true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// loadStrategyAssignments resolves an instrument's durable strategy_assignments rows into live
// usecase.StrategyAssignment values the PaperTrader can run, rebuilding the strategy.Strategy from
// its DB row's Kind+Config every time (CLAUDE.md §11.3) rather than trusting any in-memory cache.
func loadStrategyAssignments(ctx context.Context, repo *postgres.Repository, instID string) ([]usecase.StrategyAssignment, error) {
	rows, err := repo.ListAssignments(ctx, instID, true)
	if err != nil {
		return nil, err
	}
	out := make([]usecase.StrategyAssignment, 0, len(rows))
	for _, a := range rows {
		cfg, err := repo.GetStrategy(ctx, a.StrategyID)
		if err != nil {
			return nil, fmt.Errorf("resolve strategy %d for assignment %d: %w", a.StrategyID, a.ID, err)
		}
		s, err := strategy.FromConfig(cfg.Kind, cfg.Config)
		if err != nil {
			return nil, fmt.Errorf("build strategy %d (kind %q) for assignment %d: %w", a.StrategyID, cfg.Kind, a.ID, err)
		}
		out = append(out, usecase.StrategyAssignment{Bar: a.Bar, Strategy: s, StrategyID: a.StrategyID})
	}
	return out, nil
}
