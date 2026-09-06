package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// StrategyAssignment pairs a live Strategy value with the candle timeframe it evaluates against
// and the DB row (StrategyID) it was configured from. A PaperTrader can run several strategies
// across several timeframes concurrently — each strategy only re-evaluates when its own assigned
// bar closes. StrategyID is threaded onto every paper order this assignment opens (CLAUDE.md
// §11.3), so it must be populated whenever the assignment is resolved from a durable
// port.StrategyAssignment/port.StrategyConfig row — 0 is only valid for ad-hoc, unpersisted use
// (e.g. tests).
type StrategyAssignment struct {
	Bar        string
	Strategy   strategy.Strategy
	StrategyID int64
	// Kind is the strategy.Factories registry name this assignment was built from. It travels with
	// every signal to the RL model (CLAUDE.md §15.10) as the stable identity one shared policy uses
	// to tell strategies apart — StrategyID can't serve that purpose, since it differs per
	// deployment and per cloned sub-strategy, while a kind means the same thing everywhere.
	Kind string
}

// PaperTrader implements the Paper Trading Engine (CLAUDE.md §8): it evaluates strategies
// against live prices, opens virtual orders with full SL/TP features, and monitors them against
// the real price feed until SL or TP is hit. Closed trades are persisted and become the RL
// training data — no real orders are ever sent to the exchange from this use-case.
//
// Driven entirely by the WS-fed event bus (CLAUDE.md §12), not REST polling: every tick triggers
// an immediate SL/TP check (no missed intra-bar wicks, no polling delay), and every finalized
// candle triggers re-evaluation of strategies assigned to that candle's timeframe, and is
// persisted. The exchange port is used only once per configured timeframe, at startup, to seed
// each candle window.
type PaperTrader struct {
	InstID          string
	Bars            []string // candle timeframes to maintain windows for, e.g. ["1m", "15m", "1h"]
	CandleWindow    int      // how many recent candles to keep in memory per timeframe
	Strategies      []StrategyAssignment
	TickConsumer    port.MarketDataConsumer
	CandleConsumers map[string]port.MarketDataConsumer // keyed by bar
	Repo            port.Repository
	MaxOpenOrders   int
	Logger          *slog.Logger

	// ActiveTokenCount is how many tokens in the configured roster are NOT currently disabled
	// (paper_trading_config.disabled_inst_ids) — the divisor for dynamic per-position sizing
	// (CLAUDE.md §31.2): every new order opens at CurrentEquity/ActiveTokenCount, not a fixed
	// config constant, so sizing tracks the account the same way a real exchange account would —
	// grow after a win, shrink after a loss, and immediately reflect a token being enabled/disabled.
	// Computed once at startup from the same roster/disabled-list every PaperTrader instance
	// shares, so it is identical across all of them despite being a per-instance field. Falls back
	// to 1 if zero/unset (defensive; main.go should never actually pass zero since the roster is
	// never empty in practice).
	ActiveTokenCount int

	// Model/ActiveTokens/TokenBudgetUSD wire the RL agent's in-trade SL/TP adjustment pass
	// (CLAUDE.md §15.4). Model may be nil, in which case the adjustment pass is skipped entirely —
	// this lets PaperTrader run exactly as before (strategy-only) wherever the RL service isn't
	// configured, same "additive, never required" pattern as the rest of §15's rollout.
	Model        port.ModelClient
	ActiveTokens []string // the roster used to build the token-identity one-hot, CLAUDE.md §15.3

	// Mode is which account balance this engine trades against — "paper" here; cmd/trader uses
	// "demo"/"real". All three are tracked simultaneously (CLAUDE.md §15.6), and only non-real
	// modes ever auto-reset a drained balance (§15.7).
	Mode string
	// AccountInitialUSD is the shared account's configured starting balance — what a drained
	// paper/demo account resets back to. Every token trades against this one pool rather than a
	// per-token slice of it (CLAUDE.md §15.6's 2026-08-28 revision).
	AccountInitialUSD decimal.Decimal
	// MaxPositionPct / MaxTotalExposurePct bound how much of account equity the RL agent may put
	// into one position, and into all open positions combined. Hard Go-side caps on the model's
	// proposal — the same "never trust the model as the safety boundary" pattern as §15.4's SL/TP
	// ratchet. Zero disables the respective cap.
	MaxPositionPct      decimal.Decimal
	MaxTotalExposurePct decimal.Decimal

	// RLSizing lets the RL agent set each new order's notional and leverage from its
	// TargetExposure/LeverageFrac action (CLAUDE.md §15.4), instead of every order being opened at
	// the fixed NotionalUSD and 1x. Off by default and independent of the SL/TP-adjust flag: until
	// it's on, the paper_orders log carries no leverage/exposure variance at all, which is exactly
	// the training signal §15.8's continued-live-learning phase needs to learn sizing from.
	// Requires Model and MaxLeverage to be set; falls back to the fixed sizing on any model error.
	RLSizing bool
	// RLSLTPAdjust enables the in-trade SL/TP adjustment pass (CLAUDE.md §15.4). Kept separate from
	// RLSizing so either can run without the other — they're independent decisions that happen to
	// share one model client.
	RLSLTPAdjust bool
	// RLDecisionBar selects which timeframe's strategy signals/price context feed a tick-driven RL
	// SL/TP-adjust decision (CLAUDE.md §15.3). Empty means "the shortest configured bar" — see
	// decisionBar. Must be one of Bars.
	RLDecisionBar string

	// RLUpdatePnLThresholdPct / RLUpdateMaxInterval tune the signal-lifecycle conductor's update
	// cadence (CLAUDE.md §15.12). Zero falls back to the conductor package's defaults.
	RLUpdatePnLThresholdPct decimal.Decimal
	RLUpdateMaxInterval     time.Duration
	// RLEarlyClose lets the model close a position before either level is touched, recorded as
	// close_reason='rl_early' (CLAUDE.md §15.12). Off by default: it is the one lifecycle action
	// that destroys the counterfactual, since an early-closed trade can never show what it would
	// have done.
	RLEarlyClose bool
	// RLClamps bound where the model may PLACE stops and targets on an open — distinct from the
	// ratchet, which governs how they may MOVE afterward (CLAUDE.md §15.11/§15.12).
	RLClamps conductor.Clamps
	// MaxOpenDuration force-closes any open position (RL-adjusted or not) that has run longer
	// than this, close_reason='timeout' (CLAUDE.md §15.14). A hard housekeeping limit, not a
	// model decision — fires unconditionally, independent of RLEarlyClose/RLSizing/RLSLTPAdjust,
	// since a position can run long whether or not any RL feature is even turned on. Zero falls
	// back to conductor.DefaultMaxOpenDuration (6h) via conductor().IsTimedOut.
	MaxOpenDuration time.Duration
	// MaxLeverage is the ceiling LeverageFrac maps onto ([1x, MaxLeverage]). This mirrors the
	// config's risk.max_leverage so paper orders can't record leverage the live risk manager would
	// reject outright (CLAUDE.md §5) — the risk manager remains the real boundary for live trading;
	// this is the paper-mode equivalent so the two produce comparable data.
	MaxLeverage decimal.Decimal

	// TradingPaused stops evaluateStrategies from opening any new position (panel control-box
	// "paused"/"stopped" state) — existing open positions are unaffected, still monitored/closed
	// normally by monitorOpenOrders. "stopped" additionally force-closes every open position at
	// startup via a one-time RequestManualCloseAll sweep (cmd/paper-trader/main.go), which is why
	// this field alone is enough to represent both states in the per-instrument engine: after that
	// startup sweep, "stopped" behaves exactly like "paused" going forward (nothing left open, and
	// nothing new opens either).
	TradingPaused bool
	// OpensDisabled stops evaluateStrategies from opening a NEW position on this one instrument
	// (panel control-box per-token disable) while monitorOpenOrders keeps running unconditionally,
	// so an already-open position on a disabled token still closes normally via its own SL/TP/
	// timeout — disabling a token must not orphan a position that was already open when it was
	// disabled.
	OpensDisabled bool
	// DisableLong / DisableShort drop a newly-fired buy/sell signal before it can open a position
	// (panel control-box long/short toggle) — a strategy still evaluates and fires normally, this
	// only gates whether evaluateStrategies acts on the disabled side. Existing open positions on
	// the disabled side are untouched, same "don't hard-close based on a macro toggle" reasoning as
	// OpensDisabled.
	DisableLong  bool
	DisableShort bool

	// OrderEvents publishes a lightweight open/close notification for every paper order this
	// engine opens or closes, onto the internal event bus (CLAUDE.md §12) — consumed by cmd/api's
	// WebSocket bridge to push real-time position alerts to the panel. May be nil, in which case
	// publishing is skipped entirely (best-effort, never blocks the order open/close itself).
	OrderEvents port.MarketDataPublisher

	// candlesMu guards candles: each bar has its own consumer goroutine (see Run), so writes to
	// this map (even to distinct keys) must be synchronized — concurrent map writes are a fatal
	// Go runtime error, not just a race.
	candlesMu sync.Mutex
	candles   map[string][]domain.Candle // keyed by bar

	// openMu serializes the whole read-open-orders-then-maybe-open sequence in evaluateStrategies.
	// Each bar runs in its own consumer goroutine, so without it two timeframes whose candles close
	// at the same instant both read an empty book and both open a position on the same token —
	// observed in production as orders 70 (15m) and 71 (5m) on ENA-USDT-SWAP, 13ms apart. The
	// no-open-position guard inside evaluateStrategies only covers one call; this makes the check
	// and the write that follows it atomic against the other bars' goroutines.
	openMu sync.Mutex

	// rlAdjustMu/lastRLAdjustAt throttle the RL SL/TP-adjust pass to tick cadence (CLAUDE.md's
	// 2026-08-27 MidPrice-freshness audit): previously this only ran on candle close, so the model
	// could reason about a price up to one full bar interval stale while an open order's SL/TP was
	// actually at risk of being hit in real time. Now every tick is a candidate trigger, throttled
	// to RLAdjustInterval so a busy token doesn't call rl_service on every single tick.
	rlAdjustMu     sync.Mutex
	lastRLAdjustAt time.Time

	// lifecycle is the signal-lifecycle conductor (CLAUDE.md §15.12), built lazily from the RL*
	// fields above so a struct-literal PaperTrader needs no separate initialization step.
	conductorOnce sync.Once
	lifecycle     *conductor.Conductor
}

// RLAdjustInterval is the minimum time between RL SL/TP-adjust passes for one instrument
// (CLAUDE.md §15.4/§15.9's freshness fix) — a fixed wall-clock throttle, not tied to tick count,
// so the effective call rate stays predictable regardless of how fast OKX's tickers channel is
// pushing updates. A few seconds of lag behind the live tick stream is an explicit, accepted
// tradeoff for bounding rl_service's inference load; it is not meant to track every tick.
const RLAdjustInterval = 2 * time.Second

// defaultPaperLeverage is what a paper order records when the RL sizing pass isn't active.
// Lowered 20x -> 10x (2026-09-01, explicit operator request): OKX's real account this service will
// eventually trade against caps leverage at 10x, so paper trading needs to train/validate against
// the leverage ceiling the real account can actually use, not a higher one it never could. Was 20x
// (2026-08-31) before OKX's real-account limit was confirmed.
var defaultPaperLeverage = decimal.NewFromInt(10)

// decisionBar picks which timeframe's strategy signals and price context feed a TICK-driven RL
// decision (CLAUDE.md §15.3/§15.9). A tick doesn't belong to any one bar, so this has to be chosen
// rather than inferred.
//
// Configured explicitly via paper_trading.rl_decision_bar; otherwise the SHORTEST configured bar,
// because an in-trade SL/TP adjustment is a reaction to what price is doing right now and the
// shortest timeframe carries the freshest read of that. This used to be Bars[0] — array order,
// which silently meant "whichever bar happens to be listed first" and would quietly change meaning
// if the config list were reordered.
//
// Note this selects the DECISION context only. The observation still reports one timeframe block,
// but a multi-timeframe strategy contributing signals to it sees every bar (marketView), so
// higher-timeframe context still reaches the model through those signals.
//
// Delegates to decisionBarFor (tickfeed.go), shared with RealTrader's equivalent.
func (e *PaperTrader) decisionBar() string {
	return decisionBarFor(e.RLDecisionBar, e.Bars)
}

// marketView snapshots every maintained timeframe under one lock, so a multi-timeframe strategy
// (CLAUDE.md §9) sees bars that are consistent with each other, and so no lock is held across a
// Strategy.Evaluate call. bar is the timeframe that just closed — the decision cadence.
//
// Delegates to snapshotCandles (tickfeed.go), shared with RealTrader's equivalent.
func (e *PaperTrader) marketView(bar string) strategy.MarketView {
	return snapshotCandles(&e.candlesMu, e.candles, bar)
}

// seedCandlesFromRepo fills each bar's in-memory window from candles already persisted in
// Postgres, so strategies can evaluate immediately after a restart instead of waiting to
// re-accumulate history from the live feed.
//
// Why this is not the REST seeding that was removed in f1af152: that version called OKX's
// /market/candles once per (instrument, bar) at startup, and at 10 instruments x 3 bars it fired
// ~30 concurrent requests that the exchange rejected — reported confusingly as "instrument not
// found" (51001) on a different instrument each run. This reads the SAME candles the ingestor has
// already written to the database, so it makes no exchange call at all and that failure mode
// cannot occur.
//
// Removing the REST seed was still correct; the reasoning that "history fills in within the first
// few closed candles" just only held for the shortest bar. A 1H window needs ~2 days of live feed
// to fill 50 candles, so after every restart only short-window strategies on 5m could fire — which
// is exactly what was observed: 12 of 14 strategies returning hold, and every open position coming
// from the one strategy that needs the fewest candles.
//
// Best-effort per bar: a read failure leaves that window empty and it refills from the live feed,
// which is strictly the old behavior. Seeding must never keep the engine from starting.
//
// Delegates to the free function of the same name in tickfeed.go, shared with RealTrader's
// equivalent (Go's method vs. free-function namespaces are distinct, so this name is not a
// collision).
func (e *PaperTrader) seedCandlesFromRepo(ctx context.Context, logger *slog.Logger) {
	// CandleWindow is what handleCandle trims to, so an unset window means "keep nothing" there —
	// seeding into that would be immediately discarded.
	seedCandlesFromRepo(ctx, &e.candlesMu, e.candles, e.Repo, e.InstID, e.Bars, e.CandleWindow, logger)
}

// trackPnLExtremes advances an open order's peak/trough unrealized PnL (CLAUDE.md §15.11). Only
// writes when a new extreme is actually reached, so a position sitting still doesn't generate a
// database write on every tick.
func (e *PaperTrader) trackPnLExtremes(ctx context.Context, o port.PaperOrder, price decimal.Decimal, logger *slog.Logger) {
	upl := unrealizedPnLPct(o, price)
	if !upl.GreaterThan(o.PnLMaxPct) && !upl.LessThan(o.PnLMinPct) {
		return
	}
	if err := e.Repo.UpdatePaperOrderPnLExtremes(ctx, o.ID, upl, upl); err != nil {
		logger.Warn("failed to update pnl extremes", "instId", e.InstID, "orderId", o.ID, "error", err)
	}
}

// accountMode is the trading mode whose balance this engine moves. Defaults to "paper" so an
// engine constructed without an explicit Mode can never accidentally write to the demo or real
// account's balance (CLAUDE.md §15.7's real-mode carve-out).
func (e *PaperTrader) accountMode() string {
	if e.Mode == "" {
		return "paper"
	}
	return e.Mode
}

// dynamicNotional is what a new position opens at when RLSizing is off: CurrentEquity /
// ActiveTokenCount, not a fixed config constant (CLAUDE.md §31.2). This is how a real exchange
// account actually behaves — the amount committed per position tracks the account's current
// balance, growing after a win and shrinking after a loss, rather than staying pinned to whatever
// number was true the day it was configured. AccountInitialUSD/ActiveTokenCount are the fallback
// for whichever piece is unavailable, so a transient repository error or a startup
// misconfiguration degrades to a sane order of magnitude rather than a zero-size order.
func (e *PaperTrader) dynamicNotional(ctx context.Context, logger *slog.Logger) decimal.Decimal {
	count := e.ActiveTokenCount
	if count <= 0 {
		count = 1
	}
	countDec := decimal.NewFromInt(int64(count))

	equity := e.AccountInitialUSD
	if acct, err := e.Repo.GetAccountEquity(ctx, e.accountMode(), e.AccountInitialUSD); err == nil {
		equity = acct.EquityUSD
	} else {
		logger.Warn("dynamic notional: get account equity failed, falling back to configured initial",
			"mode", e.accountMode(), "error", err)
	}
	if !equity.IsPositive() {
		// A drained (or unreadable) account has nothing real to size against; falling back to the
		// configured initial keeps the order at a sane magnitude instead of opening at ~$0, which
		// would round-trip through every downstream percentage/leverage calculation as noise.
		equity = e.AccountInitialUSD
	}
	return equity.Div(countDec)
}

// evenShareOfAccount is dynamicNotional expressed as a FRACTION rather than a dollar amount: the
// share of the account one token is expected to take when equity is split evenly across the active
// roster. Fed to the model as MaxPositionPct (observation schema v7) so its size_pct is a fraction
// of the budget it actually has rather than of the whole account.
//
// Deliberately derived from ActiveTokenCount, not from a config value: the fixed-sizing path this
// mirrors (dynamicNotional) divides by the same count, so reading a config constant here would let
// the two disagree the moment a token is enabled or disabled. Lives beside dynamicNotional for that
// reason — the two must change together.
func (e *PaperTrader) evenShareOfAccount() decimal.Decimal {
	count := e.ActiveTokenCount
	if count <= 0 {
		count = 1
	}
	return decimal.NewFromInt(1).Div(decimal.NewFromInt(int64(count)))
}

// Run consumes ticks/candles from the event bus until ctx is cancelled. Candle windows start
// empty and fill in from the live feed as bars close — no REST candle-seeding at startup
// (removed 2026-08-29: it was a real crash source at Phase B's 10-instrument scale, tripping an
// undiagnosed OKX-side failure on concurrent /market/candles calls, and the depth it bought
// (CLAUDE.md §14's original candle_limit=100 window) isn't needed now that the shortest strategy
// timeframe (5m) fills a useful window within the first few live bars anyway).
func (e *PaperTrader) Run(ctx context.Context) error {
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
	}

	e.candlesMu.Lock()
	e.candles = make(map[string][]domain.Candle, len(e.Bars))
	e.candlesMu.Unlock()

	e.seedCandlesFromRepo(ctx, logger)

	errCh := make(chan error, 1+len(e.CandleConsumers))
	go func() {
		errCh <- e.TickConsumer.Run(ctx, func(ctx context.Context, data []byte) error {
			return e.handleTick(ctx, data, logger)
		})
	}()
	for bar, consumer := range e.CandleConsumers {
		bar, consumer := bar, consumer
		go func() {
			errCh <- consumer.Run(ctx, func(ctx context.Context, data []byte) error {
				return e.handleCandle(ctx, bar, data, logger)
			})
		}()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (e *PaperTrader) handleTick(ctx context.Context, data []byte, logger *slog.Logger) error {
	price, ok, err := decodeTick(data, e.InstID)
	if err != nil {
		return err
	}
	if !ok {
		return nil // shared stream across instruments; this engine only cares about its own
	}
	if err := e.monitorOpenOrders(ctx, price, logger); err != nil {
		return err
	}

	// The in-trade half of the signal lifecycle (CLAUDE.md §15.12) runs on the live tick stream
	// rather than at candle close — see the audit note on lastRLAdjustAt's declaration for why.
	// The RLAdjustInterval throttle bounds how often rl_service is consulted at all; the
	// conductor's own PnL/time cadence then decides which specific orders are actually due.
	// Best-effort/never blocking.
	if e.Model != nil && e.RLSLTPAdjust && e.shouldRunRLAdjust() {
		e.runUpdates(ctx, e.decisionBar(), price, logger)
	}
	return nil
}

// shouldRunRLAdjust reports whether enough time has passed since the last RL SL/TP-adjust pass to
// run another one now, and if so, atomically claims the slot (updates lastRLAdjustAt) so
// concurrent ticks can't both pass the check and double-fire.
func (e *PaperTrader) shouldRunRLAdjust() bool {
	e.rlAdjustMu.Lock()
	defer e.rlAdjustMu.Unlock()
	if time.Since(e.lastRLAdjustAt) < RLAdjustInterval {
		return false
	}
	e.lastRLAdjustAt = time.Now()
	return true
}

func (e *PaperTrader) handleCandle(ctx context.Context, bar string, data []byte, logger *slog.Logger) error {
	dc, ok, err := decodeCandle(data, e.InstID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	c := dc.Candle

	// OKX pushes the same bar repeatedly as it forms (confirm=0) and once more when it closes
	// (confirm=1). Appending every push would fill the window with partial copies of one bar, so a
	// push REPLACES the last entry whenever it carries the same timestamp — the window then always
	// ends with the live forming candle, which is what the observation's OHLC reports (§15.11).
	applyCandle(&e.candlesMu, e.candles, bar, c, e.CandleWindow)

	if !dc.Confirmed {
		return nil // still forming; wait for the finalized bar before persisting/evaluating
	}
	if err := e.Repo.SaveCandle(ctx, port.Candle{InstID: e.InstID, Bar: bar, Candle: c}); err != nil {
		logger.Warn("failed to persist candle", "instId", e.InstID, "bar", bar, "error", err)
	}
	if err := e.evaluateStrategies(ctx, bar, c.Close, logger); err != nil {
		return err
	}
	// RL SL/TP-adjust now runs on the tick stream instead of here (see handleTick) — CLAUDE.md
	// §15.4/§15.9's freshness fix: candle-close cadence meant the model could reason about a price
	// up to one full bar interval stale while an open order's SL/TP was actually at risk in real
	// time.
	return nil
}

func (e *PaperTrader) evaluateStrategies(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) error {
	// Panel control-box gate (CLAUDE.md): pause/stop and per-token disable both mean "open nothing
	// new here" — checked before taking openMu at all, since there is nothing to do. Existing open
	// positions are untouched; monitorOpenOrders keeps monitoring/closing them regardless of either
	// flag.
	if e.TradingPaused || e.OpensDisabled {
		return nil
	}

	// Held across the whole function: the open-position check and the OpenPaperOrder that may
	// follow it have to be atomic with respect to the other bars' consumer goroutines. See openMu.
	e.openMu.Lock()
	defer e.openMu.Unlock()

	open, err := e.Repo.ListOpenPaperOrders(ctx, e.InstID)
	if err != nil {
		return fmt.Errorf("list open paper orders: %w", err)
	}
	if e.MaxOpenOrders > 0 && len(open) >= e.MaxOpenOrders {
		return nil
	}

	// Snapshot every timeframe, not just the one that closed: a strategy assigned to 5m may consult
	// 1h/4h for trend context via MultiTimeframeStrategy (CLAUDE.md §9). Taken under one lock so all
	// bars are consistent with each other, and released before any Evaluate call.
	view := e.marketView(bar)

	for _, a := range e.Strategies {
		if a.Bar != bar {
			continue // only re-evaluate strategies assigned to the timeframe that just closed
		}
		s := a.Strategy
		signal, err := strategy.EvaluateWith(s, view)
		if err != nil {
			logger.Warn("strategy evaluation failed", "strategy", s.Name(), "instId", e.InstID, "bar", bar, "error", err)
			continue
		}
		metrics.StrategySignalsTotal.WithLabelValues(s.Name(), e.InstID, string(signal.Side)).Inc()
		if signal.Side == strategy.Hold {
			continue
		}
		// Panel control-box long/short toggle (CLAUDE.md): the strategy still evaluates and its
		// signal is still counted in the metric above, this only gates whether it's acted on —
		// existing open positions on the disabled side are left to close normally.
		if (e.DisableLong && signal.Side == strategy.Buy) || (e.DisableShort && signal.Side == strategy.Sell) {
			continue
		}

		// A buy/sell decision only exists when this token has NO baseline position open
		// (CLAUDE.md §15.12): once one is, a firing signal is an `update` about the position that
		// already exists, not a licence to open another. Without this guard every assigned strategy
		// opened independently, so two strategies disagreeing on the same token and bar produced a
		// simultaneous long AND short — positions that cannot both be right and that no single
		// lifecycle decision ever authorized. Forks are excluded: they shadow their baseline parent
		// rather than being separate positions (§15.4).
		if hasOpenBaseline(open) {
			continue
		}

		order := buildPaperOrder(e.InstID, price, signal, e.dynamicNotional(ctx, logger), a.StrategyID, bar)

		// Retain this signal for carry-forward onto later price-driven update calls (CLAUDE.md
		// §15.12): a higher-timeframe opinion stays meaningful between its candles, and dropping it
		// the moment the candle closed would hide it from every update in between.
		resolved := signal.ResolveLevels(price)
		e.conductor().RetainSignal(e.InstID, bar, domain.StrategySignal{
			StrategyID: a.StrategyID,
			Side:       string(signal.Side),
			Confidence: signal.Confidence,
			EntryPx:    resolved.EntryPx,
			SLPx:       resolved.SLPx,
			TPPx:       resolved.TPPx,
			Kind:       a.Kind,
			Bar:        a.Bar,
		})

		// The same observation is used for both the model's open decision and the persisted
		// decision-time record below, so what's stored is exactly what the model was asked.
		obs := e.buildObservation(ctx, bar, price, logger)
		obs.Category = conductor.OpenCategory(string(signal.Side))
		obs.Signal = e.carriedSignalFor(bar)

		// Ask the model whether to take this signal and how to shape it (CLAUDE.md §15.12). It may
		// decline outright — a real decision, distinct from "no opinion" — in which case no order is
		// opened at all.
		if decision, ok := e.openDecision(ctx, obs, signal, open, price, logger); ok {
			if decision.Skip {
				continue
			}
			order.Size, order.Leverage = decision.Notional, decision.Leverage
			// Keep the strategy's levels wherever the model set none: opening a position with no
			// protection because the model stayed silent would be strictly worse than the
			// structure-derived stop the strategy already proposed.
			if decision.SLPx != nil {
				order.SLPx = decision.SLPx
			}
			if decision.TPPx != nil {
				order.TPPx = decision.TPPx
			}
		}

		// Validate the levels the order will actually carry, whatever produced them. This runs on
		// EVERY open, not only when the model shaped it: the clamps used to be applied solely
		// inside openDecision, which returns immediately when rl_sizing is off, so with the model
		// out of the open path nothing validated anything. Order 80 opened with no stop at all
		// that way (CLAUDE.md §16.9) — a safety check must not be reachable only through the
		// feature flag of an optional subsystem.
		// EnsureStop runs FIRST, then Apply. Apply's TP:SL ratio check is skipped when there is no
		// stop to measure against, so filling the stop afterwards would leave the ratio unchecked —
		// observed as stoch_cross orders opening with a 5% stop against a 1% target, a 0.2
		// reward:risk that MinTPSLRatio exists to prevent.
		levels := e.conductorClamps().EnsureStop(string(signal.Side), price, order.Leverage, conductor.Levels{
			SLPx: order.SLPx,
			TPPx: order.TPPx,
		})
		levels = e.conductorClamps().Apply(string(signal.Side), price, order.Leverage, levels)
		if levels.SLPx == nil {
			logger.Error("refusing to open a position with no stop-loss",
				"strategy", s.Name(), "instId", e.InstID, "bar", bar, "side", signal.Side)
			continue
		}
		order.SLPx, order.TPPx = levels.SLPx, levels.TPPx

		// CLAUDE.md §15.3/§15.8: persist the actual observation vector at decision time (not just
		// the realized outcome) so it can later feed live/continued RL training — the whole point
		// is training data that matches exactly what rlclient would have sent, not a reconstruction.
		// Best-effort: a marshal/build failure must never block opening the order itself.
		if raw, err := json.Marshal(obs); err == nil {
			order.FeaturesJSON = raw
		} else {
			logger.Warn("failed to marshal decision-time observation", "instId", e.InstID, "error", err)
		}
		id, err := e.Repo.OpenPaperOrder(ctx, order)
		if err != nil {
			logger.Error("failed to open paper order", "strategy", s.Name(), "instId", e.InstID, "error", err)
			continue
		}
		metrics.PaperOrdersOpenedTotal.WithLabelValues(s.Name(), e.InstID, string(signal.Side)).Inc()
		logger.Info("opened paper order", "id", id, "strategy", s.Name(), "instId", e.InstID, "bar", bar,
			"side", signal.Side, "entry", price, "confidence", signal.Confidence)
		e.publishOrderEvent(ctx, "opened", id, logger)

		// `open` was read once before the loop, so without this the NEXT strategy in the same
		// evaluation still sees an empty book and opens an opposing position microseconds later —
		// which is exactly how orders 44 (sell) and 45 (buy) ended up coexisting on TRUMP/5m.
		order.ID = id
		open = append(open, order)
	}
	return nil
}

func (e *PaperTrader) monitorOpenOrders(ctx context.Context, price decimal.Decimal, logger *slog.Logger) error {
	open, err := e.Repo.ListOpenPaperOrders(ctx, e.InstID)
	if err != nil {
		return fmt.Errorf("list open paper orders: %w", err)
	}
	metrics.PaperOrdersOpenGauge.WithLabelValues(e.InstID).Set(float64(len(open)))

	now := time.Now()
	for _, o := range open {
		// Self-healing 15%-loss enforcement (2026-09-01 incident): buildRLClamps' MaxLossPct field
		// was missing from cmd/paper-trader/main.go's construction of e.RLClamps for some time, so
		// several already-open positions were opened with a leverage-blind stop (e.g. order 636: 5%
		// price distance at 20x = ~100% margin loss on touch, instead of the intended 15% ceiling).
		// That construction bug is fixed, so no NEW order can carry an over-wide stop — but a
		// config/construction bug like this could recur in some other form, and an already-open
		// position from before a fix is deployed needs no manual intervention to become safe again.
		// Checked and tightened in-place on every tick, before any close/touch decision below, so a
		// stop that's already too wide is corrected before it can be touched at its old, wider level.
		if tightened, ok := e.tightenOverWideStop(ctx, o, logger); ok {
			o = tightened
		}

		// Track how far this position has travelled in each direction before checking for a close
		// (CLAUDE.md §15.11) — a trade that ran deep into profit and round-tripped must still show
		// that peak even on the tick that stops it out. Best-effort: this is model input, never a
		// reason to block the close itself.
		e.trackPnLExtremes(ctx, o, price, logger)

		// A manual close request from the panel (2026-08-31) wins over everything else — the
		// operator explicitly asked to exit right now, unlike the timeout/SL/TP checks below,
		// which are the engine's own background decisions. Checked first rather than after the
		// touch check, since "close it now" should not wait for a coincidental SL/TP touch on the
		// same tick to decide the reason for a close that was already requested.
		reason, hit := conductor.CloseReasonManual, o.ManualCloseRequested
		if !hit {
			reason, hit = closeReason(o, price)
		}
		if !hit && e.conductor().IsTimedOut(o.OpenedAt, now) {
			// A position that has run past MaxOpenDuration is force-closed regardless of what
			// SL/TP would otherwise decide (CLAUDE.md §15.14) — checked only once neither level
			// has actually been touched this tick, so a genuine SL/TP hit always takes priority
			// over a timeout that happens to land on the same tick.
			reason, hit = conductor.CloseReasonTimeout, true
		}
		if !hit {
			continue
		}
		if err := e.closeOrder(ctx, o, price, reason, realizedPnL(o, price), logger); err != nil {
			logger.Error("failed to close paper order", "id", o.ID, "error", err)
			continue
		}
	}
	return nil
}

// tightenOverWideStop re-derives o's SL/TP through the SAME clamp pipeline evaluateStrategies uses
// at open time (conductor.Clamps.Apply, entry price + actual leverage) and persists a correction if
// the currently-stored SL is looser than what MaxLossPct/MaxSLDistPct actually allow at this order's
// leverage — self-healing for an already-open position that was opened before a clamp-wiring bug
// (like the missing MaxLossPct field fixed 2026-09-01, CLAUDE.md) is fixed, without needing a
// manual DB edit or a manual close for every affected order.
//
// It applies the correction ONLY when that makes the stop tighter, which the name promises and an
// earlier version of this comment wrongly assumed Apply guaranteed on its own. Apply enforces a
// RANGE: clampRange pulls a distance up to MinSLDistPct as readily as it pulls one down to the
// maximum. That floor is right at open time (a stop 0.001% away is stopped out by noise before the
// trade can breathe) and wrong here, because trailing a stop closer than the initial minimum is
// exactly what locking in profit means. Found on order 2114 (PUMP, 2026-09-06): the model trailed
// the stop to 0.125% from entry, this ran on the next tick, saw 0.125% < the 0.5% floor, and pushed
// it back out to 0.5% — every tick, so the model's stop could never survive and the panel showed it
// frozen at a value no adjustment row explained.
func (e *PaperTrader) tightenOverWideStop(ctx context.Context, o port.PaperOrder, logger *slog.Logger) (port.PaperOrder, bool) {
	if o.SLPx == nil || !o.EntryPx.IsPositive() {
		return o, false
	}
	corrected := e.conductorClamps().Apply(o.Side, o.EntryPx, o.Leverage, conductor.Levels{SLPx: o.SLPx, TPPx: o.TPPx})
	if corrected.SLPx == nil || corrected.SLPx.Equal(*o.SLPx) {
		return o, false
	}
	// Drop the correction when it would LOOSEN the stop (move it further from entry). For a long,
	// tighter means higher; for a short, lower.
	if o.Side == "sell" {
		if corrected.SLPx.GreaterThan(*o.SLPx) {
			return o, false
		}
	} else if corrected.SLPx.LessThan(*o.SLPx) {
		return o, false
	}
	if err := e.Repo.UpdatePaperOrderSLTP(ctx, o.ID, corrected.SLPx, corrected.TPPx); err != nil {
		logger.Error("failed to tighten over-wide stop", "id", o.ID, "instId", e.InstID, "error", err)
		return o, false
	}
	logger.Warn("tightened an over-wide stop on an already-open position",
		"id", o.ID, "instId", e.InstID, "leverage", o.Leverage,
		"oldSLPx", o.SLPx, "newSLPx", corrected.SLPx)
	o.SLPx, o.TPPx = corrected.SLPx, corrected.TPPx
	return o, true
}

// closeOrder is the single close path for a paper order, whether it was stopped out, hit its
// target, or the model closed it early (CLAUDE.md §15.12). Everything a close must do lives here so
// no caller can complete a close while skipping one of the steps — in particular the terminal model
// call, which is the ONLY way a decision ever gets scored (§15.10: the close event is the reward).
func (e *PaperTrader) closeOrder(
	ctx context.Context,
	o port.PaperOrder,
	price decimal.Decimal,
	reason string,
	pnl decimal.Decimal,
	logger *slog.Logger,
) error {
	if err := e.Repo.ClosePaperOrder(ctx, o.ID, price, reason, pnl); err != nil {
		return err
	}
	metrics.PaperOrdersClosedTotal.WithLabelValues(e.InstID, reason).Inc()
	// Prometheus metrics have no decimal support; InexactFloat64 is acceptable here since
	// this is write-only telemetry, not a value used in further financial arithmetic.
	metrics.PaperOrdersRealizedPnL.WithLabelValues(e.InstID).Add(pnl.InexactFloat64())
	logger.Info("closed paper order", "id", o.ID, "instId", e.InstID, "reason", reason, "closePx", price, "pnl", pnl)
	e.publishOrderEvent(ctx, "closed", o.ID, logger)

	// Drop the conductor's per-order update state; keeping it would leak one entry per closed
	// trade for the lifetime of the process.
	e.conductor().Forget(o.ID)

	// Deliver the realized outcome to the model. Best-effort and deliberately AFTER the order is
	// durably closed: a model or network problem must never leave a position open in the database
	// that the price feed has already resolved.
	e.reportTerminal(ctx, o, price, pnl, reason, logger)

	// CLAUDE.md §15.4/§15.6/§15.7: only a baseline order's outcome moves the shared account
	// balance — an rl_adjusted fork is tracking-only (its whole purpose is to be compared
	// against its baseline parent afterward, not to be treated as a second real bet).
	if o.Variant == "baseline" || o.Variant == "" {
		if e.AccountInitialUSD.IsPositive() {
			orderID := o.ID
			if acct, reset, err := e.Repo.ApplyRealizedPnL(ctx, e.accountMode(), pnl, &orderID, e.InstID); err != nil {
				logger.Error("failed to apply realized pnl to account", "instId", e.InstID, "error", err)
			} else if reset {
				// Recorded as a reason="reset" point in the equity timeline too, so a drain that
				// happens overnight is visible in the panel's chart afterward, not just here.
				logger.Warn("account balance drained, reset to initial", "mode", e.accountMode(),
					"instId", e.InstID, "initialUsd", acct.InitialUSD, "resetCount", acct.ResetCount)
			}
		}
	}
	return nil
}

// PaperOrderEvent is the lightweight open/close notification published to
// "okx.paper-order-events" (CLAUDE.md §12) — just enough for cmd/api's WebSocket bridge to tell
// the panel "something changed," which then re-fetches full position detail via GET /api/positions
// rather than this event carrying the full port.PaperOrder itself.
type PaperOrderEvent struct {
	Type    string `json:"type"` // "opened" or "closed"
	OrderID int64  `json:"orderId"`
	InstID  string `json:"instId"`
}

// publishOrderEvent is best-effort: a publish failure must never block/undo the order
// open/close it's reporting on. No-op if OrderEvents wasn't configured.
func (e *PaperTrader) publishOrderEvent(ctx context.Context, eventType string, orderID int64, logger *slog.Logger) {
	if e.OrderEvents == nil {
		return
	}
	event := PaperOrderEvent{Type: eventType, OrderID: orderID, InstID: e.InstID}
	if err := e.OrderEvents.Publish(ctx, e.InstID, event); err != nil {
		logger.Warn("failed to publish paper order event", "type", eventType, "orderId", orderID, "instId", e.InstID, "error", err)
	}
}

func closeReason(o port.PaperOrder, price decimal.Decimal) (string, bool) {
	return SLTPTouchReason(o.Side, o.SLPx, o.TPPx, price)
}

// SLTPTouchReason is the shared SL/TP-touch comparison — genuine domain logic (CLAUDE.md §16.3's
// trial mechanics require the exact same touch semantics PaperTrader uses for real paper orders,
// so cmd/strategy-optimizer's trial checker calls this directly rather than duplicating it).
// side is "buy" or "sell"; slPx/tpPx may be nil (no touch check on that side). Returns ("sl" or
// "tp", true) on a touch, or ("", false) if neither has been hit yet at price.
func SLTPTouchReason(side string, slPx, tpPx *decimal.Decimal, price decimal.Decimal) (string, bool) {
	switch side {
	case "buy":
		if slPx != nil && price.LessThanOrEqual(*slPx) {
			return "sl", true
		}
		if tpPx != nil && price.GreaterThanOrEqual(*tpPx) {
			return "tp", true
		}
	case "sell":
		if slPx != nil && price.GreaterThanOrEqual(*slPx) {
			return "sl", true
		}
		if tpPx != nil && price.LessThanOrEqual(*tpPx) {
			return "tp", true
		}
	}
	return "", false
}

func realizedPnL(o port.PaperOrder, closePx decimal.Decimal) decimal.Decimal {
	direction := decimal.NewFromInt(1)
	if o.Side == "sell" {
		direction = decimal.NewFromInt(-1)
	}
	return direction.Mul(closePx.Sub(o.EntryPx)).Div(o.EntryPx).Mul(o.Size).Mul(o.Leverage)
}

// hasOpenBaseline reports whether any of these orders is a real (non-fork) open position. Forks
// are tracking-only shadows of their baseline parent (CLAUDE.md §15.4), so one must never make the
// token look occupied to the open-decision path.
func hasOpenBaseline(orders []port.PaperOrder) bool {
	for _, o := range orders {
		if o.Variant == "baseline" || o.Variant == "" {
			return true
		}
	}
	return false
}

func buildPaperOrder(instID string, price decimal.Decimal, signal strategy.Signal, notionalUSD decimal.Decimal, strategyID int64, bar string) port.PaperOrder {
	var slPx, tpPx *decimal.Decimal
	direction := decimal.NewFromInt(1)
	if signal.Side == strategy.Sell {
		direction = decimal.NewFromInt(-1)
	}
	if signal.SLPct.IsPositive() {
		v := price.Sub(direction.Mul(signal.SLPct).Mul(price))
		slPx = &v
	}
	if signal.TPPct.IsPositive() {
		v := price.Add(direction.Mul(signal.TPPct).Mul(price))
		tpPx = &v
	}

	var strategyIDPtr *int64
	if strategyID != 0 {
		strategyIDPtr = &strategyID
	}

	return port.PaperOrder{
		InstID:     instID,
		StrategyID: strategyIDPtr,
		Side:       string(signal.Side),
		EntryPx:    price,
		SLPx:       slPx,
		TPPx:       tpPx,
		Size:       notionalUSD,
		Bar:        bar,
		// 1x is the un-sized default: the strategy layer has no view on leverage, so an order
		// opened without the RL sizing pass (PaperTrader.RLSizing) records the unlevered position
		// the signal itself implies. The caller overwrites Size/Leverage when RL sizing is on.
		Leverage: defaultPaperLeverage,
	}
}
