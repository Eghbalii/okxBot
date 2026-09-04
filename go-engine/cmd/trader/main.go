// Command trader runs the live trading loop. Two implementations are wired here behind
// cfg.Trading.UseConductorLifecycle (CLAUDE.md §27's real-trading plan, commit 8): the original
// flat delta-notional rebalance loop (usecase.Trader), which remains the default, and
// usecase.RealTrader — the strategy-signal + conductor-mediated lifecycle usecase.PaperTrader
// already runs, adapted for real orders. Off by default; both compile and are reachable, main
// picks one per config so the cutover is a deliberate, explicit flag flip, not a code change.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/gatewayclient"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/risk"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Real trading calls OKX only through cmd/okx-gateway, never rest.Client directly (CLAUDE.md
	// §27.1): the trader is the priority consumer under the gateway's per-endpoint-class rate
	// limiting, and centralizing here is what makes the aggregate OKX-side request rate an
	// enforceable, observable number instead of five independently-limited processes. "trader" is
	// the ONLY consumer identity the gateway treats as priority — this is the one call site in the
	// whole codebase allowed to use it. cmd/trader no longer holds OKX_API_KEY/SECRET/PASSPHRASE at
	// all — only the gateway does — so there is no local credential check to make here anymore.
	exchangeClient := gatewayclient.New(cfg.Gateway.URL, "trader")
	rlClient := rlclient.New(cfg.RLService.URL)

	// Mode selects which account_equity row this process's balance timeline is recorded under, and
	// gates the auto-reset behavior: "real" is never auto-topped-up when drained (CLAUDE.md §15.7).
	//
	// Derived from the GATEWAY's own reported Simulated flag (GET /health), not a local
	// OKX_SIMULATED_TRADING read — since credentials moved to the gateway (§27.1), that's the only
	// place real-vs-demo is actually known. This keeps the "mode must never disagree with the
	// credentials in use" invariant intact across the process boundary: cmd/trader has no
	// credentials of its own to disagree with, so it defers to whichever the gateway is holding.
	health, err := exchangeClient.Health(ctx)
	if err != nil {
		logger.Error("failed to reach okx-gateway for mode detection; refusing to start", "error", err)
		os.Exit(1)
	}
	mode := "real"
	if health.Simulated {
		mode = "demo"
	}

	// CLAUDE.md §15.7/§15.6: real money is a deliberate step, taken only after paper (and ideally
	// demo) show a real win rate. Require it to be stated outright rather than reached by leaving
	// OKX_SIMULATED_TRADING (on the gateway) unset — an unset env var is the single easiest way to
	// end up live by accident, and everything else in this stack defaults to simulated.
	if mode == "real" && !cfg.Trading.AllowRealMoney {
		logger.Error("refusing to start against REAL money: okx-gateway reports simulated=false " +
			"and trading.allow_real_money is false. Set trading.allow_real_money: true only when " +
			"you intend to trade real capital (CLAUDE.md §15.6's paper -> demo -> real progression).")
		os.Exit(1)
	}
	logger.Info("trading mode resolved", "mode", mode, "gatewaySimulated", health.Simulated,
		"useConductorLifecycle", cfg.Trading.UseConductorLifecycle)

	// CLAUDE.md §27, 2026-09-04 design: trading.inst_ids are short internal symbols ("BTC"), never
	// OKX's own wire-format instId — trading.symbol_map is the one place a symbol resolves to the
	// real, possibly account-specific/expiry-dated instId a REAL exchange call actually needs.
	// Resolved once, up front: an unresolvable symbol must stop this process from starting rather
	// than silently placing/canceling/querying against an empty or wrong instId later.
	symbolMap := okx.SymbolMap(cfg.Trading.SymbolMap)
	execInstIDs, err := symbolMap.ResolveAll(cfg.Trading.InstIDs)
	if err != nil {
		logger.Error("failed to resolve trading.inst_ids against trading.symbol_map", "error", err)
		os.Exit(1)
	}
	execInstIDFor := make(map[string]string, len(cfg.Trading.InstIDs))
	for i, sym := range cfg.Trading.InstIDs {
		execInstIDFor[sym] = execInstIDs[i]
	}

	// Postgres is OPTIONAL for the old Trader (it's used only to record the equity timeline for the
	// panel's chart, and a database problem must never stop a live trading loop), but REQUIRED for
	// RealTrader — it needs strategy assignments, candle history, and open-position bookkeeping the
	// same way cmd/paper-trader does, none of which have a "keep trading without it" fallback.
	var repo port.Repository
	pgRepo, pgErr := postgres.New(ctx, cfg.Postgres.DSN)
	if pgErr != nil {
		if cfg.Trading.UseConductorLifecycle {
			logger.Error("failed to connect to postgres; RealTrader cannot run without it", "error", pgErr)
			os.Exit(1)
		}
		logger.Warn("equity timeline disabled: could not connect to postgres", "error", pgErr)
	} else {
		defer pgRepo.Close()
		if err := pgRepo.Migrate(ctx); err != nil {
			if cfg.Trading.UseConductorLifecycle {
				logger.Error("migrations failed; RealTrader cannot run without them", "error", err)
				os.Exit(1)
			}
			logger.Warn("equity timeline: migrations failed", "error", err)
		}
		repo = pgRepo
	}

	settleCcy := cfg.Trading.ExecSettleCcy
	if settleCcy == "" {
		settleCcy = "USDT"
	}
	balances, err := exchangeClient.GetBalance(settleCcy)
	if err != nil {
		logger.Error("failed to fetch initial balance", "error", err)
		os.Exit(1)
	}
	startEquity := decimal.Zero
	if len(balances) > 0 {
		startEquity = balances[0].Eq
	}

	riskLimits := risk.Limits{
		MaxLeverage:             cfg.Risk.MaxLeverage,
		MaxPositionNotionalUSD:  cfg.Risk.MaxPositionNotionalUSD,
		MaxDailyDrawdownPct:     cfg.Risk.MaxDailyDrawdownPct,
		MinLiquidationBufferPct: cfg.Risk.MinLiquidationBufferPct,
	}
	riskManager := risk.NewManager(riskLimits, startEquity)

	if cfg.Trading.UseConductorLifecycle {
		runRealTrader(ctx, logger, cfg, exchangeClient, rlClient, riskManager, pgRepo, mode, execInstIDFor)
		return
	}

	logger.Info("starting trader (flat rebalance loop)", "instIds", cfg.Trading.InstIDs, "gatewaySimulated", health.Simulated)

	// Run one Trader per configured instrument, each polling independently.
	errCh := make(chan error, len(cfg.Trading.InstIDs))
	for _, instID := range cfg.Trading.InstIDs {
		trader := &usecase.Trader{
			InstID:       instID,
			ExecInstID:   execInstIDFor[instID],
			ExecInstType: cfg.Trading.ExecInstType,
			SettleCcy:    cfg.Trading.ExecSettleCcy,
			Exchange:     exchangeClient,
			Model:        rlClient,
			RiskManager:  riskManager,
			PollInterval: time.Duration(cfg.Trading.PollIntervalSec) * time.Second,
			TdMode:       cfg.Trading.TdMode,
			PosMode:      cfg.Trading.PosMode,
			MinOrderUSD:  cfg.Trading.MinOrderUSD,
			Logger:       logger,
			// Same roster the paper-trading path builds observations from (CLAUDE.md §15.1), so
			// live and demo observations match the shape the model was trained on.
			ActiveTokens:      cfg.Trading.InstIDs,
			Mode:              mode,
			AccountInitialUSD: cfg.Account.InitialUSD,
			Repo:              repo,
		}
		go func() { errCh <- trader.Run(ctx) }()
	}

	<-ctx.Done()
	logger.Info("shutting down trader")
}

// runRealTrader builds and runs one usecase.RealTrader per configured instrument, mirroring
// cmd/paper-trader/main.go's Kafka-dispatcher and strategy-assignment wiring (CLAUDE.md §27's plan,
// commit 8) — the shape RealTrader was designed against. repo must be non-nil; the caller already
// enforces this since RealTrader has no "run without a database" fallback.
func runRealTrader(
	ctx context.Context,
	logger *slog.Logger,
	cfg *config.Config,
	exchangeClient port.ExchangeClient,
	rlClient port.ModelClient,
	riskManager *risk.Manager,
	repo *postgres.Repository,
	mode string,
	execInstIDFor map[string]string,
) {
	if err := strategy.SeedOrigins(ctx, repo); err != nil {
		logger.Error("failed to seed origin strategies", "error", err)
		os.Exit(1)
	}

	// Reuses PaperTrading.Bars/CandleLimit/RLClamps/RLUpdate*/RLEarlyClose/RLMaxOpenDuration —
	// real trading does not need its own separate bar-list or clamp config section (CLAUDE.md §27's
	// plan §2's own note): the decision-vs-context bar split and the clamp bounds are the same
	// question for both engines, and duplicating the config key would just risk the two drifting.
	decisionBars := cfg.PaperTrading.Bars
	if len(decisionBars) == 0 {
		logger.Error("paper_trading.bars is empty; RealTrader needs at least one decision bar")
		os.Exit(1)
	}
	candleBars := cfg.Ingestion.Bars
	if len(candleBars) == 0 {
		candleBars = decisionBars
	}

	tickDispatcher := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.tickers", "trader"))
	defer tickDispatcher.Close()
	candleDispatchers := make(map[string]*kafkastream.Dispatcher, len(candleBars))
	for _, bar := range candleBars {
		d := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.candles."+bar, "trader"))
		candleDispatchers[bar] = d
		defer d.Close()
	}

	orderEventsPub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.paper-order-events")
	defer orderEventsPub.Close()

	clamps := buildRealTraderClamps(cfg)

	errCh := make(chan error, len(cfg.Trading.InstIDs)+1+len(candleDispatchers))
	const engineStartStagger = 300 * time.Millisecond
	for i, instID := range cfg.Trading.InstIDs {
		strategies, err := loadRealTraderStrategyAssignments(ctx, repo, instID)
		if err != nil {
			logger.Error("failed to load strategy assignments", "instId", instID, "error", err)
			os.Exit(1)
		}

		candleConsumers := make(map[string]port.MarketDataConsumer, len(candleBars))
		for bar, d := range candleDispatchers {
			candleConsumers[bar] = d.ForInstrument(instID)
		}

		engine := &usecase.RealTrader{
			InstID:          instID,
			Bars:            candleBars,
			CandleWindow:    cfg.PaperTrading.CandleLimit,
			Strategies:      strategies,
			TickConsumer:    tickDispatcher.ForInstrument(instID),
			CandleConsumers: candleConsumers,
			Repo:            repo,
			Exchange:        exchangeClient,
			Model:           rlClient,
			RiskManager:     riskManager,
			Logger:          logger,
			Mode:            mode,
			TdMode:          cfg.Trading.TdMode,
			PosMode:         cfg.Trading.PosMode,
			ActiveTokens:    cfg.Trading.InstIDs,

			// ExecInstID/ExecInstType/SettleCcy answer "which instrument does a REAL order actually
			// target" — InstID is now a short internal symbol ("BTC"), never OKX's own wire-format
			// instId (CLAUDE.md §27, 2026-09-04 design: SymbolMap is the one place a short symbol
			// resolves to the real, expiry-dated OKX instId this account can actually trade).
			ExecInstID:   execInstIDFor[instID],
			ExecInstType: cfg.Trading.ExecInstType,
			SettleCcy:    cfg.Trading.ExecSettleCcy,

			AccountInitialUSD:   cfg.Account.InitialUSD,
			MaxLeverage:         cfg.Risk.MaxLeverage,
			MaxPositionPct:      cfg.Account.MaxPositionPct,
			MaxTotalExposurePct: cfg.Account.MaxTotalExposurePct,

			RLUpdatePnLThresholdPct: cfg.PaperTrading.RLUpdatePnLThresholdPct,
			RLUpdateMaxInterval:     cfg.PaperTrading.RLUpdateMaxInterval,
			RLEarlyClose:            cfg.PaperTrading.RLEarlyClose,
			RLClamps:                clamps,
			MaxOpenDuration:         cfg.PaperTrading.RLMaxOpenDuration,

			// CLAUDE.md §27.5: bounds how long a placed order (open or the flattening close order)
			// is given to fill before it's canceled and given up on, no retry/re-price.
			FillTimeout: time.Duration(cfg.FillTimeout.OrderFillTimeoutSec) * time.Second,

			OrderEvents: orderEventsPub,
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
		logger.Info("shutting down trader")
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			logger.Error("trader stopped with error", "error", err)
			os.Exit(1)
		}
	}
}

// buildRealTraderClamps mirrors cmd/paper-trader/main.go's buildRLClamps exactly (same "a field
// silently dropped from a large inline struct literal" incident that fix guards against, CLAUDE.md
// §23) — a separate copy rather than an import specifically so a future field added to one config
// section doesn't silently also need to change the other's caller; both map their own
// cfg.*.RLClamps into conductor.Clamps field-by-field.
func buildRealTraderClamps(cfg *config.Config) conductor.Clamps {
	return conductor.Clamps{
		MinSLDistPct: cfg.PaperTrading.RLClamps.MinSLDistPct,
		MaxSLDistPct: cfg.PaperTrading.RLClamps.MaxSLDistPct,
		MaxLossPct:   cfg.PaperTrading.RLClamps.MaxLossPct,
		MinTPSLRatio: cfg.PaperTrading.RLClamps.MinTPSLRatio,
	}
}

// loadRealTraderStrategyAssignments mirrors cmd/paper-trader/main.go's loadStrategyAssignments —
// resolves durable strategy_assignments rows into live usecase.StrategyAssignment values.
func loadRealTraderStrategyAssignments(ctx context.Context, repo *postgres.Repository, instID string) ([]usecase.StrategyAssignment, error) {
	rows, err := repo.ListAssignments(ctx, instID, true)
	if err != nil {
		return nil, err
	}
	out := make([]usecase.StrategyAssignment, 0, len(rows))
	for _, a := range rows {
		sc, err := repo.GetStrategy(ctx, a.StrategyID)
		if err != nil {
			return nil, fmt.Errorf("resolve strategy %d for assignment %d: %w", a.StrategyID, a.ID, err)
		}
		s, err := strategy.FromConfig(sc.Kind, sc.Config)
		if err != nil {
			return nil, fmt.Errorf("build strategy %d (kind %q) for assignment %d: %w", a.StrategyID, sc.Kind, a.ID, err)
		}
		out = append(out, usecase.StrategyAssignment{Bar: a.Bar, Strategy: s, StrategyID: a.StrategyID, Kind: sc.Kind})
	}
	return out, nil
}
