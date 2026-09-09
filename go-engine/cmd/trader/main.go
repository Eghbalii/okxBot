// Command trader runs the live trading loop. Two implementations are wired here behind
// cfg.Trading.UseConductorLifecycle (CLAUDE.md §27's real-trading plan, commit 8): the original
// flat delta-notional rebalance loop (usecase.Trader), which remains the default, and
// usecase.RealTrader — the strategy-signal + conductor-mediated lifecycle usecase.PaperTrader
// already runs, adapted for real orders. Off by default; both compile and are reachable, main
// picks one per config so the cutover is a deliberate, explicit flag flip, not a code change.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
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
	usecase.TakerFeeRate = cfg.Trading.TakerFeeRate

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

	// Panel control-box config for REAL mode (CLAUDE.md real-trading readiness plan, 2026-09-04) —
	// mirrors cmd/paper-trader/main.go's identical read exactly. Before this, cmd/trader never read
	// paper_trading_config at all, so the Real tab's Pause/Stop/disable-long/disable-short/active-
	// strategies/active-tokens controls appeared to save successfully but had zero effect.
	ptCfg, err := repo.GetPaperTradingConfig(ctx, "real")
	if err != nil {
		logger.Error("failed to load real-mode paper trading config", "error", err)
		os.Exit(1)
	}
	tradingPaused := ptCfg.TradingState != "running"
	if tradingPaused {
		logger.Info("real trading is not in the running state", "tradingState", ptCfg.TradingState)
	}
	// Global per-kind "active strategies" toggle, scoped to mode=real — bulk-applied BEFORE
	// loadRealTraderStrategyAssignments reads them below, mirroring cmd/paper-trader's own
	// sequencing exactly. A no-op when ActiveKinds is empty (no restriction configured).
	// Reuses PaperTrading.Bars/CandleLimit/RLClamps/RLUpdate*/RLEarlyClose/RLMaxOpenDuration —
	// real trading does not need its own separate bar-list or clamp config section (CLAUDE.md §27's
	// plan §2's own note): the decision-vs-context bar split and the clamp bounds are the same
	// question for both engines, and duplicating the config key would just risk the two drifting.
	// active_bars overrides which timeframes strategies DECIDE on, same as PaperTrader's own field.
	decisionBars := cfg.PaperTrading.Bars
	if len(ptCfg.ActiveBars) > 0 {
		decisionBars = ptCfg.ActiveBars
	}
	if len(decisionBars) == 0 {
		logger.Error("paper_trading.bars is empty; RealTrader needs at least one decision bar")
		os.Exit(1)
	}
	if err := repo.SetAssignmentsEnabledForKinds(ctx, "real", ptCfg.ActiveKinds, cfg.Trading.InstIDs, decisionBars); err != nil {
		logger.Error("failed to apply real-mode active-strategy-kinds restriction", "error", err)
		os.Exit(1)
	}
	// trading_state="stopped" force-closes every currently-open real position, once, at startup —
	// same manual-close path and model-reward treatment as the panel's per-order Close button and
	// PaperTrader's own equivalent sweep.
	if ptCfg.TradingState == "stopped" {
		openOnly := true
		for _, instID := range cfg.Trading.InstIDs {
			open, err := repo.ListRealPositions(ctx, port.PositionFilter{InstID: instID, Open: &openOnly})
			if err != nil {
				logger.Error("failed to list open real positions for stopped sweep", "instId", instID, "error", err)
				os.Exit(1)
			}
			for _, o := range open {
				if err := repo.RequestRealManualClose(ctx, o.ID); err != nil {
					logger.Error("failed to request manual close of real order", "id", o.ID, "error", err)
					os.Exit(1)
				}
			}
		}
		logger.Info("real trading_state=stopped: flagged open real positions for close")
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

	// Restart-only HTTP surface (CLAUDE.md real-trading readiness plan, 2026-09-04) — mirrors
	// cmd/paper-trader's own control-box POST /restart, so cmd/api's mode-aware restart proxy has
	// something to forward to for mode=real. See handlers.go for why this is restart-only.
	traderSvc := &traderService{logger: logger}
	traderAddr := envOr("TRADER_ADDR", "0.0.0.0:8095")
	traderHTTPServer := &http.Server{Addr: traderAddr, Handler: traderSvc.routes()}
	go func() {
		logger.Info("serving trader restart api", "addr", traderAddr)
		if err := traderHTTPServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("trader restart api stopped", "error", err)
		}
	}()

	clamps := buildRealTraderClamps(cfg)

	errCh := make(chan error, len(cfg.Trading.InstIDs)+1+len(candleDispatchers))
	const engineStartStagger = 300 * time.Millisecond
	// Kept so the affordability service can push roster changes into engines that are already
	// running, rather than the change only landing at the next restart.
	engines := make(map[string]*usecase.RealTrader, len(cfg.Trading.InstIDs))
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

			// Panel control-box gates for real mode (CLAUDE.md real-trading readiness plan,
			// 2026-09-04) — mirrors cmd/paper-trader's own PaperTrader construction exactly.
			TradingPaused: tradingPaused,
			OpensDisabled: slices.Contains(ptCfg.DisabledInstIDs, instID),
			DisableLong:   ptCfg.DisableLong,
			DisableShort:  ptCfg.DisableShort,

			// ExecInstID/ExecInstType/SettleCcy answer "which instrument does a REAL order actually
			// target" — InstID is now a short internal symbol ("BTC"), never OKX's own wire-format
			// instId (CLAUDE.md §27, 2026-09-04 design: SymbolMap is the one place a short symbol
			// resolves to the real, expiry-dated OKX instId this account can actually trade).
			ExecInstID:   execInstIDFor[instID],
			ExecInstType: cfg.Trading.ExecInstType,
			SettleCcy:    cfg.Trading.ExecSettleCcy,

			AccountInitialUSD:   cfg.Account.InitialUSD,
			SafeMoneyUSD:        cfg.Trading.SafeMoneyUSD,
			MaxLeverage:         cfg.Risk.MaxLeverage,
			MaxPositionPct:      cfg.Account.MaxPositionPct,
			MaxTotalExposurePct: cfg.Account.MaxTotalExposurePct,

			RLUpdatePnLThresholdPct: cfg.PaperTrading.RLUpdatePnLThresholdPct,
			RLUpdateMaxInterval:     cfg.PaperTrading.RLUpdateMaxInterval,
			// NOT cfg.PaperTrading.RLEarlyClose: real trading has its own switch for this one action
			// (2026-09-08 request), so turning early close on for paper research cannot silently turn
			// it on against real capital. Every OTHER RL setting is still shared with paper_trading —
			// see the note above on why that sharing is deliberate.
			RLEarlyClose:    realEarlyCloseAllowed(cfg),
			RLClamps:        clamps,
			MaxOpenDuration: cfg.PaperTrading.RLMaxOpenDuration,

			// CLAUDE.md §27.5: bounds how long a placed order (open or the flattening close order)
			// is given to fill before it's canceled and given up on, no retry/re-price.
			FillTimeout: time.Duration(cfg.FillTimeout.OrderFillTimeoutSec) * time.Second,

			OrderEvents: orderEventsPub,
		}
		engines[instID] = engine
		delay := time.Duration(i) * engineStartStagger
		go func() {
			time.Sleep(delay)
			errCh <- engine.Run(ctx)
		}()
	}

	// Keeps the active roster in step with what the account can actually afford (2026-09-08
	// request): the per-token budget is equity/tokenCount, so profit can make a previously
	// untradeable token affordable and losses can push one out. Runs in-process rather than as a
	// cron job or separate service — it needs the same repository, exchange client, symbol map and
	// caps this binary already has wired, and a separate deployment would duplicate all of it to
	// no benefit.
	//
	// A failure here is logged, never fatal: an unchanged roster is a safe state, and taking the
	// trading loop down because an affordability check could not read a price would be far worse
	// than leaving the roster as it is.
	affordability := &usecase.AffordabilityService{
		Repo:           repo,
		Exchange:       exchangeClient,
		Logger:         logger,
		Mode:           "real",
		AllTokens:      cfg.Trading.InstIDs,
		SymbolMap:      cfg.Trading.SymbolMap,
		ExecInstType:   cfg.Trading.ExecInstType,
		MaxPositionPct: cfg.Account.MaxPositionPct,
		MaxLeverage:    cfg.Risk.MaxLeverage,
		// Push roster changes into the already-running engines so a disable takes effect on the
		// next candle rather than at the next restart.
		OnRosterChange: func(disabled []string) {
			for instID, engine := range engines {
				engine.SetOpensDisabled(slices.Contains(disabled, instID))
			}
		},
	}
	go func() {
		if err := affordability.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("affordability service stopped", "error", err)
		}
	}()

	// Account events pushed by OKX's private WebSocket, republished onto the bus by cmd/okx-gateway
	// (2026-09-09 request: keep positions synced with the exchange, ideally by socket rather than
	// polling). Each event names an instrument whose position/order state just changed on the
	// exchange; the response is to reconcile THAT instrument immediately, which is the same code
	// the periodic poll runs — one definition of how this system responds to a position change,
	// reached faster.
	//
	// The 5-second reconciliation poll keeps running underneath: a socket can be connected and
	// silently stale, and this whole change exists because a component being up is not evidence it
	// is working.
	accountConsumer := kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.account-events", "trader")
	defer accountConsumer.Close()
	go func() {
		errCh <- accountConsumer.Run(ctx, func(ctx context.Context, data []byte) error {
			var event struct {
				Channel string `json:"channel"`
				InstID  string `json:"instId"`
			}
			if err := json.Unmarshal(data, &event); err != nil {
				logger.Warn("could not decode account event", "error", err)
				return nil
			}
			engine, ok := engines[event.InstID]
			if !ok {
				// An event for an instrument this process does not trade — another roster, or an
				// instrument disabled since. Nothing to reconcile, and not an error.
				return nil
			}
			logger.Info("exchange pushed an account change; reconciling now",
				"channel", event.Channel, "instId", event.InstID)
			engine.ReconcileNow(ctx, logger)
			return nil
		})
	}()

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
// realEarlyCloseAllowed reads real trading's OWN early-close switch, deliberately NOT
// paper_trading.rl_early_close (2026-09-08 request). Extracted as a named function rather than
// left as a field read inside main()'s struct literal for the same reason buildRealTraderClamps
// was: a value buried in a large literal is exactly what got silently dropped in the incident that
// left every real position uncapped, so the mapping gets a test that fails if it ever points back
// at the paper flag.
func realEarlyCloseAllowed(cfg *config.Config) bool {
	return cfg.Trading.AllowRLEarlyClose
}

func buildRealTraderClamps(cfg *config.Config) conductor.Clamps {
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

// loadRealTraderStrategyAssignments mirrors cmd/paper-trader/main.go's loadStrategyAssignments —
// resolves durable strategy_assignments rows into live usecase.StrategyAssignment values.
func loadRealTraderStrategyAssignments(ctx context.Context, repo *postgres.Repository, instID string) ([]usecase.StrategyAssignment, error) {
	rows, err := repo.ListAssignments(ctx, instID, true, "real")
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
