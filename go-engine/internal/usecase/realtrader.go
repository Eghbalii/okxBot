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
	"github.com/eghbalii/okxBot/go-engine/internal/risk"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// RealTrader brings cmd/trader onto the same strategy-signal + conductor lifecycle
// usecase.PaperTrader already runs (CLAUDE.md §27.3), replacing the old flat delta-notional
// rebalance loop (trade.go's Trader) — but placing real orders on the exchange instead of
// bookkeeping-only virtual ones. Deliberately NOT PaperTrader-with-a-flag: the two engines share
// their pure IO skeleton (tickfeed.go) and their pure sizing/adjustment math
// (sizeFromModelAction/computeAdjustedLevels, extracted for exactly this reuse), but keep separate
// types so a bug in real-money code can never leak into paper trading's already-proven,
// heavily-tested path, and vice versa.
//
// The "no fork" mechanic (CLAUDE.md §15.4's shadow-fork A/B comparison is paper-trading-only):
// real trading holds at most one open position per token per side, and the model edits its SL/TP
// in place. Corrected 2026-09-03 (see the plan doc's §3a): that edit is watched by RealTrader's
// own in-process tick monitor, the SAME mechanism PaperTrader already uses — never a resting
// conditional/algo order on OKX. A periodic reconciliation poll (reconcile, see below) checks
// GetPositions/GetBalance against this process's own bookkeeping to catch drift (a manual close on
// OKX's own UI, a liquidation, a missed fill) rather than trusting local state blindly.
//
// Futures/perpetual-swap ("SWAP") endpoints only, matching every other exchange call in this
// codebase — this type introduces no new instrument type or exchange endpoint category.
type RealTrader struct {
	InstID          string
	Bars            []string
	CandleWindow    int
	Strategies      []StrategyAssignment
	TickConsumer    port.MarketDataConsumer
	CandleConsumers map[string]port.MarketDataConsumer
	Repo            port.Repository
	Exchange        port.ExchangeClient
	Model           port.ModelClient
	RiskManager     *risk.Manager
	Logger          *slog.Logger

	// ExecInstID is the instId actually placed/canceled/queried on the exchange — NOT necessarily
	// InstID. Found live 2026-09-04: this project's market data (candles/ticks/paper_orders/the RL
	// observation's token identity) is collected against the classic, deeply liquid SWAP
	// instruments (e.g. BTC-USDT-SWAP), but a given real account may only have usable margin on a
	// DIFFERENT OKX product for the same underlying — this account's is BTC-USD_UM_XPERP-<date>
	// ("X-Perp", instType FUTURES, settled in USD/USDC/USDG under Multi-currency margin mode).
	// Prices track within ~0.1% of the SWAP market (verified live across all 10 configured tokens),
	// so InstID's collected candles/observation remain valid training/decision input — only the
	// literal exchange call needs to target a possibly-different instId. Falls back to InstID when
	// empty, so a deployment whose account CAN trade the SWAP instrument directly needs no mapping
	// at all. ExecInstType/ExecCtValCcy answer instType/CtValCcy for GetPositions/GetInstrument;
	// SettleCcy answers GetBalance's ccy — all three must agree with whichever product ExecInstID
	// actually names, since a SWAP-shaped default (instType=SWAP, ccy=USDT) silently returns
	// nothing (not an error) against an account whose usable balance/positions live elsewhere.
	ExecInstID   string
	ExecInstType string // e.g. "SWAP" or "FUTURES"; defaults to "SWAP" when empty
	SettleCcy    string // e.g. "USDT" or "USDC"; defaults to "USDT" when empty

	// Mode is "demo" or "real" (CLAUDE.md §15.6) — never "paper". Selects which account_equity row
	// this engine's equity timeline is recorded under and which mode's rows ListPositions/
	// ListOpenPaperOrders-equivalent queries are scoped to (see openPositions/allOpenPositions
	// below) — critical, since paper trading may be running concurrently against the SAME InstID
	// and paper_orders has no per-mode partition beyond the Mode column itself.
	Mode string
	// TdMode/PosMode are OKX's margin/position mode config (CLAUDE.md §27.2) — same fields Trader
	// already carries, reused as-is.
	TdMode  string // "cross" or "isolated"
	PosMode string // "net" or "long_short" (hedge mode)

	ActiveTokens      []string
	AccountInitialUSD decimal.Decimal

	// MaxLeverage/MaxPositionPct/MaxTotalExposurePct feed sizeFromModelAction (the shared free
	// function, CLAUDE.md §27's plan commit 2) — same equity-fraction caps PaperTrader.RLSizing
	// uses, expressed against the exchange's own reported equity here (see buildObservation) rather
	// than a bookkeeping row this engine owns.
	MaxLeverage         decimal.Decimal
	MaxPositionPct      decimal.Decimal
	MaxTotalExposurePct decimal.Decimal

	// RLUpdatePnLThresholdPct/RLUpdateMaxInterval/RLEarlyClose/RLClamps/MaxOpenDuration configure
	// the conductor exactly like PaperTrader's equivalents (CLAUDE.md §15.12).
	RLUpdatePnLThresholdPct decimal.Decimal
	RLUpdateMaxInterval     time.Duration
	RLEarlyClose            bool
	RLClamps                conductor.Clamps
	MaxOpenDuration         time.Duration

	// ReconcileInterval is how often reconcile() polls GetPositions/GetBalance against this
	// process's own bookkeeping (CLAUDE.md §27.3/§27.6). Fixed at 1 minute by explicit operator
	// decision (2026-09-03) — not a tunable meant to be lowered; see ReconcileInterval's own
	// comment on why this is deliberately not a tight loop. Zero falls back to the constant below.
	ReconcileInterval time.Duration

	// FillTimeout bounds how long a placed market order (open or the flattening close order) is
	// given to fill before it's CANCELED and given up on (CLAUDE.md §27.5) — no automatic retry or
	// re-pricing; the next real signal on its own normal cadence is what tries again. Futures market
	// orders against a liquid perpetual are expected to fill essentially immediately, so this exists
	// to bound the rare case where one doesn't, not as the primary fill path. Zero falls back to
	// DefaultFillTimeout.
	FillTimeout time.Duration

	OrderEvents port.MarketDataPublisher

	candlesMu sync.Mutex
	candles   map[string][]domain.Candle

	openMu sync.Mutex

	conductorOnce sync.Once
	lifecycle     *conductor.Conductor

	instrumentOnce sync.Once
	instrument     domain.Instrument
	instrumentErr  error
}

// execInstID is the instId actually sent to the exchange — ExecInstID when set, else InstID
// (CLAUDE.md §27's real-account finding: an account may only have usable margin on a different
// OKX product than the one this engine's market data/observation identity uses).
func (e *RealTrader) execInstID() string {
	if e.ExecInstID != "" {
		return e.ExecInstID
	}
	return e.InstID
}

// execInstType is the instType used for GetPositions/GetInstrument calls — "SWAP" when
// ExecInstType is unset, matching this codebase's pre-2026-09-04 assumption for every deployment
// whose account trades the classic SWAP product directly.
func (e *RealTrader) execInstType() string {
	if e.ExecInstType != "" {
		return e.ExecInstType
	}
	return "SWAP"
}

// settleCcy is the currency used for GetBalance calls — "USDT" when SettleCcy is unset, matching
// this codebase's pre-2026-09-04 assumption.
func (e *RealTrader) settleCcy() string {
	if e.SettleCcy != "" {
		return e.SettleCcy
	}
	return "USDT"
}

// instrumentMeta fetches and caches execInstID's contract-shape metadata (CtVal/LotSz/MinSz) on
// first use — a real network call only once per process lifetime, not once per order, since an
// instrument's contract shape does not change while the process runs. A fetch failure is cached
// too (instrumentOnce fires exactly once regardless of outcome) rather than retried on every
// order: a real order must not silently fall back to an unconverted size if this call is broken,
// so callers treat a returned error as fatal to that order rather than proceeding with a guess.
func (e *RealTrader) instrumentMeta() (domain.Instrument, error) {
	e.instrumentOnce.Do(func() {
		e.instrument, e.instrumentErr = e.Exchange.GetInstrument(e.execInstType(), e.execInstID())
	})
	return e.instrument, e.instrumentErr
}

// sizeToContracts converts a desired notional (USD) at the given price into a valid contract
// count for execInstID: notional/price gives the base-unit size (e.g. BTC), divided by CtVal to
// get contracts, then rounded down to the nearest LotSz multiple — OKX rejects a size that isn't
// an exact multiple (51121, found live 2026-09-04). Rounding DOWN, never up, so a real order can
// never request more notional than what was actually sized/approved by the risk manager upstream.
// A CtVal of zero (unset/misconfigured instrument metadata) is treated as 1 — the pre-2026-09-04
// implicit assumption — rather than dividing by zero.
func sizeToContracts(notionalUSD, price decimal.Decimal, inst domain.Instrument) decimal.Decimal {
	ctVal := inst.CtVal
	if !ctVal.IsPositive() {
		ctVal = decimal.NewFromInt(1)
	}
	baseUnits := notionalUSD.Div(price)
	contracts := baseUnits.Div(ctVal)
	if inst.LotSz.IsPositive() {
		lots := contracts.Div(inst.LotSz).Floor()
		contracts = lots.Mul(inst.LotSz)
	}
	return contracts
}

// DefaultReconcileInterval is the reconciliation poll's cadence when RealTrader.ReconcileInterval
// is unset — 1 minute, the operator's explicit figure (2026-09-03): frequent enough to catch drift
// (a manual close on OKX's own UI, a liquidation) within a bounded window, deliberately NOT tied to
// the tick feed's cadence, since SL/TP execution itself is driven by the live tick stream, not by
// this poll — the poll exists only to keep bookkeeping honest, never to drive the trading loop.
const DefaultReconcileInterval = time.Minute

func (e *RealTrader) reconcileInterval() time.Duration {
	if e.ReconcileInterval > 0 {
		return e.ReconcileInterval
	}
	return DefaultReconcileInterval
}

// DefaultFillTimeout is RealTrader.FillTimeout's fallback when unset — 60s, matching
// config.FillTimeout.OrderFillTimeoutSec's own default (CLAUDE.md §27.5).
const DefaultFillTimeout = 60 * time.Second

// fillPollInterval is how often waitForFill re-polls GetOrder while waiting — short relative to
// FillTimeout since a market order against a liquid perpetual is expected to fill within one or
// two polls; this isn't the reconciliation poll's "don't hammer the exchange" concern; it's a
// short, bounded wait for a single order's own outcome.
const fillPollInterval = 500 * time.Millisecond

func (e *RealTrader) fillTimeout() time.Duration {
	if e.FillTimeout > 0 {
		return e.FillTimeout
	}
	return DefaultFillTimeout
}

// waitForFill polls Exchange.GetOrder for ordID until it reaches a terminal state (filled or
// canceled) or fillTimeout() elapses, whichever comes first (CLAUDE.md §27.5). On timeout it
// CANCELS the order and returns the status as last observed — deliberately no retry or re-price;
// the caller (openReal/closeRealWith) is responsible for deciding what an unfilled/partially-
// filled result means for its own path. A GetOrder error mid-poll is logged and treated as "not
// yet terminal" rather than aborting the wait outright — a single flaky status read must not
// abandon an order that may still be filling normally.
// WaitForFillForTesting exports waitForFill for cmd/okx-apitest (CLAUDE.md's 2026-09-04 API-key
// diagnostic) to call the real production fill-timeout/cancel path directly, without pulling in
// the rest of RealTrader's strategy/conductor/Kafka machinery — the diagnostic must never risk
// opening a position on its own initiative. Behavior is identical to waitForFill; this is purely
// a visibility export, not a separate implementation.
func (e *RealTrader) WaitForFillForTesting(ctx context.Context, ordID string, logger *slog.Logger) (domain.OrderStatus, error) {
	return e.waitForFill(ctx, ordID, logger)
}

func (e *RealTrader) waitForFill(ctx context.Context, ordID string, logger *slog.Logger) (domain.OrderStatus, error) {
	deadline := time.Now().Add(e.fillTimeout())
	var last domain.OrderStatus
	for {
		status, err := e.Exchange.GetOrder(e.execInstID(), ordID)
		if err != nil {
			logger.Warn("fill-timeout: get order status failed, will retry", "instId", e.execInstID(), "ordId", ordID, "error", err)
		} else {
			last = status
			if status.IsTerminal() {
				return last, nil
			}
		}

		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(fillPollInterval):
		}
	}

	logger.Warn("fill-timeout: order not filled within timeout, canceling",
		"instId", e.execInstID(), "ordId", ordID, "timeout", e.fillTimeout(), "lastState", last.State)
	if err := e.Exchange.CancelOrder(e.execInstID(), ordID); err != nil {
		logger.Error("fill-timeout: cancel failed", "instId", e.execInstID(), "ordId", ordID, "error", err)
		return last, fmt.Errorf("order %s not filled within %s and cancel failed: %w", ordID, e.fillTimeout(), err)
	}
	return last, nil
}

func (e *RealTrader) accountMode() string {
	if e.Mode == "" {
		return "real"
	}
	return e.Mode
}

func (e *RealTrader) decisionBar() string {
	return decisionBarFor("", e.Bars)
}

func (e *RealTrader) marketView(bar string) strategy.MarketView {
	return snapshotCandles(&e.candlesMu, e.candles, bar)
}

func (e *RealTrader) seedCandlesFromRepo(ctx context.Context, logger *slog.Logger) {
	seedCandlesFromRepo(ctx, &e.candlesMu, e.candles, e.Repo, e.InstID, e.Bars, e.CandleWindow, logger)
}

func (e *RealTrader) conductor() *conductor.Conductor {
	e.conductorOnce.Do(func() {
		e.lifecycle = conductor.New(conductor.Config{
			UpdatePnLThresholdPct: e.RLUpdatePnLThresholdPct,
			UpdateMaxInterval:     e.RLUpdateMaxInterval,
			AllowEarlyClose:       e.RLEarlyClose,
			Clamps:                e.RLClamps,
			MaxOpenDuration:       e.MaxOpenDuration,
		})
	})
	return e.lifecycle
}

func (e *RealTrader) conductorClamps() conductor.Clamps {
	return e.RLClamps
}

// Run consumes ticks/candles from the event bus and drives the reconciliation poll, until ctx is
// cancelled. Mirrors PaperTrader.Run's shape (CLAUDE.md §27's plan, tickfeed.go).
func (e *RealTrader) Run(ctx context.Context) error {
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
	}

	e.candlesMu.Lock()
	e.candles = make(map[string][]domain.Candle, len(e.Bars))
	e.candlesMu.Unlock()

	e.seedCandlesFromRepo(ctx, logger)

	errCh := make(chan error, 2+len(e.CandleConsumers))
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
	go func() {
		errCh <- e.runReconcileLoop(ctx, logger)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (e *RealTrader) handleTick(ctx context.Context, data []byte, logger *slog.Logger) error {
	price, ok, err := decodeTick(data, e.InstID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := e.monitorOpenPositions(ctx, price, logger); err != nil {
		return err
	}
	e.runUpdates(ctx, e.decisionBar(), price, logger)
	return nil
}

func (e *RealTrader) handleCandle(ctx context.Context, bar string, data []byte, logger *slog.Logger) error {
	dc, ok, err := decodeCandle(data, e.InstID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	c := dc.Candle
	applyCandle(&e.candlesMu, e.candles, bar, c, e.CandleWindow)
	if !dc.Confirmed {
		return nil
	}
	// Unlike PaperTrader, RealTrader is never the only writer of a bar's candles — cmd/paper-trader
	// (or another RealTrader instance sharing this instrument) already persists them. Re-saving here
	// would just be a redundant upsert on the same (inst_id, bar, ts) key, so this deliberately does
	// NOT call Repo.SaveCandle.
	return e.evaluateStrategies(ctx, bar, c.Close, logger)
}

// openPositions returns this MODE's currently-open real positions for InstID — never
// ListOpenPaperOrders, which is InstID-scoped only and would also return paper trading's own open
// orders on the same instrument if paper trading happens to be running concurrently (it is, in
// this project's actual deployment). Mode-scoped via ListPositions' filter instead.
func (e *RealTrader) openPositions(ctx context.Context) ([]port.PaperOrder, error) {
	open := true
	return e.Repo.ListPositions(ctx, port.PositionFilter{Mode: e.accountMode(), InstID: e.InstID, Open: &open})
}

// hasOpenPosition reports whether any of open is a real position — always true for a
// mode-filtered ListPositions result, but kept as a named check (mirroring PaperTrader's
// hasOpenBaseline) so the "is this token occupied" question reads the same way at both call sites.
func hasOpenPosition(open []port.PaperOrder) bool {
	return len(open) > 0
}

func (e *RealTrader) evaluateStrategies(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) error {
	if halted, reason := e.RiskManager.Halted(); halted {
		logger.Warn("real trading halted, skipping new opens", "instId", e.InstID, "reason", reason)
		return nil
	}

	// Serializes the whole read-open-then-maybe-open sequence against the other bars' consumer
	// goroutines — same race PaperTrader.openMu guards against (CLAUDE.md §16.9).
	e.openMu.Lock()
	defer e.openMu.Unlock()

	open, err := e.openPositions(ctx)
	if err != nil {
		return fmt.Errorf("list open real positions: %w", err)
	}

	view := e.marketView(bar)

	for _, a := range e.Strategies {
		if a.Bar != bar {
			continue
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

		// One open position per token per side (CLAUDE.md §27.3): in net PosMode this means at most
		// one position of ANY side, so a signal while one is already open is never a flip — it's
		// routed to runUpdates as an `update` instead, exactly like PaperTrader's own
		// hasOpenBaseline gate. Hedge (long_short) mode's per-side gating is left for when the
		// open/close paths actually need to key by side; net mode is what's configured today.
		if hasOpenPosition(open) {
			continue
		}

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

		obs := e.buildObservation(ctx, bar, price, logger)
		obs.Category = conductor.OpenCategory(string(signal.Side))
		obs.Signal = e.carriedSignalFor(bar)

		opened, err := e.openReal(ctx, obs, signal, open, price, a, bar, logger)
		if err != nil {
			logger.Error("failed to open real order", "strategy", s.Name(), "instId", e.InstID, "error", err)
			continue
		}
		if opened == nil {
			continue // model declined, or the signal produced no usable levels
		}
		open = append(open, *opened)
	}
	return nil
}

// openReal asks the model whether to take signal, sizes/clamps the result, places the real order,
// and persists the row. Returns nil, nil when the signal was declined (model skip, or no usable
// stop) rather than an error — declining is a normal outcome, not a failure.
func (e *RealTrader) openReal(
	ctx context.Context,
	obs domain.Observation,
	signal strategy.Signal,
	openOrders []port.PaperOrder,
	price decimal.Decimal,
	a StrategyAssignment,
	bar string,
	logger *slog.Logger,
) (*port.PaperOrder, error) {
	category := conductor.OpenCategory(string(signal.Side))
	if category == "" || e.Model == nil {
		return nil, nil
	}
	obs.Category = category

	action, err := e.Model.Predict(ctx, obs)
	if err != nil {
		logger.Warn("real open: predict failed", "instId", e.InstID, "error", err)
		return nil, nil
	}
	if action.Action == domain.ActionSkip {
		logger.Info("real open: model declined the signal", "instId", e.InstID, "side", signal.Side)
		return nil, nil
	}
	if action.Action != domain.ActionOpen {
		logger.Info("real open: model gave no open/skip answer, declining to trade without one",
			"instId", e.InstID, "action", action.Action)
		return nil, nil
	}

	cfg := sizingConfig{InstID: e.InstID, MaxLeverage: e.MaxLeverage, MaxPositionPct: e.MaxPositionPct, MaxTotalExposurePct: e.MaxTotalExposurePct}
	notional, leverage, sized := sizeFromModelAction(cfg, action, obs, openOrders, logger)
	if !sized {
		logger.Info("real open: model action not sizable, declining", "instId", e.InstID)
		return nil, nil
	}

	order := buildPaperOrder(e.InstID, price, signal, notional, a.StrategyID, bar)
	order.Mode = e.accountMode()
	order.Leverage = leverage
	if levels := nonZeroLevels(action.SLPx, action.TPPx); levels.SLPx != nil || levels.TPPx != nil {
		if levels.SLPx != nil {
			order.SLPx = levels.SLPx
		}
		if levels.TPPx != nil {
			order.TPPx = levels.TPPx
		}
	}

	// Same validation pass as PaperTrader.evaluateStrategies, on EVERY open regardless of what
	// shaped the levels (CLAUDE.md §16.9 — a safety check reachable only through an optional
	// subsystem is not a safety check). Runs BEFORE the risk-manager gate: clamps answer "is this
	// stop/target sane relative to entry," the risk manager answers "does this violate an
	// account-wide hard limit regardless of what any upstream layer decided" (§5 of the plan doc).
	clampedLevels := e.conductorClamps().EnsureStop(order.Side, price, order.Leverage, conductor.Levels{SLPx: order.SLPx, TPPx: order.TPPx})
	clampedLevels = e.conductorClamps().Apply(order.Side, price, order.Leverage, clampedLevels)
	if clampedLevels.SLPx == nil {
		logger.Error("refusing to open a real position with no stop-loss", "instId", e.InstID, "side", order.Side)
		return nil, nil
	}
	order.SLPx, order.TPPx = clampedLevels.SLPx, clampedLevels.TPPx

	approved, err := e.RiskManager.Approve(risk.ProposedAction{
		Leverage:             order.Leverage,
		PositionNotionalUSD:  signedNotional(order.Side, order.Size),
		LiquidationBufferPct: liquidationBufferEstimate(order.Leverage),
	})
	if err != nil {
		logger.Warn("real open rejected by risk manager", "instId", e.InstID, "error", err)
		return nil, nil
	}
	order.Leverage = approved.Leverage
	order.Size = approved.PositionNotionalUSD.Abs()
	if order.Size.IsZero() {
		return nil, nil
	}

	if err := e.setLeverageIfNeeded(order.Leverage, order.Side, logger); err != nil {
		return nil, fmt.Errorf("set leverage: %w", err)
	}

	inst, err := e.instrumentMeta()
	if err != nil {
		return nil, fmt.Errorf("fetch instrument metadata: %w", err)
	}
	sz := sizeToContracts(order.Size, price, inst)
	if sz.IsZero() || (inst.MinSz.IsPositive() && sz.LessThan(inst.MinSz)) {
		logger.Info("real open: sized order below instrument minimum, declining",
			"instId", e.execInstID(), "sz", sz, "minSz", inst.MinSz)
		return nil, nil
	}
	req := domain.OrderRequest{InstID: e.execInstID(), TdMode: e.TdMode, Side: order.Side, OrdType: "market", Sz: sz}
	if e.PosMode == "long_short" {
		req.PosSide = posSideFor(signedNotional(order.Side, order.Size))
	}
	result, err := e.Exchange.PlaceOrder(req)
	if err != nil {
		return nil, fmt.Errorf("place order: %w", err)
	}
	if result != nil && result.SCode != "0" {
		return nil, fmt.Errorf("order rejected: sCode=%s sMsg=%s", result.SCode, result.SMsg)
	}
	if result != nil && result.OrdID != "" {
		ordID := result.OrdID
		order.ExchangeOrderID = &ordID
	}

	// CLAUDE.md §27.5: confirm the fill rather than trusting PlaceOrder's acceptance response alone
	// — a market order against a liquid perpetual is expected to fill essentially immediately, but
	// this must not be assumed. Nothing is persisted for an order that never filled at all: there is
	// no real position to record, and this token's open-position slot must stay free for the next
	// signal rather than being blocked by a phantom row.
	if order.ExchangeOrderID != nil {
		status, err := e.waitForFill(ctx, *order.ExchangeOrderID, logger)
		if err != nil {
			return nil, fmt.Errorf("wait for fill: %w", err)
		}
		switch {
		case status.IsFilled():
			if status.AvgPx.IsPositive() {
				order.EntryPx = status.AvgPx
			}
		case status.AccFillSz.IsPositive():
			// Partially filled within the timeout window: a real, smaller-than-intended position
			// exists on the exchange (canceled by waitForFill's timeout path for the remainder), so
			// record what actually filled rather than the originally requested size — never assume
			// the unfilled remainder will complete after the order was just canceled.
			logger.Warn("real open: order partially filled before timeout/cancel",
				"instId", e.InstID, "ordId", *order.ExchangeOrderID,
				"requestedSz", sz, "filledSz", status.AccFillSz)
			order.Size = order.Size.Mul(status.AccFillSz).Div(sz)
			if status.AvgPx.IsPositive() {
				order.EntryPx = status.AvgPx
			}
		default:
			// Never filled at all before the timeout — canceled, nothing to record.
			logger.Info("real open: order canceled unfilled, no position opened",
				"instId", e.InstID, "ordId", *order.ExchangeOrderID)
			return nil, nil
		}
	}

	if raw, err := json.Marshal(obs); err == nil {
		order.FeaturesJSON = raw
	} else {
		logger.Warn("failed to marshal decision-time observation", "instId", e.InstID, "error", err)
	}

	id, err := e.Repo.OpenPaperOrder(ctx, order)
	if err != nil {
		return nil, fmt.Errorf("persist real order: %w", err)
	}
	order.ID = id
	metrics.PaperOrdersOpenedTotal.WithLabelValues(a.Kind, e.InstID, order.Side).Inc()
	logger.Info("opened real order", "id", id, "instId", e.InstID, "side", order.Side,
		"entry", price, "size", order.Size, "leverage", order.Leverage, "exchangeOrderId", order.ExchangeOrderID)
	e.publishOrderEvent(ctx, "opened", id, logger)

	return &order, nil
}

// setLeverageIfNeeded calls SetLeverage unconditionally on open — RealTrader has no cheap prior
// leverage to compare against the way trade.go's Trader does (it reads the exchange's current
// position leverage every poll; RealTrader only calls GetPositions from the reconciliation poll,
// not on every open) — an extra SetLeverage call when the value happens to already match is a
// harmless no-op on OKX's side, not worth threading additional state to avoid.
func (e *RealTrader) setLeverageIfNeeded(leverage decimal.Decimal, side string, logger *slog.Logger) error {
	if !leverage.IsPositive() {
		return nil
	}
	req := domain.LeverageChange{InstID: e.execInstID(), Lever: leverage, MgnMode: e.TdMode}
	if e.PosMode == "long_short" {
		req.PosSide = posSideFor(signedNotionalForSide(side))
	}
	return e.Exchange.SetLeverage(req)
}

func signedNotional(side string, notional decimal.Decimal) decimal.Decimal {
	if side == "sell" {
		return notional.Neg()
	}
	return notional
}

func signedNotionalForSide(side string) decimal.Decimal {
	if side == "sell" {
		return decimal.NewFromInt(-1)
	}
	return decimal.NewFromInt(1)
}

// liquidationBufferEstimate mirrors trade.go's execute() conservative 100/leverage approximation
// (CLAUDE.md §27.2) — ignores maintenance margin, same accepted simplification as the existing live
// path. RealTrader has no prior position's LiqPx/MarkPx to cross-check against at OPEN time (that
// cross-check only makes sense once a position exists, per §27.2's own note) — the reconciliation
// poll (reconcile) is where an already-open position's real liquidation distance gets checked
// against what OKX reports.
func liquidationBufferEstimate(leverage decimal.Decimal) decimal.Decimal {
	if !leverage.IsPositive() {
		return decimal.Zero
	}
	return decimal.NewFromInt(100).Div(leverage)
}

func nonZeroLevels(slPx, tpPx decimal.Decimal) conductor.Levels {
	return conductor.Levels{SLPx: nonZeroPx(slPx), TPPx: nonZeroPx(tpPx)}
}

// runUpdates is RealTrader's in-trade half of the lifecycle (CLAUDE.md §15.12), mirroring
// PaperTrader.runUpdates. Purely local per §3a's correction: no exchange call for an SL/TP move,
// since RealTrader watches SL/TP itself rather than resting a conditional order on OKX.
func (e *RealTrader) runUpdates(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) {
	open, err := e.openPositions(ctx)
	if err != nil {
		logger.Warn("real updates: list open positions failed", "instId", e.InstID, "error", err)
		return
	}
	if len(open) == 0 {
		return
	}
	if e.Model == nil {
		return
	}

	obs := e.buildObservation(ctx, bar, price, logger)
	obs.Category = domain.CategoryUpdate
	now := time.Now()

	for _, o := range open {
		pnl := unrealizedPnLPct(o, price)
		if !e.conductor().ShouldUpdate(o.ID, pnl, now) {
			continue
		}

		obs.OrderID = o.ID
		obs.PositionState = positionStateOf(o, price)
		obs.Signal = e.carriedSignalFor(bar)

		action, err := e.Model.Predict(ctx, obs)
		if err != nil {
			logger.Warn("real updates: predict failed", "instId", e.InstID, "orderId", o.ID, "error", err)
			continue
		}

		switch action.Action {
		case domain.ActionClose:
			e.closeEarly(ctx, o, price, logger)
		case domain.ActionUpdate:
			e.applyRealAdjustment(ctx, o, action, price, logger)
		}
	}
}

// applyRealAdjustment is RealTrader's no-fork SL/TP edit: computeAdjustedLevels (the shared free
// function, plan commit 2) is the SAME ratchet-checked computation PaperTrader.applyAdjustment
// uses, but the IO here is real-trading-scoped (still UpdatePaperOrderSLTP/
// RecordPaperOrderAdjustment — paper_orders/paper_order_adjustments are mode-generic tables, so no
// new persistence is needed for real rows). No exchange call: the new levels take effect on this
// engine's own next tick via monitorOpenPositions.
func (e *RealTrader) applyRealAdjustment(ctx context.Context, o port.PaperOrder, action *domain.Action, price decimal.Decimal, logger *slog.Logger) {
	newSL, newTP, changed := computeAdjustedLevels(o, action, price)
	if !changed {
		return
	}
	if err := e.Repo.UpdatePaperOrderSLTP(ctx, o.ID, newSL, newTP); err != nil {
		logger.Warn("real updates: sl/tp update failed", "instId", e.InstID, "orderId", o.ID, "error", err)
		return
	}
	if !samePriceOrNil(newSL, o.SLPx) {
		if err := e.Repo.RecordPaperOrderAdjustment(ctx, o.ID, "sl", o.SLPx, newSL, "model"); err != nil {
			logger.Warn("real updates: record sl adjustment failed", "instId", e.InstID, "orderId", o.ID, "error", err)
		}
	}
	if !samePriceOrNil(newTP, o.TPPx) {
		if err := e.Repo.RecordPaperOrderAdjustment(ctx, o.ID, "tp", o.TPPx, newTP, "model"); err != nil {
			logger.Warn("real updates: record tp adjustment failed", "instId", e.InstID, "orderId", o.ID, "error", err)
		}
	}
	logger.Info("real sl/tp adjustment applied", "instId", e.InstID, "orderId", o.ID, "newSL", newSL, "newTP", newTP)
}

// closeEarly closes a real position at market because the model asked to (CLAUDE.md §15.12). Same
// RLEarlyClose gate as PaperTrader's equivalent — the one lifecycle action that destroys the
// counterfactual.
func (e *RealTrader) closeEarly(ctx context.Context, o port.PaperOrder, price decimal.Decimal, logger *slog.Logger) {
	if !e.RLEarlyClose {
		return
	}
	if err := e.closeReal(ctx, o, price, conductor.CloseReasonRLEarly, logger); err != nil {
		logger.Error("real updates: early close failed", "instId", e.InstID, "orderId", o.ID, "error", err)
	}
}

// monitorOpenPositions is RealTrader's SL/TP-touch and timeout check — the SAME in-process
// tick-driven mechanism PaperTrader.monitorOpenOrders already uses (CLAUDE.md §27.3's correction:
// no OKX conditional/algo order, this process watches its own open positions on every tick).
func (e *RealTrader) monitorOpenPositions(ctx context.Context, price decimal.Decimal, logger *slog.Logger) error {
	open, err := e.openPositions(ctx)
	if err != nil {
		return fmt.Errorf("list open real positions: %w", err)
	}

	now := time.Now()
	for _, o := range open {
		reason, hit := closeReason(o, price)
		if !hit && e.conductor().IsTimedOut(o.OpenedAt, now) {
			reason, hit = conductor.CloseReasonTimeout, true
		}
		if !hit {
			continue
		}
		if err := e.closeReal(ctx, o, price, reason, logger); err != nil {
			logger.Error("failed to close real order", "id", o.ID, "instId", e.InstID, "error", err)
		}
	}
	return nil
}

// closeReal is RealTrader's single close path (mirrors PaperTrader.closeOrder — every close, SL/TP
// touch, timeout, or model-driven early close, goes through here so nothing can skip the terminal
// model call). Exchange-first: the flattening market order is placed BEFORE the DB is marked
// closed, so a DB failure never leaves the system believing a still-open real position is closed
// (CLAUDE.md §27.3). If the exchange already reports the position flat (a reconciliation-poll-
// detected close, see reconcile), skipExchange lets the flattening order be skipped since there is
// nothing left to close on OKX's side — the DB/reward/audit consequences are identical either way.
func (e *RealTrader) closeReal(ctx context.Context, o port.PaperOrder, price decimal.Decimal, reason string, logger *slog.Logger) error {
	return e.closeRealWith(ctx, o, price, reason, false, logger)
}

func (e *RealTrader) closeRealWith(ctx context.Context, o port.PaperOrder, price decimal.Decimal, reason string, skipExchange bool, logger *slog.Logger) error {
	if !skipExchange {
		side := "sell"
		if o.Side == "sell" {
			side = "buy"
		}
		closePrice := o.EntryPx
		if price.IsPositive() {
			closePrice = price
		}
		inst, err := e.instrumentMeta()
		if err != nil {
			return fmt.Errorf("fetch instrument metadata: %w", err)
		}
		sz := sizeToContracts(o.Size, closePrice, inst)
		req := domain.OrderRequest{InstID: e.execInstID(), TdMode: e.TdMode, Side: side, OrdType: "market", Sz: sz}
		if e.PosMode == "long_short" {
			req.PosSide = posSideFor(signedNotionalForSide(o.Side))
		}
		result, err := e.Exchange.PlaceOrder(req)
		if err != nil {
			return fmt.Errorf("flatten position: %w", err)
		}
		if result != nil && result.SCode != "0" {
			return fmt.Errorf("flatten order rejected: sCode=%s sMsg=%s", result.SCode, result.SMsg)
		}

		// CLAUDE.md §27.5: confirm the flatten actually filled before marking the DB row closed —
		// an unfilled or partially-filled flatten leaves real exposure still open on the exchange,
		// and closing the DB row in that case would make the system believe a position is flat when
		// it isn't. A partial fill here is deliberately NOT split into a smaller closed row (unlike
		// a partial OPEN fill, which records the smaller size actually acquired): a partially-
		// flattened position is still one open position with a reduced size, which the next tick's
		// ordinary SL/TP/timeout check and the reconciliation poll both already handle correctly
		// without new bookkeeping — this only needs to not lie about it being closed.
		if result != nil && result.OrdID != "" {
			status, err := e.waitForFill(ctx, result.OrdID, logger)
			if err != nil {
				return fmt.Errorf("wait for flatten fill: %w", err)
			}
			if !status.IsFilled() {
				return fmt.Errorf("flatten order for %d not fully filled (state=%s, filled=%s/%s); "+
					"position may still be open on the exchange, not marking closed",
					o.ID, status.State, status.AccFillSz, status.Sz)
			}
		}
	}

	pnl := realizedPnL(o, price)
	if err := e.Repo.ClosePaperOrder(ctx, o.ID, price, reason, pnl); err != nil {
		return err
	}
	metrics.PaperOrdersClosedTotal.WithLabelValues(e.InstID, reason).Inc()
	metrics.PaperOrdersRealizedPnL.WithLabelValues(e.InstID).Add(pnl.InexactFloat64())
	logger.Info("closed real order", "id", o.ID, "instId", e.InstID, "reason", reason, "closePx", price, "pnl", pnl)
	e.publishOrderEvent(ctx, "closed", o.ID, logger)

	e.conductor().Forget(o.ID)
	e.reportTerminalReal(ctx, o, price, pnl, reason, logger)

	if e.AccountInitialUSD.IsPositive() {
		orderID := o.ID
		if _, _, err := e.Repo.ApplyRealizedPnL(ctx, e.accountMode(), pnl, &orderID, e.InstID); err != nil {
			logger.Error("failed to apply realized pnl to account", "instId", e.InstID, "error", err)
		}
	}
	return nil
}

// reportTerminalReal delivers a closed real trade's outcome to the model — same terminal-call
// contract as PaperTrader.reportTerminal (CLAUDE.md §15.10: the close event IS the reward).
func (e *RealTrader) reportTerminalReal(ctx context.Context, o port.PaperOrder, closePx, pnl decimal.Decimal, closeReason string, logger *slog.Logger) {
	if e.Model == nil {
		return
	}
	category := conductor.TerminalCategory(closeReason)
	if category == "" {
		return
	}
	obs := e.buildObservation(ctx, e.decisionBar(), closePx, logger)
	obs.Category = category
	obs.OrderID = o.ID
	obs.Signal = e.carriedSignalFor(e.decisionBar())
	ps := positionStateOf(o, closePx)
	ps.RealizedPnLUSD = pnl
	obs.PositionState = ps
	if _, err := e.Model.Predict(ctx, obs); err != nil {
		logger.Warn("real updates: terminal report failed; this trade will not train the model",
			"instId", e.InstID, "orderId", o.ID, "category", category, "error", err)
	}
}

func (e *RealTrader) carriedSignalFor(bar string) *domain.StrategySignal {
	sig, ok := e.conductor().CarriedSignal(e.InstID, bar)
	if !ok {
		return nil
	}
	return &sig
}

func (e *RealTrader) publishOrderEvent(ctx context.Context, eventType string, orderID int64, logger *slog.Logger) {
	if e.OrderEvents == nil {
		return
	}
	event := PaperOrderEvent{Type: eventType, OrderID: orderID, InstID: e.InstID}
	if err := e.OrderEvents.Publish(ctx, e.InstID, event); err != nil {
		logger.Warn("failed to publish real order event", "type", eventType, "orderId", orderID, "instId", e.InstID, "error", err)
	}
}

// buildObservation assembles the observation for this token, reusing the same shape PaperTrader
// sends — CLAUDE.md §27's plan §6: AccountEquityUSD/OpenExposureUSD are what differ from paper's
// version (ground-truth exchange values here, not this engine's own bookkeeping), everything else
// (candle window, strategy signals, price context, token identity) is identical logic.
func (e *RealTrader) buildObservation(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) domain.Observation {
	view := e.marketView(bar)
	window := view.Candles

	tb := domain.TimeframeBlock{Bar: bar, PriceContext: buildPriceContext(window)}
	for _, a := range e.Strategies {
		if a.Bar != bar {
			continue
		}
		sig, err := strategy.EvaluateWith(a.Strategy, view)
		if err != nil {
			continue
		}
		resolved := sig.ResolveLevels(price)
		tb.StrategySignals = append(tb.StrategySignals, domain.StrategySignal{
			StrategyID: a.StrategyID,
			Side:       string(sig.Side),
			Confidence: sig.Confidence,
			EntryPx:    resolved.EntryPx,
			SLPx:       resolved.SLPx,
			TPPx:       resolved.TPPx,
			Kind:       a.Kind,
			Bar:        a.Bar,
		})
	}

	obs := domain.Observation{
		SchemaVersion:     domain.ObservationSchemaVersion,
		InstID:            e.InstID,
		ActiveTokens:      e.ActiveTokens,
		LastPrice:         price,
		Timeframes:        []domain.TimeframeBlock{tb},
		AccountInitialUSD: e.AccountInitialUSD,
		Category:          domain.CategoryUpdate,
	}

	// Ground truth from the exchange, not GetAccountEquity's bookkeeping row — real trading does
	// not own this number the way paper trading owns its shared account (CLAUDE.md §27's plan §6:
	// this is the one spot flagged as easy to get wrong by careless reuse).
	balances, err := e.Exchange.GetBalance(e.settleCcy())
	if err != nil {
		logger.Warn("real observation: get balance failed", "instId", e.InstID, "error", err)
	} else if len(balances) > 0 {
		obs.AccountEquityUSD = balances[0].Eq
	}
	obs.OpenExposureUSD = e.openExposureReal(ctx, logger)

	return obs
}

// openExposureReal sums the notional of every open real position across ALL tokens this mode
// trades, mirroring PaperTrader.openExposure but scoped to Mode via ListPositions rather than
// summing every mode's rows.
func (e *RealTrader) openExposureReal(ctx context.Context, logger *slog.Logger) decimal.Decimal {
	openOnly := true
	positions, err := e.Repo.ListPositions(ctx, port.PositionFilter{Mode: e.accountMode(), Open: &openOnly})
	if err != nil {
		logger.Warn("real observation: list open positions failed", "mode", e.accountMode(), "error", err)
		return decimal.Zero
	}
	var total decimal.Decimal
	for _, p := range positions {
		total = total.Add(p.Size)
	}
	return total
}

// runReconcileLoop periodically compares this process's own open real positions against OKX's
// authoritative GetPositions/GetBalance response (CLAUDE.md §27.3/§27.6), on a fixed 1-minute
// cadence (reconcileInterval) — separate from and much slower than the tick-driven SL/TP monitor,
// since this poll exists only to catch drift, not to drive trading.
func (e *RealTrader) runReconcileLoop(ctx context.Context, logger *slog.Logger) error {
	ticker := time.NewTicker(e.reconcileInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			e.reconcile(ctx, logger)
		}
	}
}

// reconcile is the one-shot comparison runReconcileLoop calls on each tick of its own ticker.
// Exported as a method (not folded into the loop) so tests can call it directly without waiting on
// a real ticker.
func (e *RealTrader) reconcile(ctx context.Context, logger *slog.Logger) {
	remotePositions, err := e.Exchange.GetPositions(e.execInstType())
	if err != nil {
		logger.Warn("reconcile: get positions failed", "instId", e.InstID, "error", err)
		return
	}
	var remote *domain.Position
	for i := range remotePositions {
		if remotePositions[i].InstID == e.execInstID() && !remotePositions[i].Pos.IsZero() {
			remote = &remotePositions[i]
			break
		}
	}

	local, err := e.openPositions(ctx)
	if err != nil {
		logger.Warn("reconcile: list local open positions failed", "instId", e.InstID, "error", err)
		return
	}

	switch {
	case remote == nil && len(local) > 0:
		// OKX shows flat but we still think a position is open — a liquidation, a manual close on
		// OKX's own UI/app, or anything else that happened outside this system. Route through the
		// normal close path (skipExchange: nothing left to flatten) so the DB/reward/audit
		// consequences are identical to a tick-driven close, just discovered a poll interval late.
		logger.Warn("reconcile: exchange reports flat but local state shows an open position; closing locally",
			"instId", e.InstID, "localOrders", len(local))
		for _, o := range local {
			// No live tick price is available on this path; use the order's own entry price as the
			// best available estimate for the realized-PnL calculation rather than blocking the
			// close on having a fresher number.
			if err := e.closeRealWith(ctx, o, o.EntryPx, conductor.CloseReasonManual, true, logger); err != nil {
				logger.Error("reconcile: failed to close locally-stale position", "id", o.ID, "error", err)
			}
		}
	case remote != nil && len(local) == 0:
		logger.Error("reconcile: exchange reports an open position this system has no record of",
			"instId", e.InstID, "remoteSize", remote.Pos, "remoteSide", remote.PosSide)
		e.RiskManager.Halt(fmt.Sprintf("reconcile: untracked open position on %s (exchange reports %s %s)",
			e.InstID, remote.Pos.String(), remote.PosSide))
	case remote != nil && len(local) > 0:
		// Both sides agree a position exists; check it's the SAME position. Compare notional as the
		// simplest available cross-check (size in contracts vs. this system's own USD notional isn't
		// directly comparable without the instrument's contract multiplier, which isn't wired in yet
		// per trade.go's own long-standing note) — a sign/side mismatch is the concrete, checkable
		// case worth alerting on now.
		localSide := local[0].Side
		remoteSide := "buy"
		if remote.PosSide == "short" || remote.Pos.IsNegative() {
			remoteSide = "sell"
		}
		if localSide != remoteSide {
			logger.Error("reconcile: side mismatch between local record and exchange",
				"instId", e.InstID, "localSide", localSide, "remoteSide", remoteSide, "remotePosSide", remote.PosSide)
			e.RiskManager.Halt(fmt.Sprintf("reconcile: side mismatch on %s (local %s, exchange %s)",
				e.InstID, localSide, remoteSide))
		}
	}

	if balances, err := e.Exchange.GetBalance(e.settleCcy()); err != nil {
		logger.Warn("reconcile: get balance failed", "instId", e.InstID, "error", err)
	} else if len(balances) > 0 {
		e.recordEquityReal(ctx, balances[0].Eq, logger)
	}
}

// recordEquityReal mirrors trade.go's Trader.recordEquity: the exchange's reported equity is
// ground truth, so this only observes it and records the delta from what was last stored, never
// applying a top-up/reset the way paper mode's balance does. Best-effort.
func (e *RealTrader) recordEquityReal(ctx context.Context, equity decimal.Decimal, logger *slog.Logger) {
	if e.Repo == nil {
		return
	}
	initial := e.AccountInitialUSD
	if !initial.IsPositive() {
		initial = equity
	}
	acct, err := e.Repo.GetAccountEquity(ctx, e.accountMode(), initial)
	if err != nil {
		logger.Warn("reconcile: equity timeline read failed", "mode", e.accountMode(), "error", err)
		return
	}
	delta := equity.Sub(acct.EquityUSD)
	if delta.IsZero() {
		return
	}
	if _, _, err := e.Repo.ApplyRealizedPnL(ctx, e.accountMode(), delta, nil, e.InstID); err != nil {
		logger.Warn("reconcile: equity timeline write failed", "mode", e.accountMode(), "error", err)
	}
}
