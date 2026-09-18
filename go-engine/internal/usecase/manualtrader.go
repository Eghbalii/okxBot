package usecase

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// ManualTrader is the discretionary/operator-placed order lifecycle (docs/MANUAL_TRADE_PLAN.md),
// deliberately a SEPARATE type from RealTrader rather than a mode on it — a manual order has no
// strategy signal, no conductor category, no RL model call, and no observation vector, so bolting
// it onto RealTrader's own open/update/close methods would mean threading a "this call has none of
// the things I normally require" flag through code that is not written to expect that.
//
// It reuses RealTrader's PROVEN safety mechanics rather than reinventing them: the same
// place-then-confirm-fill sequence (waitForFill), the same size->contracts conversion
// (sizeToContracts), and the same exchange-side-protection-or-close-immediately posture
// (placeProtection's reasoning, reproduced here as placeManualProtection). Where it genuinely
// differs is §8.4's settled behavior: a manual order and a strategy position can share ONE net
// exchange position, and OKX's conditional orders for a position do not stack cleanly — so before
// placing its own protective order, ManualTrader checks whether RealTrader already protects this
// token and, if so, deliberately does not place a second one.
//
// One ManualTrader instance is account-wide (unlike RealTrader, which is one-per-configured-
// instrument) — a manual order can be placed on ANY token the operator picks, not just the
// pre-configured roster (docs/MANUAL_TRADE_PLAN.md §1's rejected alternative explains why this
// could not simply spin up a RealTrader per manual token instead).
type ManualTrader struct {
	Repo     port.Repository
	Exchange port.ExchangeClient
	Logger   *slog.Logger

	ExecInstType string // e.g. "SWAP" or "FUTURES"; defaults to "SWAP" when empty
	SettleCcy    string // e.g. "USDT" or "USDC"; defaults to "USDT" when empty
	TdMode       string // "cross" or "isolated"
	PosMode      string // "net" or "long_short" (hedge mode)

	// ExecInstIDFor resolves a short symbol ("BTC") to the instrument orders actually execute
	// against (CLAUDE.md §33.4) — required since a manual order's token is chosen live, not fixed
	// at construction the way RealTrader.ExecInstID is.
	ExecInstIDFor func(symbol string) (string, error)

	// RealTraderProtects reports whether RealTrader already holds a live protective algo order on
	// instID (§8.4). Wired by cmd/trader to a closure reading the shared engines map — kept as a
	// function rather than a direct dependency on *RealTrader/map[string]*RealTrader so this type
	// does not need to know cmd/trader's own wiring shape, only the one fact it needs from it.
	RealTraderProtects func(instID string) bool

	FillTimeout time.Duration

	OrderEvents port.MarketDataPublisher

	// PollInterval is how often Run polls manual_order_intents for pending rows. Defaults to
	// DefaultManualIntentPollInterval when unset.
	PollInterval time.Duration

	instrumentCache map[string]instrumentCacheEntry
}

type instrumentCacheEntry struct {
	inst domain.Instrument
	err  error
}

// DefaultManualIntentPollInterval is how often Run checks for a new manual-order request — fast
// relative to the model's own decision cadence, since an operator clicking "Buy" expects
// near-immediate feedback (docs/MANUAL_TRADE_PLAN.md §4).
const DefaultManualIntentPollInterval = 1500 * time.Millisecond

func (m *ManualTrader) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *ManualTrader) execInstType() string {
	if m.ExecInstType != "" {
		return m.ExecInstType
	}
	return "SWAP"
}

func (m *ManualTrader) settleCcy() string {
	if m.SettleCcy != "" {
		return m.SettleCcy
	}
	return "USDT"
}

func (m *ManualTrader) pollInterval() time.Duration {
	if m.PollInterval > 0 {
		return m.PollInterval
	}
	return DefaultManualIntentPollInterval
}

func (m *ManualTrader) fillTimeout() time.Duration {
	if m.FillTimeout > 0 {
		return m.FillTimeout
	}
	return DefaultFillTimeout
}

func (m *ManualTrader) execInstID(symbol string) (string, error) {
	if m.ExecInstIDFor == nil {
		return symbol, nil
	}
	return m.ExecInstIDFor(symbol)
}

// instrumentMeta fetches and caches execInstID's contract-shape metadata, mirroring
// RealTrader.instrumentMeta but keyed per-instrument since one ManualTrader serves every token
// rather than one fixed instrument.
func (m *ManualTrader) instrumentMeta(execInstID string) (domain.Instrument, error) {
	if m.instrumentCache == nil {
		m.instrumentCache = make(map[string]instrumentCacheEntry)
	}
	if entry, ok := m.instrumentCache[execInstID]; ok {
		return entry.inst, entry.err
	}
	inst, err := m.Exchange.GetInstrument(m.execInstType(), execInstID)
	m.instrumentCache[execInstID] = instrumentCacheEntry{inst: inst, err: err}
	return inst, err
}

func (m *ManualTrader) publishOrderEvent(ctx context.Context, eventType string, orderID int64, instID string, logger *slog.Logger) {
	if m.OrderEvents == nil {
		return
	}
	event := PaperOrderEvent{Type: eventType, OrderID: orderID, InstID: instID}
	if err := m.OrderEvents.Publish(ctx, instID, event); err != nil {
		logger.Warn("failed to publish manual order event", "type", eventType, "orderId", orderID, "instId", instID, "error", err)
	}
}

// Run polls manual_order_intents until ctx is cancelled, claiming and processing every pending
// request it finds. Errors from one processing pass are logged, not returned — a single bad intent
// must not stop the loop from serving the next one.
func (m *ManualTrader) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.pollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.processPendingIntents(ctx)
			m.ProcessCloseRequests(ctx)
		}
	}
}

func (m *ManualTrader) processPendingIntents(ctx context.Context) {
	logger := m.logger()
	intents, err := m.Repo.ClaimPendingManualOrderIntents(ctx)
	if err != nil {
		logger.Warn("manual trader: claim pending intents failed", "error", err)
		return
	}
	for _, in := range intents {
		m.processIntent(ctx, in, logger)
	}
}

func (m *ManualTrader) processIntent(ctx context.Context, in port.ManualOrderIntent, logger *slog.Logger) {
	orderID, err := m.openFromIntent(ctx, in, logger)
	if err != nil {
		msg := err.Error()
		if finErr := m.Repo.FinishManualOrderIntent(ctx, in.ID, nil, &msg); finErr != nil {
			logger.Warn("manual trader: failed to record failed intent", "intentId", in.ID, "error", finErr)
		}
		logger.Error("manual order open failed", "intentId", in.ID, "instId", in.InstID, "error", err)
		return
	}
	if err := m.Repo.FinishManualOrderIntent(ctx, in.ID, orderID, nil); err != nil {
		logger.Warn("manual trader: failed to record finished intent", "intentId", in.ID, "error", err)
	}
}

// openFromIntent runs the full open sequence for one claimed intent: resolve the execution
// instrument, set leverage, size, place the order, confirm its fill (or accept it resting if it's
// a limit order that hasn't filled yet), then protect it — mirroring RealTrader.openReal's proven
// sequence (docs/MANUAL_TRADE_PLAN.md §4).
func (m *ManualTrader) openFromIntent(ctx context.Context, in port.ManualOrderIntent, logger *slog.Logger) (*int64, error) {
	execInstID, err := m.execInstID(in.InstID)
	if err != nil {
		return nil, fmt.Errorf("resolve execution instrument for %s: %w", in.InstID, err)
	}

	if in.Leverage.IsPositive() {
		req := domain.LeverageChange{InstID: execInstID, Lever: in.Leverage, MgnMode: m.TdMode}
		if m.PosMode == "long_short" {
			req.PosSide = posSideFor(signedNotionalForSide(in.Side))
		}
		if err := m.Exchange.SetLeverage(req); err != nil {
			return nil, fmt.Errorf("set leverage: %w", err)
		}
	}

	inst, err := m.instrumentMeta(execInstID)
	if err != nil {
		return nil, fmt.Errorf("fetch instrument metadata: %w", err)
	}

	// A market order needs a reference price to convert notional into contracts; a limit order
	// uses the operator's own given price for that same conversion, since that IS the price it
	// will fill at if it fills at all.
	refPx := in.LimitPx
	if refPx == nil || !refPx.IsPositive() {
		ticker, err := m.Exchange.GetTicker(execInstID)
		if err != nil {
			return nil, fmt.Errorf("fetch reference price: %w", err)
		}
		if !ticker.Last.IsPositive() {
			return nil, fmt.Errorf("exchange reported no usable last price for %s", execInstID)
		}
		last := ticker.Last
		refPx = &last
	}
	sz := sizeToContracts(in.SizeUSD, in.Leverage, *refPx, inst)
	if sz.IsZero() || (inst.MinSz.IsPositive() && sz.LessThan(inst.MinSz)) {
		return nil, fmt.Errorf("sized order (%s contracts) is below the instrument minimum (%s)", sz, inst.MinSz)
	}

	orderType := "market"
	var limitPx decimal.Decimal
	if in.OrderType == "limit" {
		orderType = "limit"
		if in.LimitPx == nil || !in.LimitPx.IsPositive() {
			return nil, errors.New("limit order requires a positive limit price")
		}
		limitPx = inst.RoundPriceToTick(*in.LimitPx)
	}

	req := domain.OrderRequest{InstID: execInstID, TdMode: m.TdMode, Side: in.Side, OrdType: orderType, Sz: sz, Px: limitPx}
	if m.PosMode == "long_short" {
		req.PosSide = posSideFor(signedNotionalForSide(in.Side))
	}
	result, err := m.Exchange.PlaceOrder(req)
	if err != nil {
		return nil, fmt.Errorf("place order: %w", err)
	}
	if result != nil && result.SCode != "0" {
		return nil, fmt.Errorf("order rejected: sCode=%s sMsg=%s", result.SCode, result.SMsg)
	}

	order := port.ManualOrder{
		InstID:     in.InstID,
		ExecInstID: execInstID,
		Side:       in.Side,
		OrderType:  in.OrderType,
		LimitPx:    in.LimitPx,
		SLPx:       in.SLPx,
		TPPx:       in.TPPx,
		Size:       in.SizeUSD,
		Leverage:   in.Leverage,
		Status:     "pending",
	}
	if result != nil && result.OrdID != "" {
		ordID := result.OrdID
		order.ExchangeOrderID = &ordID
	}

	// Persist the row IMMEDIATELY after the exchange accepts the order, before waiting for its
	// fill (mirrors RealTrader.openReal's own reasoning, CLAUDE.md real-trading readiness plan):
	// the order is visible on the panel for the whole in-flight window, and — just as importantly
	// for the reconcile-halt problem this whole design exists to avoid (docs/MANUAL_TRADE_PLAN.md
	// §1/§4) — this system knows about the position from the moment it exists, before the next
	// reconcile pass could ever see it as untracked.
	localID, err := m.Repo.OpenManualOrder(ctx, order)
	if err != nil {
		return nil, fmt.Errorf("persist manual order: %w", err)
	}
	order.ID = localID

	if order.ExchangeOrderID == nil {
		// No exchange order id at all — nothing to wait on. Treat as filled immediately, the same
		// edge case RealTrader.openReal handles the same way.
		return m.finishOpen(ctx, order, "filled", nil, nil, domain.Instrument{}, logger)
	}

	if err := m.Repo.UpdateManualOrderStatus(ctx, localID, "opening", nil, nil, nil); err != nil {
		logger.Warn("manual trader: failed to mark order opening", "id", localID, "error", err)
	}

	var status domain.OrderStatus
	if in.OrderType == "limit" {
		// A limit order is allowed to rest unfilled INDEFINITELY (docs/MANUAL_TRADE_PLAN.md §8.1) —
		// unlike a market order, waiting toward waitForFill's fixed timeout and then canceling would
		// be actively wrong here: a limit order sitting unfilled for minutes/hours is the normal,
		// expected case, not a failure to give up on. So this only PROBES briefly for the common
		// case of an immediate/near-immediate fill (a limit price that happened to already be
		// marketable), and otherwise leaves the order resting on the exchange with no cancellation.
		status, err = m.probeLimitFill(ctx, execInstID, *order.ExchangeOrderID, logger)
	} else {
		status, err = m.waitForFill(ctx, execInstID, *order.ExchangeOrderID, logger)
	}
	if err != nil {
		if setErr := m.Repo.SetManualOrderError(ctx, localID, fmt.Sprintf("wait for fill: %v", err)); setErr != nil {
			logger.Warn("manual trader: failed to record open error", "id", localID, "error", setErr)
		}
		return &localID, fmt.Errorf("wait for fill: %w", err)
	}

	switch {
	case status.IsFilled():
		return m.finishOpen(ctx, order, "filled", &status, refPx, inst, logger)
	case status.AccFillSz.IsPositive():
		logger.Warn("manual open: order partially filled before timeout/cancel",
			"instId", in.InstID, "ordId", *order.ExchangeOrderID, "requestedSz", sz, "filledSz", status.AccFillSz)
		return m.finishOpen(ctx, order, "partial", &status, refPx, inst, logger)
	case in.OrderType == "limit":
		// Still resting after the brief probe above — leave it exactly as it is, with no entry
		// price yet, for the panel to show as "waiting to fill" and for a future cancel/close
		// action to act on. Never canceled: that is the whole point of a limit order.
		if err := m.Repo.UpdateManualOrderStatus(ctx, localID, "resting", nil, nil, nil); err != nil {
			logger.Warn("manual trader: failed to mark order resting", "id", localID, "error", err)
		}
		logger.Info("manual limit order resting on the exchange, unfilled", "id", localID, "instId", in.InstID)
		return &localID, nil
	default:
		// Market order, never filled at all before the timeout (waitForFill cancels it) — no
		// position exists. The row stays, marked canceled, visible rather than silently dropped.
		if err := m.Repo.UpdateManualOrderStatus(ctx, localID, "canceled", nil, nil, nil); err != nil {
			logger.Warn("manual trader: failed to mark order canceled", "id", localID, "error", err)
		}
		logger.Info("manual open: order canceled unfilled, no position opened", "id", localID, "instId", in.InstID)
		return &localID, nil
	}
}

// waitForFill polls Exchange.GetOrder until ordID reaches a terminal state or fillTimeout()
// elapses, mirroring RealTrader.waitForFill exactly (same poll interval, same cancel-on-timeout
// behavior, same "timeout is not an error" contract) — kept as its own copy rather than an
// extraction shared with RealTrader, since RealTrader's version reads e.execInstID()/e.Exchange
// off *RealTrader and this type has no *RealTrader to borrow the method from; the logic itself
// must stay identical, which is why every constant/branch below matches realtrader.go's version
// line for line.
//
// Used for MARKET orders only — see probeLimitFill for why a limit order needs a different wait
// strategy entirely, not just a longer timeout.
func (m *ManualTrader) waitForFill(ctx context.Context, execInstID, ordID string, logger *slog.Logger) (domain.OrderStatus, error) {
	deadline := time.Now().Add(m.fillTimeout())
	var last domain.OrderStatus
	for {
		status, err := m.Exchange.GetOrder(execInstID, ordID)
		if err != nil {
			logger.Warn("manual fill-timeout: get order status failed, will retry", "instId", execInstID, "ordId", ordID, "error", err)
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

	logger.Warn("manual fill-timeout: order not filled within timeout, canceling",
		"instId", execInstID, "ordId", ordID, "timeout", m.fillTimeout(), "lastState", last.State)
	if err := m.Exchange.CancelOrder(execInstID, ordID); err != nil {
		logger.Error("manual fill-timeout: cancel failed", "instId", execInstID, "ordId", ordID, "error", err)
		return last, fmt.Errorf("order %s not filled within %s and cancel failed: %w", ordID, m.fillTimeout(), err)
	}
	return last, nil
}

// limitProbeWindow/limitProbeInterval bound the brief check for a limit order that happens to be
// immediately (or near-immediately) marketable — e.g. a "limit buy" placed above the current ask.
// Deliberately much shorter than fillTimeout(): the common case this probe exists for resolves in
// well under a second, and anything still unresolved after this window is exactly the normal
// "resting, waiting for the market to reach it" case that must NOT be timed out or canceled.
const (
	limitProbeWindow   = 3 * time.Second
	limitProbeInterval = 500 * time.Millisecond
)

// probeLimitFill checks briefly whether a just-placed LIMIT order has already filled, then returns
// whatever it last observed WITHOUT canceling the order if it hasn't — a limit order is allowed to
// rest unfilled indefinitely (docs/MANUAL_TRADE_PLAN.md §8.1), which is a fundamentally different
// contract from waitForFill's "give up and cancel after a timeout". Reusing waitForFill here (even
// with a short deadline) would be wrong for exactly one reason: on timeout it CANCELS the order,
// and a limit order timing out its probe is not a failure to cancel, it is the normal steady state.
func (m *ManualTrader) probeLimitFill(ctx context.Context, execInstID, ordID string, logger *slog.Logger) (domain.OrderStatus, error) {
	deadline := time.Now().Add(limitProbeWindow)
	var last domain.OrderStatus
	for {
		status, err := m.Exchange.GetOrder(execInstID, ordID)
		if err != nil {
			logger.Warn("manual limit probe: get order status failed, will retry", "instId", execInstID, "ordId", ordID, "error", err)
		} else {
			last = status
			if status.IsTerminal() || status.AccFillSz.IsPositive() {
				return last, nil
			}
		}
		if time.Now().After(deadline) {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(limitProbeInterval):
		}
	}
}

// finishOpen records a fill outcome (filled/partial), places exchange-side protection (or defers
// to RealTrader's if it already protects this token, §8.4), and returns the local order id.
func (m *ManualTrader) finishOpen(ctx context.Context, order port.ManualOrder, finalStatus string, status *domain.OrderStatus, refPx *decimal.Decimal, inst domain.Instrument, logger *slog.Logger) (*int64, error) {
	var entryPx, size, contracts *decimal.Decimal
	if status != nil {
		if status.AvgPx.IsPositive() {
			px := status.AvgPx
			entryPx = &px
		} else if refPx != nil {
			entryPx = refPx
		}
		if status.AccFillSz.IsPositive() {
			fillSz := status.AccFillSz
			contracts = &fillSz
			if entryPx != nil {
				margin := marginFromFill(*status, *entryPx, order.Leverage, inst)
				if margin.IsPositive() {
					size = &margin
				}
			}
		}
	} else if refPx != nil {
		entryPx = refPx
	}

	if err := m.Repo.UpdateManualOrderStatus(ctx, order.ID, finalStatus, entryPx, size, contracts); err != nil {
		logger.Warn("manual trader: failed to update order status", "id", order.ID, "status", finalStatus, "error", err)
	}
	if entryPx != nil {
		order.EntryPx = entryPx
	}
	if contracts != nil {
		order.Contracts = contracts
	}

	// §8.4: if RealTrader already holds a live protective order on this token, this manual order
	// deliberately does NOT place a second one — OKX's conditional orders for a position don't
	// stack cleanly, and the two would otherwise contend over one net exchange position.
	if m.RealTraderProtects != nil && m.RealTraderProtects(order.InstID) {
		if err := m.Repo.SetManualOrderProtection(ctx, order.ID, nil, true); err != nil {
			logger.Warn("manual trader: failed to record shared-protection flag", "id", order.ID, "error", err)
		}
		logger.Info("manual order shares an exchange position already protected by a strategy; no separate SL/TP placed",
			"id", order.ID, "instId", order.InstID)
		metrics.PaperOrdersOpenedTotal.WithLabelValues("manual", order.InstID, order.Side).Inc()
		m.publishOrderEvent(ctx, "opened", order.ID, order.InstID, logger)
		id := order.ID
		return &id, nil
	}

	algoID, protErr := m.placeManualProtection(ctx, order, logger)
	if protErr != nil {
		metrics.RealUnprotectedClosedTotal.WithLabelValues(order.InstID).Inc()
		logger.Error("could not rest sl/tp on the exchange for a just-opened manual position; closing it immediately",
			"id", order.ID, "instId", order.InstID, "error", protErr)
		if closeErr := m.closeManual(ctx, order, "manual", logger); closeErr != nil {
			logger.Error("FAILED TO CLOSE AN UNPROTECTED MANUAL POSITION", "id", order.ID, "instId", order.InstID, "error", closeErr)
		}
		id := order.ID
		return &id, fmt.Errorf("place protection: %w", protErr)
	}
	if err := m.Repo.SetManualOrderProtection(ctx, order.ID, &algoID, false); err != nil {
		logger.Warn("manual trader: failed to record protection algo id", "id", order.ID, "error", err)
	}

	metrics.PaperOrdersOpenedTotal.WithLabelValues("manual", order.InstID, order.Side).Inc()
	logger.Info("opened manual order", "id", order.ID, "instId", order.InstID, "side", order.Side,
		"entry", order.EntryPx, "size", order.Size, "leverage", order.Leverage)
	m.publishOrderEvent(ctx, "opened", order.ID, order.InstID, logger)

	id := order.ID
	return &id, nil
}

// placeManualProtection mirrors RealTrader.placeProtection: both trigger prices ride on one OCO
// algo order, rounded to the instrument's own tick, retried once before giving up.
func (m *ManualTrader) placeManualProtection(ctx context.Context, o port.ManualOrder, logger *slog.Logger) (string, error) {
	if o.Contracts == nil || !o.Contracts.IsPositive() {
		return "", fmt.Errorf("manual order %d has no filled contracts to protect", o.ID)
	}
	inst, err := m.instrumentMeta(o.ExecInstID)
	if err != nil {
		return "", fmt.Errorf("fetch instrument metadata: %w", err)
	}
	req := domain.AlgoOrderRequest{
		InstID: o.ExecInstID,
		TdMode: m.TdMode,
		Side:   closingSide(o.Side),
		Sz:     *o.Contracts,
	}
	if m.PosMode == "long_short" {
		req.PosSide = posSideFor(signedNotionalForSide(o.Side))
	}
	if o.SLPx != nil && o.SLPx.IsPositive() {
		req.SLTriggerPx = inst.RoundPriceToTick(*o.SLPx)
	}
	if o.TPPx != nil && o.TPPx.IsPositive() {
		req.TPTriggerPx = inst.RoundPriceToTick(*o.TPPx)
	}
	if !req.SLTriggerPx.IsPositive() && !req.TPTriggerPx.IsPositive() {
		return "", fmt.Errorf("manual order %d has no stop or target to rest on the exchange", o.ID)
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		algoID, err := m.Exchange.PlaceAlgoOrder(req)
		if err == nil {
			metrics.RealProtectionPlacedTotal.WithLabelValues(o.InstID).Inc()
			logger.Info("rested sl/tp on the exchange for a manual order", "id", o.ID, "instId", o.InstID,
				"algoId", algoID, "sl", req.SLTriggerPx, "tp", req.TPTriggerPx)
			return algoID, nil
		}
		lastErr = err
		logger.Warn("failed to rest sl/tp on the exchange for a manual order", "id", o.ID, "instId", o.InstID, "attempt", attempt, "error", err)
	}
	metrics.RealProtectionFailedTotal.WithLabelValues(o.InstID).Inc()
	return "", fmt.Errorf("place protective order: %w", lastErr)
}

// closeManual flattens an open manual order at market, mirroring RealTrader.closeRealWith's
// exchange-first-then-database sequence (fill confirmation before the row is marked closed) and
// its idempotency guard (a second racing close path finds nothing to do rather than double-closing).
func (m *ManualTrader) closeManual(ctx context.Context, o port.ManualOrder, reason string, logger *slog.Logger) error {
	if o.Contracts == nil || !o.Contracts.IsPositive() {
		return fmt.Errorf("manual order %d has no filled contracts to close", o.ID)
	}
	side := "sell"
	if o.Side == "sell" {
		side = "buy"
	}
	req := domain.OrderRequest{InstID: o.ExecInstID, TdMode: m.TdMode, Side: side, OrdType: "market", Sz: *o.Contracts}
	if m.PosMode == "long_short" {
		req.PosSide = posSideFor(signedNotionalForSide(o.Side))
	}
	result, err := m.Exchange.PlaceOrder(req)
	if err != nil {
		return m.recordCloseError(ctx, o.ID, fmt.Errorf("flatten position: %w", err), logger)
	}
	if result != nil && result.SCode != "0" {
		return m.recordCloseError(ctx, o.ID, fmt.Errorf("flatten order rejected: sCode=%s sMsg=%s", result.SCode, result.SMsg), logger)
	}

	var closePx decimal.Decimal
	var exchangeFee *decimal.Decimal
	if result != nil && result.OrdID != "" {
		status, err := m.waitForFill(ctx, o.ExecInstID, result.OrdID, logger)
		if err != nil {
			return m.recordCloseError(ctx, o.ID, fmt.Errorf("wait for flatten fill: %w", err), logger)
		}
		if !status.IsFilled() {
			return m.recordCloseError(ctx, o.ID, fmt.Errorf(
				"flatten order for %d not fully filled (state=%s, filled=%s/%s); position may still be open, not marking closed",
				o.ID, status.State, status.AccFillSz, status.Sz), logger)
		}
		if status.AvgPx.IsPositive() {
			closePx = status.AvgPx
		}
		if !status.Fee.IsZero() {
			fee := status.Fee
			exchangeFee = &fee
		}
	}
	if !closePx.IsPositive() && o.EntryPx != nil {
		closePx = *o.EntryPx
	}

	pnl := decimal.Zero
	if o.EntryPx != nil && closePx.IsPositive() {
		direction := decimal.NewFromInt(1)
		if o.Side == "sell" {
			direction = decimal.NewFromInt(-1)
		}
		pnl = closePx.Sub(*o.EntryPx).Div(*o.EntryPx).Mul(direction).Mul(o.Size).Mul(o.Leverage)
		if exchangeFee != nil {
			pnl = pnl.Add(*exchangeFee)
		}
	}

	if err := m.Repo.CloseManualOrder(ctx, o.ID, closePx, reason, pnl, exchangeFee); err != nil {
		if errors.Is(err, port.ErrOrderAlreadyClosed) {
			logger.Info("manual order was already closed by another path; nothing to do", "id", o.ID, "instId", o.InstID, "reason", reason)
			return nil
		}
		return err
	}
	// Cancel any resting protective order this manual order owns — never RealTrader's, if this
	// order was flagged protected-by-strategy (§8.4), since that order still protects the
	// strategy's own slice of the shared position.
	if !o.ProtectedByStrategy && o.ExchangeAlgoOrderID != nil && *o.ExchangeAlgoOrderID != "" {
		if err := m.Exchange.CancelAlgoOrder(o.ExecInstID, *o.ExchangeAlgoOrderID); err != nil {
			logger.Warn("manual trader: failed to cancel protective order after close", "id", o.ID, "algoId", *o.ExchangeAlgoOrderID, "error", err)
		}
	}
	metrics.PaperOrdersClosedTotal.WithLabelValues(o.InstID, reason).Inc()
	metrics.PaperOrdersRealizedPnL.WithLabelValues(o.InstID).Add(pnl.InexactFloat64())
	logger.Info("closed manual order", "id", o.ID, "instId", o.InstID, "reason", reason, "closePx", closePx, "pnl", pnl)
	m.publishOrderEvent(ctx, "closed", o.ID, o.InstID, logger)
	return nil
}

func (m *ManualTrader) recordCloseError(ctx context.Context, id int64, cause error, logger *slog.Logger) error {
	if m.Repo != nil {
		if err := m.Repo.SetManualOrderError(ctx, id, cause.Error()); err != nil {
			logger.Warn("manual trader: failed to record close error", "id", id, "error", err)
		}
	}
	logger.Error("manual close failed", "id", id, "error", cause)
	return cause
}

// ProcessCloseRequests ends every manual order flagged manual_close_requested, ACROSS EVERY
// INSTRUMENT — this type is account-wide (unlike RealTrader, which is one-per-instrument), so
// unlike processPendingIntents' polling loop there is no per-instrument caller to drive this from;
// it must sweep the whole account itself.
//
// One flag serves two different actions, safely, because a row's status is never simultaneously
// "resting" and "filled/partial" (docs/MANUAL_TRADE_PLAN.md §8.1): a FILLED/PARTIAL order is
// flattened at market (closeManual); a still-RESTING (unfilled limit) order is CANCELED instead —
// a different exchange call (CancelOrder, not a flatten) and a different terminal state, so which
// action applies is read from the row's own current status at process time, never assumed from
// which button the panel happened to show.
//
// Queried with no Open filter (nil) deliberately: PositionFilter's Open semantics are defined
// around "is this a real position" (filled/partial), which would silently exclude resting orders —
// exactly the rows this sweep also needs to see.
func (m *ManualTrader) ProcessCloseRequests(ctx context.Context) {
	logger := m.logger()
	orders, err := m.Repo.ListManualOrders(ctx, port.PositionFilter{})
	if err != nil {
		logger.Warn("manual trader: list manual orders failed", "error", err)
		return
	}
	for _, o := range orders {
		if !o.ManualCloseRequested || o.ClosedAt != nil {
			continue
		}
		switch o.Status {
		case "filled", "partial":
			if err := m.closeManual(ctx, o, "manual", logger); err != nil {
				logger.Error("manual trader: close request failed", "id", o.ID, "instId", o.InstID, "error", err)
			}
		case "resting":
			if err := m.cancelManual(ctx, o, logger); err != nil {
				logger.Error("manual trader: cancel request failed", "id", o.ID, "instId", o.InstID, "error", err)
			}
		}
	}
}

// cancelManual cancels a still-resting (unfilled) limit order on the exchange, then records the
// cancellation — the exchange call CancelManualOrder's own repository-layer doc comment notes is
// needed but does not itself perform (that method is a pure DB write, matching CloseManualOrder's
// own split between "what happened on the exchange" and "recording it").
func (m *ManualTrader) cancelManual(ctx context.Context, o port.ManualOrder, logger *slog.Logger) error {
	if o.ExchangeOrderID != nil && *o.ExchangeOrderID != "" {
		if err := m.Exchange.CancelOrder(o.ExecInstID, *o.ExchangeOrderID); err != nil {
			return m.recordCloseError(ctx, o.ID, fmt.Errorf("cancel resting order: %w", err), logger)
		}
	}
	if err := m.Repo.CancelManualOrder(ctx, o.ID); err != nil {
		if errors.Is(err, port.ErrOrderAlreadyClosed) {
			logger.Info("manual order was already resolved by another path; nothing to cancel", "id", o.ID, "instId", o.InstID)
			return nil
		}
		return err
	}
	logger.Info("canceled resting manual order", "id", o.ID, "instId", o.InstID)
	m.publishOrderEvent(ctx, "closed", o.ID, o.InstID, logger)
	return nil
}
