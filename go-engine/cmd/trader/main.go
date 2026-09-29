// Command trader runs the live trading loop. Two implementations are wired here behind
// cfg.Trading.UseConductorLifecycle (CLAUDE.md §27's real-trading plan, commit 8): the original
// flat delta-notional rebalance loop (usecase.Trader), which remains the default, and
// usecase.BotTrader — the strategy-signal + conductor-mediated lifecycle usecase.PaperTrader
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
	"sort"
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

// btcReferenceSymbol is the symbol BTC's candles are stored under (see cmd/paper-trader).
const btcReferenceSymbol = "BTC"

// tokenStatsRefresh is how often the roster-wide token figures are reloaded.
const tokenStatsRefresh = time.Hour

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
	// gates the auto-reset behavior: "bot" is never auto-topped-up when drained (CLAUDE.md §15.7).
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
	mode := "bot"
	if health.Simulated {
		mode = "demo"
	}

	// CLAUDE.md §15.7/§15.6: real money is a deliberate step, taken only after paper (and ideally
	// demo) show a real win rate. Require it to be stated outright rather than reached by leaving
	// OKX_SIMULATED_TRADING (on the gateway) unset — an unset env var is the single easiest way to
	// end up live by accident, and everything else in this stack defaults to simulated.
	if mode == "bot" && !cfg.Trading.AllowRealMoney {
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
	// BotTrader — it needs strategy assignments, candle history, and open-position bookkeeping the
	// same way cmd/paper-trader does, none of which have a "keep trading without it" fallback.
	var repo port.Repository
	pgRepo, pgErr := postgres.New(ctx, cfg.Postgres.DSN)
	if pgErr != nil {
		if cfg.Trading.UseConductorLifecycle {
			logger.Error("failed to connect to postgres; BotTrader cannot run without it", "error", pgErr)
			os.Exit(1)
		}
		logger.Warn("equity timeline disabled: could not connect to postgres", "error", pgErr)
	} else {
		defer pgRepo.Close()
		if err := pgRepo.Migrate(ctx); err != nil {
			if cfg.Trading.UseConductorLifecycle {
				logger.Error("migrations failed; BotTrader cannot run without them", "error", err)
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
		runBotTrader(ctx, logger, cfg, exchangeClient, rlClient, riskManager, pgRepo, mode)
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

// runBotTrader builds and runs one usecase.BotTrader per configured instrument, mirroring
// cmd/paper-trader/main.go's Kafka-dispatcher and strategy-assignment wiring (CLAUDE.md §27's plan,
// commit 8) — the shape BotTrader was designed against. repo must be non-nil; the caller already
// enforces this since BotTrader has no "run without a database" fallback.
func runBotTrader(
	ctx context.Context,
	logger *slog.Logger,
	cfg *config.Config,
	exchangeClient port.ExchangeClient,
	rlClient port.ModelClient,
	riskManager *risk.Manager,
	repo *postgres.Repository,
	mode string,
) {
	if err := strategy.SeedOrigins(ctx, repo); err != nil {
		logger.Error("failed to seed origin strategies", "error", err)
		os.Exit(1)
	}

	// Panel control-box config for REAL mode (CLAUDE.md real-trading readiness plan, 2026-09-04) —
	// mirrors cmd/paper-trader/main.go's identical read exactly. Before this, cmd/trader never read
	// paper_trading_config at all, so the Real tab's Pause/Stop/disable-long/disable-short/active-
	// strategies/active-tokens controls appeared to save successfully but had zero effect.
	ptCfg, err := repo.GetPaperTradingConfig(ctx, "bot", "")
	if err != nil {
		logger.Error("failed to load real-mode paper trading config", "error", err)
		os.Exit(1)
	}
	tradingPaused := ptCfg.TradingState != "running"
	if tradingPaused {
		logger.Info("real trading is not in the running state", "tradingState", ptCfg.TradingState)
	}
	// Reuses PaperTrading.Bars/CandleLimit/RLClamps/RLUpdate*/RLMaxOpenDuration —
	// real trading does not need its own separate bar-list or clamp config section (CLAUDE.md §27's
	// plan §2's own note): the decision-vs-context bar split and the clamp bounds are the same
	// question for both engines, and duplicating the config key would just risk the two drifting.
	// active_bars overrides which timeframes strategies DECIDE on, same as PaperTrader's own field.
	decisionBars := cfg.PaperTrading.Bars
	if len(ptCfg.ActiveBars) > 0 {
		decisionBars = ptCfg.ActiveBars
	}
	if len(decisionBars) == 0 {
		logger.Error("paper_trading.bars is empty; BotTrader needs at least one decision bar")
		os.Exit(1)
	}
	// The instrument roster is no longer trading.inst_ids from config.yaml (2026-09-29, second
	// request the same day): the first cut of this change still built the roster from the fixed
	// 10-symbol config list, which meant a token the optimizer promoted for a NEW instrument (one
	// outside that hand-maintained list) got a mode="bot" strategy_assignments row that was never
	// actually loaded — silently unreachable. The roster is now every DISTINCT inst_id with at
	// least one enabled mode="bot" assignment, discovered fresh from the database at startup —
	// mirroring cmd/paper-trader's own DB-backed roster (RosterFor) instead of a config-file list.
	//
	// BotTrader no longer consults its own per-kind ("active strategies") or per-token ("Manage
	// Tokens" disabled_inst_ids) enable toggles at all, and now not even trading.inst_ids itself —
	// it trades EXACTLY what the strategy-optimizer has promoted, mirrored into a mode="bot"
	// strategy_assignments row at promotion time (internal/optimizer/promote.go). Both panel
	// controls (ptCfg.ActiveKinds, ptCfg.DisabledInstIDs) are still read/saved for the paper-mode
	// UI's own use but are deliberately NOT applied here — loadBotTraderStrategyAssignments below
	// is the sole source of truth for which (strategy, inst, bar) combinations BotTrader runs, and
	// botAssignedInstIDs below is the sole source of truth for WHICH TOKENS get an engine at all.
	instIDs, err := botAssignedInstIDs(ctx, repo)
	if err != nil {
		logger.Error("failed to determine bot-mode instrument roster from strategy_assignments", "error", err)
		os.Exit(1)
	}
	if len(instIDs) == 0 {
		logger.Error("no enabled mode=bot strategy_assignments — nothing for real trading to run; " +
			"has the strategy-optimizer promoted anything yet?")
		os.Exit(1)
	}
	// Each new token needs OKX's own wire-format instId to actually place a real order — resolved
	// from the SAME instruments.exec_inst_id column the ingestor/paper-trader roster already
	// populates from OKX's public GetAllTickers scan (internal/usecase/market_scan.go), which is
	// already the X-Perp FUTURES product this account can trade (confirmed live, CLAUDE.md §4: this
	// account's classic SWAP instruments report maxBuy=maxSell=0). This replaces
	// trading.symbol_map's hand-maintained 10-entry map as the source for any token beyond those
	// original 10 — the map itself is left in cfg for backward compatibility/override but is no
	// longer required to cover every real-trading token.
	execInstIDFor, missingExecID, err := resolveBotExecInstIDs(ctx, repo, instIDs, cfg.Trading.SymbolMap)
	if err != nil {
		logger.Error("failed to resolve real-trading instrument ids", "error", err)
		os.Exit(1)
	}
	if len(missingExecID) > 0 {
		// Not fatal: every OTHER token still trades. A token with no known exec_inst_id has never
		// been seen by the ingestor's public scan yet (or the scan hasn't run since it was assigned)
		// — it is dropped from THIS run's roster and logged loudly rather than reaching an order
		// placement call with an empty instId.
		logger.Error("dropping bot-mode tokens with no resolvable OKX instrument id (not on the ingestor's roster yet)",
			"tokens", missingExecID)
		filtered := instIDs[:0]
		for _, id := range instIDs {
			if _, ok := execInstIDFor[id]; ok {
				filtered = append(filtered, id)
			}
		}
		instIDs = filtered
	}
	if len(instIDs) == 0 {
		logger.Error("every bot-mode token was dropped for a missing exec_inst_id — nothing to trade")
		os.Exit(1)
	}
	// trading_state="stopped" force-closes every currently-open real position, once, at startup —
	// same manual-close path and model-reward treatment as the panel's per-order Close button and
	// PaperTrader's own equivalent sweep.
	if ptCfg.TradingState == "stopped" {
		openOnly := true
		for _, instID := range instIDs {
			open, err := repo.ListBotPositions(ctx, port.PositionFilter{InstID: instID, Open: &openOnly})
			if err != nil {
				logger.Error("failed to list open bot positions for stopped sweep", "instId", instID, "error", err)
				os.Exit(1)
			}
			for _, o := range open {
				if err := repo.RequestBotManualClose(ctx, o.ID); err != nil {
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

	// The market-wide reference block (docs/RL_V8_PLAN.md) — see cmd/paper-trader for why it is
	// separate from the per-instrument engines and independent of the traded roster. Its own
	// consumer registration per decision bar.
	btcRef := &usecase.BTCReference{
		InstID:       btcReferenceSymbol,
		Bars:         decisionBars,
		CandleWindow: cfg.PaperTrading.CandleLimit,
		Repo:         repo,
		Consumers:    make(map[string]port.MarketDataConsumer, len(decisionBars)),
	}
	for _, bar := range decisionBars {
		d, ok := candleDispatchers[bar]
		if !ok {
			logger.Error("btc reference: no candle dispatcher for decision bar", "bar", bar)
			os.Exit(1)
		}
		btcRef.Consumers[bar] = d.ForInstrument(btcReferenceSymbol)
	}

	// The roster-wide half of the token profile, refreshed on the discovery scan's own cadence.
	tokenStats := &usecase.TokenStatsCache{Repo: repo, Refresh: tokenStatsRefresh}

	orderEventsPub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.paper-order-events")
	defer orderEventsPub.Close()

	// Restart-only HTTP surface (CLAUDE.md real-trading readiness plan, 2026-09-04) — mirrors
	// cmd/paper-trader's own control-box POST /restart, so cmd/api's mode-aware restart proxy has
	// something to forward to for mode=bot. See handlers.go for why this is restart-only.
	traderSvc := &traderService{logger: logger}
	traderAddr := envOr("TRADER_ADDR", "0.0.0.0:8095")
	traderHTTPServer := &http.Server{Addr: traderAddr, Handler: traderSvc.routes()}
	go func() {
		logger.Info("serving trader restart api", "addr", traderAddr)
		if err := traderHTTPServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("trader restart api stopped", "error", err)
		}
	}()

	clamps := buildBotTraderClamps(cfg)

	errCh := make(chan error, len(instIDs)+1+len(candleDispatchers))
	const engineStartStagger = 300 * time.Millisecond
	// Kept so the affordability service can push roster changes into engines that are already
	// running, rather than the change only landing at the next restart.
	engines := make(map[string]*usecase.BotTrader, len(instIDs))
	for i, instID := range instIDs {
		strategies, err := loadBotTraderStrategyAssignments(ctx, repo, instID, logger)
		if err != nil {
			logger.Error("failed to load strategy assignments", "instId", instID, "error", err)
			os.Exit(1)
		}

		candleConsumers := make(map[string]port.MarketDataConsumer, len(candleBars))
		for bar, d := range candleDispatchers {
			candleConsumers[bar] = d.ForInstrument(instID)
		}

		engine := &usecase.BotTrader{
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
			ActiveTokens:    instIDs,
			// v8 observation inputs (docs/RL_V8_PLAN.md). BTCCandles is NOT optional: nil makes
			// buildObservation fail, which skips every model call — deliberately, since a zeroed
			// BTC block would read as "BTC is flat and uncorrelated" rather than as missing data.
			BTCCandles: btcRef.Window,
			TokenStats: tokenStats.For,

			// Panel control-box gates for real mode (CLAUDE.md real-trading readiness plan,
			// 2026-09-04) — mirrors cmd/paper-trader's own PaperTrader construction exactly.
			// NOT ptCfg.DisabledInstIDs (2026-09-29): the "Manage Tokens" per-token disable checkbox
			// is one of the enable/disable flags BotTrader now deliberately ignores — see the note
			// above loadBotTraderStrategyAssignments. Which tokens actually trade is entirely decided
			// by which mode="bot" strategy_assignments rows the optimizer has promoted.
			TradingPaused: tradingPaused,
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
			// Hardcoded false, not cfg-driven (2026-09-29 request): BotTrader never allows the model
			// to close a position early — paper trading's own RLEarlyClose flag is untouched and
			// still config-driven for research.
			RLEarlyClose:    false,
			RLClamps:        clamps,
			MaxOpenDuration: cfg.PaperTrading.RLMaxOpenDuration,

			// CLAUDE.md §27.5: bounds how long a placed order (open or the flattening close order)
			// is given to fill before it's canceled and given up on, no retry/re-price.
			FillTimeout: time.Duration(cfg.FillTimeout.OrderFillTimeoutSec) * time.Second,

			OrderEvents: orderEventsPub,

			// The reconciliation poll is driven once for the whole roster by reconcileDriver below,
			// not per engine (2026-09-10) — GetPositions/GetBalance are account-wide, so a
			// per-engine poll multiplied one call by the roster size and hit OKX's rate limit.
			ReconciledExternally: true,
		}
		engines[instID] = engine
		delay := time.Duration(i) * engineStartStagger
		go func() {
			time.Sleep(delay)
			errCh <- engine.Run(ctx)
		}()
	}

	// Manual/discretionary trading (docs/MANUAL_TRADE_PLAN.md): one account-wide ManualTrader,
	// unlike BotTrader which is one-per-configured-instrument — a manual order can be placed on
	// ANY token the operator picks from a live search, not just the pre-configured roster.
	manualTrader := &usecase.ManualTrader{
		Repo:         repo,
		Exchange:     exchangeClient,
		Logger:       logger,
		ExecInstType: cfg.Trading.ExecInstType,
		SettleCcy:    cfg.Trading.ExecSettleCcy,
		TdMode:       cfg.Trading.TdMode,
		PosMode:      cfg.Trading.PosMode,
		// 2026-09-29: resolves against trading.symbol_map FIRST, then the instruments table's own
		// exec_inst_id (the same X-Perp id the ingestor's public scan already populates) — mirrors
		// resolveBotExecInstIDs's fallback order, but resolved live per call rather than once at
		// startup, since a manual order can target ANY token the operator picks from a live search,
		// not just BotTrader's own pre-resolved roster.
		ExecInstIDFor: func(symbol string) (string, error) {
			if v, ok := cfg.Trading.SymbolMap[symbol]; ok && v != "" {
				return v, nil
			}
			in, err := repo.ListInstruments(ctx, port.InstrumentFilter{Exchange: "okx"})
			if err != nil {
				return "", fmt.Errorf("resolve %q: list instruments: %w", symbol, err)
			}
			for _, i := range in {
				if i.Symbol == symbol && i.ExecInstID != "" {
					return i.ExecInstID, nil
				}
			}
			return "", fmt.Errorf("no OKX instrument id known for symbol %q", symbol)
		},
		FillTimeout:       time.Duration(cfg.FillTimeout.OrderFillTimeoutSec) * time.Second,
		OrderEvents:       orderEventsPub,
		AccountInitialUSD: cfg.Account.InitialUSD,
		// §8.4: a manual order and a strategy position can share ONE net exchange position, and
		// OKX's conditional orders for a position don't stack cleanly — so before placing its own
		// protective order, ManualTrader checks whether BotTrader already holds a live one on this
		// token. A pure in-memory/DB check against the already-running engines map, no extra
		// exchange call: BotTrader itself is the source of truth for whether IT protects a token,
		// via the algo order id it already recorded when it opened.
		BotTraderProtects: func(instID string) bool {
			if _, ok := engines[instID]; !ok {
				// Not a token BotTrader watches at all — nothing it could be protecting.
				return false
			}
			open := true
			positions, err := repo.ListBotPositions(ctx, port.PositionFilter{InstID: instID, Open: &open})
			if err != nil {
				logger.Warn("manual trader: could not check for an existing strategy position", "instId", instID, "error", err)
				return false
			}
			for _, p := range positions {
				if p.ExchangeAlgoOrderID != nil && *p.ExchangeAlgoOrderID != "" {
					return true
				}
			}
			return false
		},
	}
	go func() {
		errCh <- manualTrader.Run(ctx)
	}()

	// Keeps ManualTrader's position-mode honest with the account's ACTUAL current mode (2026-09-19
	// Trade page fixes: POST /api/manual/account-mode can switch it live from the panel), on a slow
	// cadence independent of Run's own fast intent-polling loop — position mode rarely changes, and
	// polling it every 1-2s alongside intents would waste rate-limit budget on a value that's almost
	// always unchanged. An initial call before the ticker starts means a fresh process picks up
	// whatever the account's mode already is rather than waiting up to a full interval.
	manualTrader.RefreshPosMode(ctx)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				manualTrader.RefreshPosMode(ctx)
			}
		}
	}()

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
		Repo:     repo,
		Exchange: exchangeClient,
		Logger:   logger,
		Mode:     "bot",
		// The full bot-mode ASSIGNED set (2026-09-29: was cfg.Trading.InstIDs, the fixed 10-token
		// config list; now instIDs, the dynamic roster botAssignedInstIDs just resolved from
		// strategy_assignments) — this service decides which tokens are affordable and re-enables
		// one that has become affordable again, so handing it only the already-enabled subset would
		// leave it unable to ever restore a token it disabled itself.
		AllTokens:      instIDs,
		Symbols:        dbBackedSymbolResolver{repo: repo, ctx: ctx, symbolMap: cfg.Trading.SymbolMap},
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

	// One reconciliation poll for the whole roster, replacing the per-engine loop each BotTrader
	// used to run (2026-09-10). GetPositions and GetBalance are ACCOUNT-wide — they take no
	// instrument and returned an identical response to all 10 engines — so the old shape issued 20
	// calls per cycle to learn what 2 calls carry, and OKX rate-limited it (CLAUDE.md §38.2).
	// Per-position work (verifying each protective order via GetAlgoOrder) is genuinely
	// per-instrument and still runs inside each engine.
	reconcileDriver := &usecase.ReconcileDriver{
		Engines:      engines,
		ManualTrader: manualTrader,
		Exchange:     exchangeClient,
		Logger:       logger,
		InstType:     cfg.Trading.ExecInstType,
		SettleCcy:    cfg.Trading.ExecSettleCcy,
	}
	go func() { errCh <- reconcileDriver.Run(ctx) }()

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
			logger.Info("exchange pushed an account change; reconciling now",
				"channel", event.Channel, "instId", event.InstID)
			// Returns false for an instrument this process does not trade — another roster, or one
			// disabled since. Nothing to reconcile, and not an error.
			reconcileDriver.ReconcileInstrument(ctx, event.InstID)
			return nil
		})
	}()

	go func() { errCh <- btcRef.Run(ctx, logger) }()
	go func() { errCh <- tokenStats.Run(ctx, logger) }()
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

// buildBotTraderClamps mirrors cmd/paper-trader/main.go's buildRLClamps exactly (same "a field
// silently dropped from a large inline struct literal" incident that fix guards against, CLAUDE.md
// §23) — a separate copy rather than an import specifically so a future field added to one config
// section doesn't silently also need to change the other's caller; both map their own
// cfg.*.RLClamps into conductor.Clamps field-by-field.
func buildBotTraderClamps(cfg *config.Config) conductor.Clamps {
	return conductor.Clamps{
		MinSLDistPct: cfg.PaperTrading.RLClamps.MinSLDistPct,
		MaxSLDistPct: cfg.PaperTrading.RLClamps.MaxSLDistPct,
		MaxLossPct:   cfg.PaperTrading.RLClamps.MaxLossPct,
		MinTPSLRatio: cfg.PaperTrading.RLClamps.MinTPSLRatio,
		MaxTPSLRatio: cfg.PaperTrading.RLClamps.MaxTPSLRatio,
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadBotTraderStrategyAssignments mirrors cmd/paper-trader/main.go's loadStrategyAssignments —
// resolves durable strategy_assignments rows into live usecase.StrategyAssignment values.
func loadBotTraderStrategyAssignments(ctx context.Context, repo *postgres.Repository, instID string, logger *slog.Logger) ([]usecase.StrategyAssignment, error) {
	rows, err := repo.ListAssignments(ctx, instID, true, "bot", "")
	if err != nil {
		return nil, err
	}
	out := make([]usecase.StrategyAssignment, 0, len(rows))
	skipped := 0
	for _, a := range rows {
		sc, err := repo.GetStrategy(ctx, a.StrategyID)
		if err != nil {
			return nil, fmt.Errorf("resolve strategy %d for assignment %d: %w", a.StrategyID, a.ID, err)
		}
		s, ok := buildAssignmentStrategy(sc.Kind, sc.Config, logger, instID, a.ID, a.StrategyID)
		if !ok {
			skipped++
			continue
		}
		out = append(out, usecase.StrategyAssignment{Bar: a.Bar, Strategy: s, StrategyID: a.StrategyID, Kind: sc.Kind})
	}
	if skipped > 0 {
		// Surfaced as its own line so the count is visible even when the per-row errors have
		// scrolled away: "3 of 30 strategies are not running" is the operationally useful fact.
		logger.Error("some strategy assignments could not be built and will not trade",
			"instId", instID, "skipped", skipped, "loaded", len(out))
	}
	return out, nil
}

// dbBackedSymbolResolver implements port.SymbolResolver by checking trading.symbol_map first (so
// a hand-corrected entry always wins, e.g. ahead of a contract roll the ingestor's scan hasn't
// caught up to yet), then falling back to the instruments table's own exec_inst_id — the same
// X-Perp id the ingestor's public OKX scan populates for every discovered token
// (internal/usecase/market_scan.go), confirmed live to be this account's tradeable product family
// (CLAUDE.md §4). Used by AffordabilityService so afford-ability checks aren't limited to the
// original hand-maintained 10-token symbol_map (2026-09-29, same request as botAssignedInstIDs).
type dbBackedSymbolResolver struct {
	repo      *postgres.Repository
	ctx       context.Context
	symbolMap map[string]string
}

func (r dbBackedSymbolResolver) Resolve(symbol string) (string, error) {
	if v, ok := r.symbolMap[symbol]; ok && v != "" {
		return v, nil
	}
	instruments, err := r.repo.ListInstruments(r.ctx, port.InstrumentFilter{Exchange: "okx"})
	if err != nil {
		return "", fmt.Errorf("resolve %q: list instruments: %w", symbol, err)
	}
	for _, in := range instruments {
		if in.Symbol == symbol && in.ExecInstID != "" {
			return in.ExecInstID, nil
		}
	}
	return "", fmt.Errorf("no OKX instrument id known for symbol %q", symbol)
}

func (r dbBackedSymbolResolver) ResolveAll(symbols []string) ([]string, error) {
	out := make([]string, len(symbols))
	for i, s := range symbols {
		v, err := r.Resolve(s)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// botAssignedInstIDs returns every DISTINCT inst_id with at least one enabled mode="bot"
// strategy_assignments row, sorted for a stable startup log/engine-start order (2026-09-29,
// mirroring cmd/paper-trader's own RosterFor which reads from the instruments table rather than
// config — BotTrader's roster comes from strategy_assignments instead, since a bot-mode row is
// itself what "the optimizer activated this token for real trading" means; see
// internal/optimizer/promote.go). instID="" in ListAssignments returns every instrument's rows.
func botAssignedInstIDs(ctx context.Context, repo *postgres.Repository) ([]string, error) {
	rows, err := repo.ListAssignments(ctx, "", true, "bot", "")
	if err != nil {
		return nil, fmt.Errorf("list bot-mode assignments: %w", err)
	}
	seen := make(map[string]bool, len(rows))
	var out []string
	for _, a := range rows {
		if !seen[a.InstID] {
			seen[a.InstID] = true
			out = append(out, a.InstID)
		}
	}
	sort.Strings(out)
	return out, nil
}

// resolveBotExecInstIDs resolves each of instIDs to OKX's own wire-format instrument id, needed to
// actually place a real order. Checks trading.symbol_map FIRST (2026-09-29: the original 10-entry
// hand-maintained map stays authoritative where it exists, e.g. if it's ever hand-corrected ahead
// of the ingestor's own scan picking up a contract roll), falling back to the instruments table's
// own exec_inst_id — the SAME X-Perp FUTURES id the ingestor's public OKX scan already populates
// for every token it discovers (internal/usecase/market_scan.go), confirmed live to be this
// account's tradeable product family (CLAUDE.md §4: classic SWAP instruments report
// maxBuy=maxSell=0 on this account). A token resolvable by NEITHER is reported in missing rather
// than failing the whole roster — one token with no known instrument id must not stop every other
// already-tradeable token from starting.
func resolveBotExecInstIDs(ctx context.Context, repo *postgres.Repository, instIDs []string, symbolMap map[string]string) (execInstIDFor map[string]string, missing []string, err error) {
	execInstIDFor = make(map[string]string, len(instIDs))
	var unresolvedBySymbolMap []string
	for _, id := range instIDs {
		if v, ok := symbolMap[id]; ok && v != "" {
			execInstIDFor[id] = v
			continue
		}
		unresolvedBySymbolMap = append(unresolvedBySymbolMap, id)
	}
	if len(unresolvedBySymbolMap) == 0 {
		return execInstIDFor, nil, nil
	}
	instruments, err := repo.ListInstruments(ctx, port.InstrumentFilter{Exchange: "okx"})
	if err != nil {
		return nil, nil, fmt.Errorf("list instruments to resolve real-trading exec ids: %w", err)
	}
	bySymbol := make(map[string]string, len(instruments))
	for _, in := range instruments {
		if in.ExecInstID != "" {
			bySymbol[in.Symbol] = in.ExecInstID
		}
	}
	for _, id := range unresolvedBySymbolMap {
		if v, ok := bySymbol[id]; ok {
			execInstIDFor[id] = v
		} else {
			missing = append(missing, id)
		}
	}
	return execInstIDFor, missing, nil
}
