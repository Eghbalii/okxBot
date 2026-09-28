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
	"strings"
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

// btcReferenceSymbol is the symbol BTC's candles are stored under. Explicit rather than assumed:
// the symbol changed once already (§33.4's short-symbol migration), and a hardcoded literal buried
// in a struct literal would have gone quietly wrong rather than failing.
const btcReferenceSymbol = "BTC"

// tokenStatsRefresh is how often the roster-wide token figures are reloaded. These change on the
// discovery scan's own cadence (hours, §53), so anything faster is a query for a number that has
// not moved.
const tokenStatsRefresh = time.Hour

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

	// PAPER_EXCHANGE (default "okx") is this PROCESS's own instance identity — running a second,
	// fully independent cmd/paper-trader (e.g. against MEXC) is a second invocation of this same
	// binary with PAPER_EXCHANGE=mexc, its own CONFIG_PATH, and its own GATEWAY_URL, rather than
	// one process juggling two exchanges' state (2026-09-22, mirroring cmd/okx-gateway's own
	// GATEWAY_EXCHANGE pattern, §46.4). Every Exchange-scoped repository call/field below is threaded
	// from this one value.
	exchange := strings.ToLower(envOr("PAPER_EXCHANGE", "okx"))

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

	// The instrument roster comes from the DATABASE, not config.yaml (2026-09-13, migration 000031) —
	// the same source the ingestor reads, so a token the discovery scan admitted starts paper-trading
	// on the next restart rather than needing a config edit. config.yaml's list seeds a database that
	// has never held a roster, so this deploy changes nothing about what is traded until a scan or an
	// operator says otherwise (usecase.RosterFor).
	seedExecIDs, err := usecase.SeedExecIDs(cfg.Trading.SymbolMap, cfg.Trading.InstIDs)
	if err != nil {
		logger.Error("failed to resolve trading.inst_ids against trading.symbol_map", "error", err)
		os.Exit(1)
	}
	roster, err := usecase.RosterFor(ctx, repo, exchange, "paper",
		cfg.Trading.InstIDs, seedExecIDs, cfg.Trading.ExecInstType, logger)
	if err != nil {
		logger.Error("failed to load instrument roster", "error", err)
		os.Exit(1)
	}
	if len(roster.Symbols) == 0 {
		logger.Error("instrument roster is empty for the paper consumer — nothing to trade")
		os.Exit(1)
	}
	instIDs := roster.Symbols

	// Restart when the roster changes, so a scan-admitted token is picked up without a manual
	// redeploy. Same mechanism and reasoning as the ingestor's: each PaperTrader owns one
	// instrument's candle windows and consumer registrations, built once at startup, and a restart
	// re-derives all of it from the database rather than mutating it in place half-way.
	go (&usecase.RosterWatcher{
		Repo: repo, Exchange: exchange, Consumer: "paper",
		Interval: time.Minute, Logger: logger, Baseline: instIDs,
		OnChange: func(reason string) {
			logger.Info("restarting to pick up the new instrument roster", "reason", reason)
			stop()
		},
	}).Run(ctx)

	logger.Info("starting paper trader", "instIds", instIDs)

	// Funding-rate poller (CLAUDE.md, 2026-09-06): keeps funding_rates current so realizedPnL's
	// funding-cost lookup has real, per-instrument, per-period data rather than a fixed config
	// constant. paper-trader is the right home for this despite having no other OKX dependency —
	// it is the sole owner of the closed-position PnL calculation that actually consumes this data.
	go runFundingRatePoller(
		ctx,
		gatewayclient.New(cfg.Gateway.URL, "paper-trader"),
		repo,
		cfg.Trading.SymbolMap,
		instIDs,
		cfg.FundingRate.PollInterval,
		logger,
	)

	// Panel control-box config (CLAUDE.md): read fresh from Postgres at every start, same
	// crash-recovery posture as loadStrategyAssignments below — a restart resumes with exactly the
	// pause/stop/direction/kind/token/bar restrictions the panel last saved, not whatever was true
	// in memory before the process last exited.
	ptCfg, err := repo.GetPaperTradingConfig(ctx, "paper", exchange)
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
	if err := ensureDefaultAssignment(ctx, repo, exchange, instIDs, paperTradingBars); err != nil {
		logger.Error("failed to ensure default strategy assignment", "error", err)
		os.Exit(1)
	}
	// Global per-kind "active strategies" toggle (CLAUDE.md): bulk-applied to strategy_assignments
	// BEFORE loadStrategyAssignments reads them below, so ListAssignments(enabledOnly=true) picks
	// up the result with no change needed to that function. A no-op when ActiveKinds is empty.
	if err := repo.SetAssignmentsEnabledForKinds(ctx, "paper", exchange, ptCfg.ActiveKinds, instIDs, paperTradingBars); err != nil {
		logger.Error("failed to apply active-strategy-kinds restriction", "error", err)
		os.Exit(1)
	}
	// trading_state="stopped" force-closes every currently-open paper order, once, at startup —
	// same manual-close path and model-reward treatment as the panel's per-order Close button
	// (CLAUDE.md §20/§15.14). Deliberately a startup sweep, not a continuous poll: "stopped" is a
	// deliberate, infrequent operator action, and every open position closes on its very next tick
	// regardless of how this flag was applied.
	if ptCfg.TradingState == "stopped" {
		n, err := repo.RequestManualCloseAll(ctx, exchange)
		if err != nil {
			logger.Error("failed to request manual close of all open orders", "error", err)
			os.Exit(1)
		}
		logger.Info("trading_state=stopped: flagged open orders for close", "count", n)
	}

	// Panel control-box HTTP surface (CLAUDE.md) — GET/PUT /config + POST /restart, mirroring
	// cmd/strategy-tester's own pattern. GET /config reflects ptCfg as loaded at THIS startup, not
	// a live DB round-trip, same asymmetry as the tester (a save is only "live" after a restart).
	ptSvc := &paperTraderService{repo: repo, logger: logger, current: ptCfg, allInstIDs: instIDs, exchange: exchange}
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
	//
	// Topic PREFIX derives from cfg.Ingestion.Exchange (INGEST_EXCHANGE env, default "okx") — the
	// same value cmd/ingestor uses to decide which exchange's market data it publishes — NOT from
	// this process's own `exchange` (PAPER_EXCHANGE) variable above. The two answer different
	// questions and must not be conflated: `exchange` is this PROCESS's isolation/comparison label
	// (e.g. "mexc", scoping account_equity/paper_orders/config rows so two experiments never share
	// state), while the topic prefix is which REAL exchange's ingestor populated the Kafka topics
	// this process reads ticks/candles from. For a future config-variant experiment on the SAME
	// exchange (e.g. PAPER_EXCHANGE="mexc_no_early_close") the two would genuinely differ — this is
	// the specific case that motivated keeping them as two separate values rather than one.
	//
	// cmd/ingestor publishes MEXC's own ticks/candles to "mexc.tickers"/"mexc.candles.<bar>" (a
	// second, independent ingestor instance, INGEST_EXCHANGE=mexc); a paper-trader instance whose
	// own config.yaml pairs with that ingestor (config.mexc.yaml, also carrying INGEST_EXCHANGE=mexc
	// so both processes agree) must consume from those topics, never OKX's "okx.tickers". Before
	// this fix, every paper-trader instance read "okx.tickers"/"okx.candles.<bar>" unconditionally
	// regardless of cfg.Ingestion.Exchange — found this session: paper-trader-mexc was silently
	// reading OKX's own candle data out of Postgres via a SEPARATE mechanism (both exchanges using
	// the same short symbols under candles' then-unscoped (inst_id, bar, ts) key, fixed by
	// migration 000038) before this Kafka-topic bug was even reached, since seedCandlesFromRepo runs
	// before any Kafka message is ever consumed.
	topicExchange := cfg.Ingestion.Exchange
	if topicExchange == "" {
		topicExchange = "okx"
	}
	// consumerGroup is scoped by topicExchange, not a bare "paper-trader" (found live 2026-09-22,
	// the same day as the topicExchange fix above): Kafka consumer groups coordinate rebalancing
	// PER GROUP ID, so two independent processes sharing the literal group "paper-trader" — this
	// OKX instance and paper-trader-mexc, even though they subscribe to entirely different topics
	// (okx.* vs mexc.*) — were treated as members of ONE group and kept rebalancing against each
	// other. Measured live: okx.tickers consumer lag climbed from ~86k to ~105k and kept growing
	// the whole time paper-trader-mexc was running, and dropped to near-zero within 30s of
	// stopping it — a real degradation of THIS project's production OKX paper-trading data path,
	// not a cosmetic issue. "okx" keeps the group id "paper-trader" unchanged (the exact string
	// every existing deployment/consumer-lag dashboard already expects); any other exchange gets
	// its own "paper-trader-<exchange>" group, fully independent of OKX's.
	consumerGroup := "paper-trader"
	if topicExchange != "okx" {
		consumerGroup = "paper-trader-" + topicExchange
	}
	tickDispatcher := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, topicExchange+".tickers", consumerGroup))
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
		// Same topicExchange prefix AND consumerGroup as the ticker dispatcher above — see their
		// comments for why each is what it is.
		d := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, topicExchange+".candles."+bar, consumerGroup))
		candleDispatchers[bar] = d
		defer d.Close()
	}

	// The market-wide reference block (docs/RL_V8_PLAN.md). Every other observation input is
	// intra-token, so without this the model cannot see that an altcoin reverses the moment BTC's
	// candle turns red — the operator's own observation and the reason the block exists.
	//
	// Its own consumer group, and its own dispatcher registration per bar, so it works whether or
	// not BTC is in the traded roster: disabling BTC for trading must not silently blind the model
	// about the whole market.
	btcRef := &usecase.BTCReference{
		InstID:       btcReferenceSymbol,
		Exchange:     exchange,
		Bars:         paperTradingBars,
		CandleWindow: cfg.PaperTrading.CandleLimit,
		Repo:         repo,
		Consumers:    make(map[string]port.MarketDataConsumer, len(paperTradingBars)),
	}
	for _, bar := range paperTradingBars {
		d, ok := candleDispatchers[bar]
		if !ok {
			// A decision bar with no ingestion is already a startup error elsewhere
			// (ValidatePaperTradingBars); guard anyway so a future reordering cannot produce a
			// reference that silently never updates.
			logger.Error("btc reference: no candle dispatcher for decision bar", "bar", bar)
			os.Exit(1)
		}
		btcRef.Consumers[bar] = d.ForInstrument(btcReferenceSymbol)
	}

	// The roster-wide half of the token profile (volume, rank, 24h figures), refreshed on the
	// discovery scan's own cadence rather than queried per decision.
	tokenStats := &usecase.TokenStatsCache{Repo: repo, Refresh: tokenStatsRefresh}

	// Paper-order open/close events, for the panel's real-time WebSocket bridge (cmd/api).
	orderEventsPub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.paper-order-events")
	defer orderEventsPub.Close()

	// CLAUDE.md §31.2: the divisor for dynamic per-position sizing (CurrentEquity /
	// PositionSlots). Recomputed only at startup, matching this service's existing "config changes
	// need a restart" posture (§22) — enabling/disabling a token mid-run doesn't retroactively
	// resize an order already open, only the next one to open.
	//
	// Assignments are loaded up front for this reason (they used to be read inside the engine loop
	// below), and reused there rather than re-queried per instrument.
	strategiesByInst := make(map[string][]usecase.StrategyAssignment, len(instIDs))
	for _, instID := range instIDs {
		if slices.Contains(ptCfg.DisabledInstIDs, instID) {
			// A disabled token opens nothing, so its assignments must not be loaded as tradeable —
			// countPositionSlots below relies on an absent map entry meaning "holds no slot".
			continue
		}
		// Strategy assignments are durable (strategy_assignments table, CLAUDE.md §11.3): loaded
		// fresh from Postgres on every start, so a crash/restart resumes with exactly the same
		// token/timeframe->strategy bindings the panel last configured, not whatever was hardcoded
		// here in Go.
		strategies, err := loadStrategyAssignments(ctx, repo, instID, exchange, logger)
		if err != nil {
			logger.Error("failed to load strategy assignments", "instId", instID, "error", err)
			os.Exit(1)
		}
		strategiesByInst[instID] = strategies
	}
	positionSlots := countPositionSlots(strategiesByInst)
	if positionSlots == 0 {
		// Nothing can open, so nothing can be sized. Falling through would divide by the defensive
		// 1 and open full-account positions the moment an assignment appeared.
		logger.Error("no enabled tokens with assignments — nothing to trade")
		os.Exit(1)
	}
	logger.Info("dynamic sizing divisor", "positionSlots", positionSlots, "tokens", len(instIDs))

	errCh := make(chan error, len(instIDs)+1+len(candleDispatchers))
	// Staggering each instrument's engine start (rather than launching every goroutine in the same
	// instant) spreads out seedCandles()'s REST calls — with 10 instruments x 3 bars, an
	// unstaggered start fires 30 requests within milliseconds and was observed tripping an OKX
	// rate limit reported as instrument-not-found (code 51001) on a different, unpredictable
	// instrument each run. 300ms keeps total startup delay well under a second even at Phase B's
	// 10-token scale.
	const engineStartStagger = 300 * time.Millisecond
	for i, instID := range instIDs {
		// Loaded above, where the slot count needed them — re-querying here would issue one extra
		// round trip per instrument for rows already in hand, and risk the two disagreeing.
		strategies := strategiesByInst[instID]

		candleConsumers := make(map[string]port.MarketDataConsumer, len(candleBars))
		for bar, d := range candleDispatchers {
			candleConsumers[bar] = d.ForInstrument(instID)
		}

		engine := &usecase.PaperTrader{
			InstID: instID,
			// Every ingested bar, so the context timeframes get a maintained window (and are seeded
			// from the database on restart) even though no strategy decides on them.
			Bars:            candleBars,
			CandleWindow:    cfg.PaperTrading.CandleLimit,
			Strategies:      strategies,
			TickConsumer:    tickDispatcher.ForInstrument(instID),
			CandleConsumers: candleConsumers,
			Repo:            repo,
			PositionSlots:   positionSlots,
			MaxOpenOrders:   cfg.PaperTrading.MaxOpenOrders,
			Logger:          logger,
			Model:           model,
			RLSLTPAdjust:    cfg.PaperTrading.RLSLTPAdjust,
			RLDecisionBar:   cfg.PaperTrading.RLDecisionBar,
			RLSizing:        cfg.PaperTrading.RLSizing,
			MaxLeverage:     cfg.Risk.MaxLeverage,
			// Signal-lifecycle conductor (CLAUDE.md §15.12): update cadence, early close, and the
			// clamps bounding where the model may place stops/targets.
			RLUpdatePnLThresholdPct: cfg.PaperTrading.RLUpdatePnLThresholdPct,
			RLUpdateMaxInterval:     cfg.PaperTrading.RLUpdateMaxInterval,
			RLEarlyClose:            cfg.PaperTrading.RLEarlyClose,
			RLClamps:                buildRLClamps(cfg),
			// Force-closes a stale position regardless of RL flags (CLAUDE.md §15.14) — unlike
			// everything else in this block, this is unconditional housekeeping, not RL behavior.
			MaxOpenDuration: cfg.PaperTrading.RLMaxOpenDuration,
			ActiveTokens:    instIDs,
			// v8 observation inputs (docs/RL_V8_PLAN.md). BTCCandles is NOT optional: nil makes
			// buildObservation fail, which skips every model call — deliberately, since a zeroed
			// BTC block would read as "BTC is perfectly flat and uncorrelated" rather than as
			// missing information.
			BTCCandles: btcRef.Window,
			TokenStats: tokenStats.For,
			// One shared account across every token (CLAUDE.md §15.6): each per-instrument engine
			// trades against the same "paper" balance row, not a slice of it. Exchange scopes that
			// balance/config/assignments/open-orders to THIS process's own instance identity
			// (2026-09-22 multi-exchange paper trading) — a second process with PAPER_EXCHANGE=mexc
			// shares no state with this one despite both using Mode="paper".
			Mode:                "paper",
			Exchange:            exchange,
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

	go func() { errCh <- btcRef.Run(ctx, logger) }()
	go func() { errCh <- tokenStats.Run(ctx, logger) }()
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
		MaxTPSLRatio: cfg.PaperTrading.RLClamps.MaxTPSLRatio,
	}
}

// countPositionSlots is the dynamic-sizing divisor (CurrentEquity / PositionSlots, CLAUDE.md
// §31.2/§32.4), extracted so a change to its counting rule fails a test rather than only being
// caught by watching real positions come out too small — the exact way the bug this function fixes
// was actually found. strategiesByInst holds only tokens eligible to open at all: a disabled token
// is never a key (see the caller's loop), and its own value may still legitimately be empty if the
// panel has no strategy assigned there.
//
// Counts distinct TOKENS, not (strategy, token) pairs, as of 2026-09-17 — reverted back alongside
// papertrade.go's own revert of the 2026-09-14 one-per-strategy open guard (hasOpenBaseline, not
// hasOpenBaselineFor). With the guard back to one open position per TOKEN, a (strategy,token)-pair
// divisor under-sizes every position by however many strategies are assigned to that token: at 13
// strategies this was closer to $0.10/position than the intended $4, since equity was being split
// ~390 ways for a roster that can only ever hold ~34 real open positions (one per token) at once.
// Counting tokens matches what the account actually needs to be able to size for.
func countPositionSlots(strategiesByInst map[string][]usecase.StrategyAssignment) int {
	slots := 0
	for _, strategies := range strategiesByInst {
		// A token holds exactly one slot as long as at least one strategy is actually assigned and
		// enabled on it — a token with zero enabled assignments can never open, so it must not claim
		// a slot either, for the same reason a disabled token (never a key here at all) does not.
		if len(strategies) > 0 {
			slots++
		}
	}
	return slots
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
func ensureDefaultAssignment(ctx context.Context, repo *postgres.Repository, exchange string, instIDs []string, paperTradingBars []string) error {
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
		assignments, err := repo.ListAssignments(ctx, instID, false, "paper", exchange)
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
			Exchange:   exchange,
		}); err != nil {
			return err
		}
	}
	return nil
}

// loadStrategyAssignments resolves an instrument's durable strategy_assignments rows into live
// usecase.StrategyAssignment values the PaperTrader can run, rebuilding the strategy.Strategy from
// its DB row's Kind+Config every time (CLAUDE.md §11.3) rather than trusting any in-memory cache.
func loadStrategyAssignments(ctx context.Context, repo *postgres.Repository, instID, exchange string, logger *slog.Logger) ([]usecase.StrategyAssignment, error) {
	rows, err := repo.ListAssignments(ctx, instID, true, "paper", exchange)
	if err != nil {
		return nil, err
	}
	out := make([]usecase.StrategyAssignment, 0, len(rows))
	skipped := 0
	for _, a := range rows {
		cfg, err := repo.GetStrategy(ctx, a.StrategyID)
		if err != nil {
			return nil, fmt.Errorf("resolve strategy %d for assignment %d: %w", a.StrategyID, a.ID, err)
		}
		s, err := strategy.FromConfig(cfg.Kind, cfg.Config)
		if err != nil {
			// SKIP rather than failing the whole load — see cmd/trader's identical guard for the
			// incident that motivated it (2026-09-13): one strategy kind enabled in the database
			// but absent from the binary took the service down on every restart, forever.
			logger.Error("skipping unusable strategy assignment — this strategy will not trade",
				"instId", instID, "assignmentId", a.ID, "strategyId", a.StrategyID,
				"kind", cfg.Kind, "error", err)
			skipped++
			continue
		}
		out = append(out, usecase.StrategyAssignment{Bar: a.Bar, Strategy: s, StrategyID: a.StrategyID, Kind: cfg.Kind})
	}
	if skipped > 0 {
		// Surfaced as its own line so the count is visible even when the per-row errors have
		// scrolled away: "3 of 30 strategies are not running" is the operationally useful fact.
		logger.Error("some strategy assignments could not be built and will not trade",
			"instId", instID, "skipped", skipped, "loaded", len(out))
	}
	return out, nil
}
