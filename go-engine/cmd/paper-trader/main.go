// Command paper-trader runs the Paper Trading Engine (CLAUDE.md §8): it evaluates strategies
// against live OKX prices and tracks virtual orders through to close, independent of live
// trading. This is the primary source of training data for the RL model.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/gatewayclient"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/rlclient"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
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
	usecase.TakerFeeRate = cfg.Trading.TakerFeeRate

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

	logger.Info("starting paper trader", "instIds", cfg.Trading.InstIDs)

	// Funding-rate poller (CLAUDE.md, 2026-09-06): keeps funding_rates current so realizedPnL's
	// funding-cost lookup has real, per-instrument, per-period data rather than a fixed config
	// constant. paper-trader is the right home for this despite having no other OKX dependency —
	// it is the sole owner of the closed-position PnL calculation that actually consumes this data.
	go runFundingRatePoller(
		ctx,
		gatewayclient.New(cfg.Gateway.URL, "paper-trader"),
		repo,
		cfg.Trading.SymbolMap,
		cfg.Trading.InstIDs,
		cfg.FundingRate.PollInterval,
		logger,
	)

	// Panel control-box config (CLAUDE.md): read fresh from Postgres at every start, same
	// crash-recovery posture as loadStrategyAssignments below — a restart resumes with exactly the
	// pause/stop/direction/kind/token/bar restrictions the panel last saved, not whatever was true
	// in memory before the process last exited.
	ptCfg, err := repo.GetPaperTradingConfig(ctx, "paper")
	if err != nil {
		logger.Error("failed to load paper trading config", "error", err)
		os.Exit(1)
	}
	// active_bars overrides which timeframes strategies actually DECIDE on (paper_trading.bars),
	// not which the ingestor collects/this process consumes from Kafka (ingestion.bars) — a bar
	// removed here just stops triggering evaluateStrategies, it keeps being persisted for context.
	paperTradingBars := cfg.PaperTrading.Bars
	if len(ptCfg.ActiveBars) > 0 {
		paperTradingBars = ptCfg.ActiveBars
	}
	// disabled_inst_ids never removes a token's PaperTrader goroutine (monitorOpenOrders must keep
	// closing its existing positions normally) — it's applied per-instrument below via
	// PaperTrader.OpensDisabled instead, so the full configured roster is used here unchanged.
	tradingPaused := ptCfg.TradingState != "running"
	if tradingPaused {
		logger.Info("paper trading is not in the running state", "tradingState", ptCfg.TradingState)
	}

	if err := strategy.SeedOrigins(ctx, repo); err != nil {
		logger.Error("failed to seed origin strategies", "error", err)
		os.Exit(1)
	}
	if err := ensureDefaultAssignment(ctx, repo, cfg.Trading.InstIDs, paperTradingBars); err != nil {
		logger.Error("failed to ensure default strategy assignment", "error", err)
		os.Exit(1)
	}
	// Global per-kind "active strategies" toggle (CLAUDE.md): bulk-applied to strategy_assignments
	// BEFORE loadStrategyAssignments reads them below, so ListAssignments(enabledOnly=true) picks
	// up the result with no change needed to that function. A no-op when ActiveKinds is empty.
	if err := repo.SetAssignmentsEnabledForKinds(ctx, "paper", ptCfg.ActiveKinds, cfg.Trading.InstIDs, paperTradingBars); err != nil {
		logger.Error("failed to apply active-strategy-kinds restriction", "error", err)
		os.Exit(1)
	}
	// trading_state="stopped" force-closes every currently-open paper order, once, at startup —
	// same manual-close path and model-reward treatment as the panel's per-order Close button
	// (CLAUDE.md §20/§15.14). Deliberately a startup sweep, not a continuous poll: "stopped" is a
	// deliberate, infrequent operator action, and every open position closes on its very next tick
	// regardless of how this flag was applied.
	if ptCfg.TradingState == "stopped" {
		n, err := repo.RequestManualCloseAll(ctx)
		if err != nil {
			logger.Error("failed to request manual close of all open orders", "error", err)
			os.Exit(1)
		}
		logger.Info("trading_state=stopped: flagged open orders for close", "count", n)
	}

	// Panel control-box HTTP surface (CLAUDE.md) — GET/PUT /config + POST /restart, mirroring
	// cmd/strategy-tester's own pattern. GET /config reflects ptCfg as loaded at THIS startup, not
	// a live DB round-trip, same asymmetry as the tester (a save is only "live" after a restart).
	ptSvc := &paperTraderService{repo: repo, logger: logger, current: ptCfg, allInstIDs: cfg.Trading.InstIDs}
	ptAddr := envOr("PAPER_TRADER_ADDR", "0.0.0.0:8093")
	ptHTTPServer := &http.Server{Addr: ptAddr, Handler: ptSvc.routes()}
	go func() {
		logger.Info("serving paper-trader control-box api", "addr", ptAddr)
		if err := ptHTTPServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("paper-trader control-box api stopped", "error", err)
		}
	}()

	// CLAUDE.md §15.4: nil unless explicitly enabled, so PaperTrader's SL/TP-adjustment pass is a
	// strict no-op wherever operators haven't opted in — same "additive, never required" posture
	// as the rest of §15's rollout.
	var model port.ModelClient
	if cfg.PaperTrading.RLSLTPAdjust || cfg.PaperTrading.RLSizing {
		model = rlclient.New(cfg.RLService.URL)
	}

	// One shared Kafka consumer-group reader per topic (tickers + each configured bar), fanned out
	// to each instrument's PaperTrader by instId via kafkastream.Dispatcher — Kafka consumer
	// groups own whole partitions, unlike Redis Streams' per-instrument consumer identity, so the
	// per-instrument routing that used to happen via N separate consumers now happens in-process
	// via one dispatcher per topic (CLAUDE.md §12).
	tickDispatcher := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.tickers", "paper-trader"))
	defer tickDispatcher.Close()
	// Consume EVERY ingested bar, not just the decision bars (CLAUDE.md §9): 4H/1D are collected
	// for higher-timeframe context that a strategy assigned to 5m can consult via
	// MultiTimeframeStrategy. PaperTrader is also the only writer of the candles table, so a bar
	// nobody consumes is never persisted — before this, the ingestor published 4H/1D to Kafka and
	// those messages simply expired unread, leaving the context timeframes permanently empty in
	// the database while the ingestor's logs showed it correctly subscribed to all five channels.
	//
	// Consuming a bar does not make it a decision bar: evaluateStrategies filters assignments by
	// `a.Bar != bar`, so a context-only timeframe with no assignments maintains its window and
	// persists its candles without ever triggering a trade.
	candleBars := cfg.Ingestion.Bars
	if len(candleBars) == 0 {
		candleBars = cfg.PaperTrading.Bars
	}
	candleDispatchers := make(map[string]*kafkastream.Dispatcher, len(candleBars))
	for _, bar := range candleBars {
		d := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.candles."+bar, "paper-trader"))
		candleDispatchers[bar] = d
		defer d.Close()
	}

	// Paper-order open/close events, for the panel's real-time WebSocket bridge (cmd/api).
	orderEventsPub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.paper-order-events")
	defer orderEventsPub.Close()

	// CLAUDE.md §31.2: how many tokens actually open new positions right now — the divisor for
	// dynamic per-position sizing (CurrentEquity / ActiveTokenCount), computed once here from the
	// same roster/disabled-list every PaperTrader instance below shares, so it is identical across
	// all of them despite living as a per-instance field. Recomputed only at startup, matching this
	// service's existing "config changes need a restart" posture (§22) — enabling/disabling a token
	// mid-run doesn't retroactively resize an order already open, only the next one to open.
	activeTokenCount := 0
	for _, instID := range cfg.Trading.InstIDs {
		if !slices.Contains(ptCfg.DisabledInstIDs, instID) {
			activeTokenCount++
		}
	}

	errCh := make(chan error, len(cfg.Trading.InstIDs)+1+len(candleDispatchers))
	// Staggering each instrument's engine start (rather than launching every goroutine in the same
	// instant) spreads out seedCandles()'s REST calls — with 10 instruments x 3 bars, an
	// unstaggered start fires 30 requests within milliseconds and was observed tripping an OKX
	// rate limit reported as instrument-not-found (code 51001) on a different, unpredictable
	// instrument each run. 300ms keeps total startup delay well under a second even at Phase B's
	// 10-token scale.
	const engineStartStagger = 300 * time.Millisecond
	for i, instID := range cfg.Trading.InstIDs {
		// Strategy assignments are durable (strategy_assignments table, CLAUDE.md §11.3): loaded
		// fresh from Postgres on every start, so a crash/restart resumes with exactly the same
		// token/timeframe->strategy bindings the panel last configured, not whatever was hardcoded
		// here in Go.
		strategies, err := loadStrategyAssignments(ctx, repo, instID)
		if err != nil {
			logger.Error("failed to load strategy assignments", "instId", instID, "error", err)
			os.Exit(1)
		}

		candleConsumers := make(map[string]port.MarketDataConsumer, len(candleBars))
		for bar, d := range candleDispatchers {
			candleConsumers[bar] = d.ForInstrument(instID)
		}

		engine := &usecase.PaperTrader{
			InstID: instID,
			// Every ingested bar, so the context timeframes get a maintained window (and are seeded
			// from the database on restart) even though no strategy decides on them.
			Bars:             candleBars,
			CandleWindow:     cfg.PaperTrading.CandleLimit,
			Strategies:       strategies,
			TickConsumer:     tickDispatcher.ForInstrument(instID),
			CandleConsumers:  candleConsumers,
			Repo:             repo,
			ActiveTokenCount: activeTokenCount,
			MaxOpenOrders:    cfg.PaperTrading.MaxOpenOrders,
			Logger:           logger,
			Model:            model,
			RLSLTPAdjust:     cfg.PaperTrading.RLSLTPAdjust,
			RLDecisionBar:    cfg.PaperTrading.RLDecisionBar,
			RLSizing:         cfg.PaperTrading.RLSizing,
			MaxLeverage:      cfg.Risk.MaxLeverage,
			// Signal-lifecycle conductor (CLAUDE.md §15.12): update cadence, early close, and the
			// clamps bounding where the model may place stops/targets.
			RLUpdatePnLThresholdPct: cfg.PaperTrading.RLUpdatePnLThresholdPct,
			RLUpdateMaxInterval:     cfg.PaperTrading.RLUpdateMaxInterval,
			RLEarlyClose:            cfg.PaperTrading.RLEarlyClose,
			RLClamps:                buildRLClamps(cfg),
			// Force-closes a stale position regardless of RL flags (CLAUDE.md §15.14) — unlike
			// everything else in this block, this is unconditional housekeeping, not RL behavior.
			MaxOpenDuration: cfg.PaperTrading.RLMaxOpenDuration,
			ActiveTokens:    cfg.Trading.InstIDs,
			// One shared account across every token (CLAUDE.md §15.6): each per-instrument engine
			// trades against the same "paper" balance row, not a slice of it.
			Mode:                "paper",
			AccountInitialUSD:   cfg.Account.InitialUSD,
			MaxPositionPct:      cfg.Account.MaxPositionPct,
			MaxTotalExposurePct: cfg.Account.MaxTotalExposurePct,
			OrderEvents:         orderEventsPub,
			// Panel control-box gates (CLAUDE.md): TradingPaused is process-wide (running vs.
			// paused/stopped), OpensDisabled is per-token — both only stop NEW opens, existing
			// positions keep closing normally regardless.
			TradingPaused: tradingPaused,
			OpensDisabled: slices.Contains(ptCfg.DisabledInstIDs, instID),
			DisableLong:   ptCfg.DisableLong,
			DisableShort:  ptCfg.DisableShort,
		}
		delay := time.Duration(i) * engineStartStagger
		go func() {
			time.Sleep(delay)
			errCh <- engine.Run(ctx)
		}()
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

// buildRLClamps maps every configured conductor.Clamps field from cfg.PaperTrading.RLClamps.
// Extracted into its own function (2026-09-01 incident fix) specifically so a future field added
// to config.PaperTrading.RLClamps or conductor.Clamps can't be silently left out of a large inline
// struct literal buried inside main() the way MaxLossPct was: this bug meant §19.2's leverage-
// aware 15%-loss cap was never actually applied to a single real paper order — a strategy's raw
// SLPct (leverage-blind) passed straight through MaxSLDistPct alone. Observed in production as
// order 636: 20x leverage, a 5% price-distance stop, meaning a 100% margin loss on touch instead
// of the intended 15% ceiling. TestBuildRLClamps_MapsMaxLossPct below exists so this specific class
// of regression (a field silently dropped from the mapping) fails a test, not a live position.
func buildRLClamps(cfg *config.Config) conductor.Clamps {
	return conductor.Clamps{
		MinSLDistPct: cfg.PaperTrading.RLClamps.MinSLDistPct,
		MaxSLDistPct: cfg.PaperTrading.RLClamps.MaxSLDistPct,
		MaxLossPct:   cfg.PaperTrading.RLClamps.MaxLossPct,
		MinTPSLRatio: cfg.PaperTrading.RLClamps.MinTPSLRatio,
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
func ensureDefaultAssignment(ctx context.Context, repo *postgres.Repository, instIDs []string, paperTradingBars []string) error {
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

	// The default assignment must land on a bar PaperTrader actually evaluates
	// (paper_trading.bars) — a hardcoded "1m" here silently never fires if 1m isn't configured,
	// since the assignment exists but its candle window never gets populated/evaluated.
	if len(paperTradingBars) == 0 {
		return fmt.Errorf("paper_trading.bars is empty; cannot pick a default assignment bar")
	}
	defaultBar := paperTradingBars[0]

	for _, instID := range instIDs {
		assignments, err := repo.ListAssignments(ctx, instID, false, "paper")
		if err != nil {
			return err
		}
		if len(assignments) > 0 {
			continue
		}
		if _, err := repo.CreateAssignment(ctx, port.StrategyAssignment{
			StrategyID: defaultOriginID,
			InstID:     instID,
			Bar:        defaultBar,
			Enabled:    true,
			Mode:       "paper",
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
	rows, err := repo.ListAssignments(ctx, instID, true, "paper")
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
		out = append(out, usecase.StrategyAssignment{Bar: a.Bar, Strategy: s, StrategyID: a.StrategyID, Kind: cfg.Kind})
	}
	return out, nil
}
