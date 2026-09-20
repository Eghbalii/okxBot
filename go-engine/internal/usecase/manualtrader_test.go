package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// assertErr is a stand-in exchange failure for tests that only need PlaceAlgoOrder/etc. to fail,
// not to inspect a specific error value.
var assertErr = errors.New("fake exchange error")

func newTestManualTrader(repo *fakeRepository, exchange *fakeExchangeClient) *ManualTrader {
	return &ManualTrader{
		Repo:     repo,
		Exchange: exchange,
		Logger:   testLogger(),
		TdMode:   "cross",
		PosMode:  "net",
	}
}

// A market order that fills completely opens a manual_orders row, protects it on the exchange, and
// leaves no manual_close_requested/error behind.
func TestManualTrader_MarketOrderOpensAndProtects(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1), TickSz: decimal.NewFromFloat(0.01)},
		ticker:     domain.Ticker{Last: decimal.NewFromInt(100)},
	}
	mt := newTestManualTrader(repo, exchange)

	intentID, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "market",
		SizeUSD: decimal.NewFromInt(100), Leverage: decimal.NewFromInt(10),
		SLPx: ptrDec(decimal.NewFromInt(90)), TPPx: ptrDec(decimal.NewFromInt(120)),
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}

	mt.processPendingIntents(context.Background())

	in, err := repo.GetManualOrderIntent(context.Background(), intentID)
	if err != nil {
		t.Fatalf("get intent: %v", err)
	}
	if in.Status != "done" {
		t.Fatalf("intent status = %q, want done (error=%v)", in.Status, in.Error)
	}
	if in.ManualOrderID == nil {
		t.Fatal("intent has no manual_order_id after a successful open")
	}

	order, err := repo.GetManualOrder(context.Background(), *in.ManualOrderID)
	if err != nil {
		t.Fatalf("get manual order: %v", err)
	}
	if order.Status != "filled" {
		t.Errorf("order status = %q, want filled", order.Status)
	}
	if order.ExchangeAlgoOrderID == nil || *order.ExchangeAlgoOrderID == "" {
		t.Error("filled order has no protective algo order id")
	}
	if order.ProtectedByStrategy {
		t.Error("order should not be marked protected-by-strategy when it placed its own protection")
	}
	if len(exchange.placedAlgoOrders) != 1 {
		t.Fatalf("placed %d algo orders, want 1", len(exchange.placedAlgoOrders))
	}
	algo := exchange.placedAlgoOrders[0]
	if !algo.SLTriggerPx.Equal(decimal.NewFromInt(90)) || !algo.TPTriggerPx.Equal(decimal.NewFromInt(120)) {
		t.Errorf("algo order triggers = sl=%s tp=%s, want sl=90 tp=120", algo.SLTriggerPx, algo.TPTriggerPx)
	}
}

// A per-intent TdMode (the panel's Cross/Isolated pill) reaches the exchange's own order/leverage
// requests, overriding the ManualTrader's static default — OKX already accepts this per-request, so
// each order should carry exactly the margin mode the operator picked for IT, not whatever the
// trader was constructed with (2026-09-19 Trade page fixes).
func TestManualTrader_IntentTdModeOverridesDefault(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1), TickSz: decimal.NewFromFloat(0.01)},
		ticker:     domain.Ticker{Last: decimal.NewFromInt(100)},
	}
	mt := newTestManualTrader(repo, exchange) // constructed with TdMode: "cross"

	_, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "market",
		SizeUSD: decimal.NewFromInt(100), Leverage: decimal.NewFromInt(10),
		SLPx: ptrDec(decimal.NewFromInt(90)), TPPx: ptrDec(decimal.NewFromInt(120)),
		TdMode: "isolated",
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}

	mt.processPendingIntents(context.Background())

	if len(exchange.placedOrders) != 1 || exchange.placedOrders[0].TdMode != "isolated" {
		t.Fatalf("expected exactly one order carrying TdMode=isolated, got %+v", exchange.placedOrders)
	}
	if len(exchange.leverageCalls) != 1 || exchange.leverageCalls[0].MgnMode != "isolated" {
		t.Fatalf("expected the leverage call to carry MgnMode=isolated, got %+v", exchange.leverageCalls)
	}
	if len(exchange.placedAlgoOrders) != 1 || exchange.placedAlgoOrders[0].TdMode != "isolated" {
		t.Fatalf("expected the protective algo order to carry TdMode=isolated too, got %+v", exchange.placedAlgoOrders)
	}
}

// An intent with no TdMode set falls back to the ManualTrader's own default — every caller that
// predates this field (or never sends one) keeps behaving exactly as before.
func TestManualTrader_IntentWithoutTdModeFallsBackToDefault(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1), TickSz: decimal.NewFromFloat(0.01)},
		ticker:     domain.Ticker{Last: decimal.NewFromInt(100)},
	}
	mt := newTestManualTrader(repo, exchange) // constructed with TdMode: "cross"

	_, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "market",
		SizeUSD: decimal.NewFromInt(100), Leverage: decimal.NewFromInt(10),
		SLPx: ptrDec(decimal.NewFromInt(90)), TPPx: ptrDec(decimal.NewFromInt(120)),
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}

	mt.processPendingIntents(context.Background())

	if len(exchange.placedOrders) != 1 || exchange.placedOrders[0].TdMode != "cross" {
		t.Fatalf("expected the order to fall back to TdMode=cross, got %+v", exchange.placedOrders)
	}
}

// posMode() defaults to the struct's own PosMode field until RefreshPosMode has run at least once
// — every existing caller that never wires a refresher (including every other test in this file)
// must keep behaving exactly as before.
func TestManualTrader_PosModeDefaultsToStaticFieldBeforeRefresh(t *testing.T) {
	mt := &ManualTrader{PosMode: "long_short"}
	if got := mt.posMode(); got != "long_short" {
		t.Fatalf("posMode() = %q before any refresh, want long_short", got)
	}
}

// RefreshPosMode re-reads the account's live position mode and posMode() reflects it afterward —
// this is what lets a switch made from the panel (POST /api/manual/account-mode) take effect for
// the next order this process places without a restart.
func TestManualTrader_RefreshPosModeUpdatesLiveValue(t *testing.T) {
	exchange := &fakeExchangeClient{accountConfig: domain.AccountConfig{PosMode: "long_short_mode"}}
	mt := &ManualTrader{Exchange: exchange, PosMode: "net"}

	if got := mt.posMode(); got != "net" {
		t.Fatalf("posMode() before refresh = %q, want net", got)
	}

	mt.RefreshPosMode(context.Background())

	if got := mt.posMode(); got != "long_short" {
		t.Fatalf("posMode() after refresh = %q, want long_short", got)
	}
}

// A failed refresh must not clobber the last known-good value — an unreadable account config is
// "don't know", not "assume net mode", matching this codebase's own established fail-safe-defaults
// discipline (CLAUDE.md §46.5) rather than silently reverting a live hedge-mode account to a value
// that would send malformed orders.
func TestManualTrader_RefreshPosModeErrorKeepsPreviousValue(t *testing.T) {
	exchange := &fakeExchangeClient{accountConfig: domain.AccountConfig{PosMode: "long_short_mode"}}
	mt := &ManualTrader{Exchange: exchange, PosMode: "net"}

	mt.RefreshPosMode(context.Background())
	if got := mt.posMode(); got != "long_short" {
		t.Fatalf("posMode() after successful refresh = %q, want long_short", got)
	}

	exchange.accountConfigErr = assertErr
	mt.RefreshPosMode(context.Background())
	if got := mt.posMode(); got != "long_short" {
		t.Fatalf("posMode() after a FAILED refresh = %q, want the last known-good long_short", got)
	}
}

// A limit order that OKX accepts but does not immediately fill is left RESTING, not treated as a
// failure — docs/MANUAL_TRADE_PLAN.md §8.1's whole reason for the extra status value.
func TestManualTrader_LimitOrderRestsUnfilled(t *testing.T) {
	repo := newFakeRepository()
	live := domain.OrderStatus{State: "live", AccFillSz: decimal.Zero}
	exchange := &fakeExchangeClient{
		instrument:  &domain.Instrument{CtVal: decimal.NewFromInt(1)},
		orderStatus: &live, // every GetOrder call reports "live" — the order never fills during the probe
	}
	mt := newTestManualTrader(repo, exchange)

	intentID, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "limit", LimitPx: ptrDec(decimal.NewFromInt(50)),
		SizeUSD: decimal.NewFromInt(100), Leverage: decimal.NewFromInt(10),
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}

	mt.processPendingIntents(context.Background())

	in, err := repo.GetManualOrderIntent(context.Background(), intentID)
	if err != nil {
		t.Fatalf("get intent: %v", err)
	}
	if in.Status != "done" || in.ManualOrderID == nil {
		errMsg := ""
		if in.Error != nil {
			errMsg = *in.Error
		}
		t.Fatalf("intent status=%s manualOrderID=%v error=%q, want done with a manual_order_id", in.Status, in.ManualOrderID, errMsg)
	}
	order, err := repo.GetManualOrder(context.Background(), *in.ManualOrderID)
	if err != nil {
		t.Fatalf("get manual order: %v", err)
	}
	if order.Status != "resting" {
		t.Errorf("order status = %q, want resting", order.Status)
	}
	if len(exchange.placedAlgoOrders) != 0 {
		t.Error("a resting, unfilled order must not have protection placed yet")
	}
	if order.EntryPx != nil {
		t.Errorf("a resting order must have no entry price yet, got %s", *order.EntryPx)
	}
}

// §8.4: when BotTrader already protects this token, ManualTrader must NOT place a second
// protective order — it records protected_by_strategy instead.
func TestManualTrader_SkipsProtectionWhenStrategyAlreadyProtects(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1)},
		ticker:     domain.Ticker{Last: decimal.NewFromInt(100)},
	}
	mt := newTestManualTrader(repo, exchange)
	mt.BotTraderProtects = func(instID string) bool { return instID == "BTC" }

	_, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "market",
		SizeUSD: decimal.NewFromInt(100), Leverage: decimal.NewFromInt(10),
		SLPx: ptrDec(decimal.NewFromInt(90)),
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}
	mt.processPendingIntents(context.Background())

	orders, err := repo.ListManualOrders(context.Background(), port.PositionFilter{})
	if err != nil {
		t.Fatalf("list manual orders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d manual orders, want 1", len(orders))
	}
	o := orders[0]
	if !o.ProtectedByStrategy {
		t.Error("order should be marked protected_by_strategy")
	}
	if o.ExchangeAlgoOrderID != nil {
		t.Errorf("order should have no algo order id of its own, got %v", *o.ExchangeAlgoOrderID)
	}
	if len(exchange.placedAlgoOrders) != 0 {
		t.Errorf("placed %d algo orders, want 0 (protection should come from the strategy's own order)", len(exchange.placedAlgoOrders))
	}
}

// A position that cannot be protected is closed again immediately rather than left running
// unprotected — the same posture BotTrader.openBot enforces.
func TestManualTrader_ClosesPositionWhenProtectionFails(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		instrument:   &domain.Instrument{CtVal: decimal.NewFromInt(1)},
		ticker:       domain.Ticker{Last: decimal.NewFromInt(100)},
		placeAlgoErr: assertErr,
	}
	mt := newTestManualTrader(repo, exchange)

	_, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "market",
		SizeUSD: decimal.NewFromInt(100), Leverage: decimal.NewFromInt(10),
		SLPx: ptrDec(decimal.NewFromInt(90)),
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}
	mt.processPendingIntents(context.Background())

	orders, err := repo.ListManualOrders(context.Background(), port.PositionFilter{})
	if err != nil {
		t.Fatalf("list manual orders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d manual orders, want 1", len(orders))
	}
	o := orders[0]
	if o.ClosedAt == nil {
		t.Error("an unprotectable position must be closed again immediately, but it is still open")
	}
	// Two PlaceOrder calls: the original entry, and the flatten that closed it again.
	if len(exchange.placedOrders) != 2 {
		t.Errorf("placed %d orders, want 2 (entry + flatten)", len(exchange.placedOrders))
	}
}

// Closing an open manual order flattens it at market and records the exchange's own close figures.
func TestManualTrader_CloseFlattensAndRecordsOutcome(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1)}}
	mt := newTestManualTrader(repo, exchange)

	entryPx := decimal.NewFromInt(100)
	contracts := decimal.NewFromInt(1)
	id, err := repo.OpenManualOrder(context.Background(), port.ManualOrder{
		InstID: "BTC", ExecInstID: "BTC-USDT-SWAP", Side: "buy", OrderType: "market",
		Status: "filled", EntryPx: &entryPx, Size: decimal.NewFromInt(100),
		Leverage: decimal.NewFromInt(10), Contracts: &contracts,
	})
	if err != nil {
		t.Fatalf("open manual order: %v", err)
	}

	if err := mt.closeManual(context.Background(), mustGetManualOrder(t, repo, id), "manual", testLogger()); err != nil {
		t.Fatalf("close manual: %v", err)
	}

	o, err := repo.GetManualOrder(context.Background(), id)
	if err != nil {
		t.Fatalf("get manual order: %v", err)
	}
	if o.ClosedAt == nil {
		t.Fatal("order was not closed")
	}
	if o.CloseReason == nil || *o.CloseReason != "manual" {
		t.Errorf("close reason = %v, want manual", o.CloseReason)
	}
	if len(exchange.placedOrders) != 1 {
		t.Fatalf("placed %d orders, want 1 (the flatten)", len(exchange.placedOrders))
	}
	if exchange.placedOrders[0].Side != "sell" {
		t.Errorf("flatten side = %q, want sell (closing a long)", exchange.placedOrders[0].Side)
	}
}

// Closing an already-closed order is a no-op, not an error — mirrors CloseBotOrderConfirmed's own
// idempotency guard, which exists because two racing close paths must not both succeed.
func TestManualTrader_CloseIsIdempotent(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1)}}
	mt := newTestManualTrader(repo, exchange)

	entryPx := decimal.NewFromInt(100)
	contracts := decimal.NewFromInt(1)
	id, _ := repo.OpenManualOrder(context.Background(), port.ManualOrder{
		InstID: "BTC", ExecInstID: "BTC-USDT-SWAP", Side: "buy", OrderType: "market",
		Status: "filled", EntryPx: &entryPx, Size: decimal.NewFromInt(100),
		Leverage: decimal.NewFromInt(10), Contracts: &contracts,
	})

	o := mustGetManualOrder(t, repo, id)
	if err := mt.closeManual(context.Background(), o, "manual", testLogger()); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Same in-memory view as before the first close (ClosedAt still nil on this local copy) —
	// simulates a second caller racing the first without having seen its result yet.
	if err := mt.closeManual(context.Background(), o, "manual", testLogger()); err != nil {
		t.Fatalf("second close should be a no-op, not an error: %v", err)
	}
}

// ProcessCloseRequests must flatten a FILLED order flagged manual_close_requested — the ordinary
// close-intent sweep path.
func TestManualTrader_ProcessCloseRequestsFlattensFilledOrder(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1)}}
	mt := newTestManualTrader(repo, exchange)

	entryPx := decimal.NewFromInt(100)
	contracts := decimal.NewFromInt(1)
	id, _ := repo.OpenManualOrder(context.Background(), port.ManualOrder{
		InstID: "BTC", ExecInstID: "BTC-USDT-SWAP", Side: "buy", OrderType: "market",
		Status: "filled", EntryPx: &entryPx, Size: decimal.NewFromInt(100), Leverage: decimal.NewFromInt(10),
		Contracts: &contracts,
	})
	if err := repo.RequestManualOrderClose(context.Background(), id); err != nil {
		t.Fatalf("request close: %v", err)
	}

	mt.ProcessCloseRequests(context.Background())

	o := mustGetManualOrder(t, repo, id)
	if o.ClosedAt == nil {
		t.Fatal("flagged order was not closed by the sweep")
	}
	if o.CloseReason == nil || *o.CloseReason != "manual" {
		t.Errorf("close reason = %v, want manual", o.CloseReason)
	}
}

// ProcessCloseRequests must CANCEL a still-RESTING order flagged manual_close_requested, not try
// to flatten it — a resting order never became a position, so there is nothing to flatten
// (docs/MANUAL_TRADE_PLAN.md §8.1's close-vs-cancel split, driven by the row's own status).
func TestManualTrader_ProcessCloseRequestsCancelsRestingOrder(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1)}}
	mt := newTestManualTrader(repo, exchange)

	ordID := "resting-ord-1"
	id, _ := repo.OpenManualOrder(context.Background(), port.ManualOrder{
		InstID: "BTC", ExecInstID: "BTC-USDT-SWAP", Side: "buy", OrderType: "limit",
		Status: "resting", Size: decimal.NewFromInt(100), Leverage: decimal.NewFromInt(10),
		ExchangeOrderID: &ordID,
	})
	if err := repo.RequestManualOrderClose(context.Background(), id); err != nil {
		t.Fatalf("request close: %v", err)
	}

	mt.ProcessCloseRequests(context.Background())

	o := mustGetManualOrder(t, repo, id)
	if o.Status != "canceled" {
		t.Errorf("status = %q, want canceled", o.Status)
	}
	if o.CloseReason == nil || *o.CloseReason != "canceled" {
		t.Errorf("close reason = %v, want canceled", o.CloseReason)
	}
	if len(exchange.placedOrders) != 0 {
		t.Errorf("placed %d orders, want 0 — canceling a resting order must never place a flatten", len(exchange.placedOrders))
	}
	if len(exchange.cancelOrderCalls) != 1 || exchange.cancelOrderCalls[0] != ordID {
		t.Errorf("cancelOrderCalls = %v, want exactly [%q]", exchange.cancelOrderCalls, ordID)
	}
}

func mustGetManualOrder(t *testing.T, repo *fakeRepository, id int64) port.ManualOrder {
	t.Helper()
	o, err := repo.GetManualOrder(context.Background(), id)
	if err != nil {
		t.Fatalf("get manual order %d: %v", id, err)
	}
	return o
}

func ptrDec(d decimal.Decimal) *decimal.Decimal { return &d }

// A manual order requesting more margin than is currently available under the trading cap must be
// declined outright — not silently trimmed — before any exchange call is made (2026-09-20 request:
// the panel's own warning is a soft, bypassable hint; this is the real enforcement).
func TestManualTrader_DeclinesOrderExceedingAvailableMargin(t *testing.T) {
	repo := newFakeRepository()
	repo.accounts["manual"] = port.AccountEquity{
		Mode: "manual", EquityUSD: decimal.NewFromInt(20), AccountBalanceUSD: decimal.NewFromInt(20),
		TradingCapUSD: ptrDec(decimal.NewFromInt(20)),
	}
	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1), TickSz: decimal.NewFromFloat(0.01)},
		ticker:     domain.Ticker{Last: decimal.NewFromInt(100)},
	}
	mt := newTestManualTrader(repo, exchange)

	intentID, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "market",
		SizeUSD: decimal.NewFromInt(25), Leverage: decimal.NewFromInt(10), // 25 > the 20 available
		SLPx: ptrDec(decimal.NewFromInt(90)), TPPx: ptrDec(decimal.NewFromInt(120)),
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}

	mt.processPendingIntents(context.Background())

	in, err := repo.GetManualOrderIntent(context.Background(), intentID)
	if err != nil {
		t.Fatalf("get intent: %v", err)
	}
	if in.Status != "failed" {
		errMsg := ""
		if in.Error != nil {
			errMsg = *in.Error
		}
		t.Fatalf("intent status = %q (error=%q), want failed (a 25-margin order against a 20 cap must be declined)", in.Status, errMsg)
	}
	if in.Error == nil || !strings.Contains(*in.Error, "exceeds available") {
		errMsg := ""
		if in.Error != nil {
			errMsg = *in.Error
		}
		t.Fatalf("error = %q, want it to name the cap-exceeded reason specifically", errMsg)
	}
	if in.ManualOrderID != nil {
		t.Fatal("a declined-for-cap intent must never reach an actual order")
	}
	if len(exchange.placedOrders) != 0 {
		t.Errorf("placed %d orders, want 0 — the cap check must run BEFORE any exchange call", len(exchange.placedOrders))
	}
	if len(exchange.leverageCalls) != 0 {
		t.Errorf("called SetLeverage %d times, want 0 — the cap check must run before leverage is even set", len(exchange.leverageCalls))
	}
}

// A request that fits within available margin (cap minus what's already committed to other open
// manual orders) must proceed normally — the check must not fire on requests that are genuinely
// fine, only on ones that would overcommit the cap.
func TestManualTrader_AllowsOrderWithinAvailableMargin(t *testing.T) {
	repo := newFakeRepository()
	repo.accounts["manual"] = port.AccountEquity{
		Mode: "manual", EquityUSD: decimal.NewFromInt(20), AccountBalanceUSD: decimal.NewFromInt(20),
		TradingCapUSD: ptrDec(decimal.NewFromInt(20)),
	}
	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1), TickSz: decimal.NewFromFloat(0.01)},
		ticker:     domain.Ticker{Last: decimal.NewFromInt(100)},
	}
	mt := newTestManualTrader(repo, exchange)

	intentID, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "market",
		SizeUSD: decimal.NewFromInt(15), Leverage: decimal.NewFromInt(10), // 15 <= the 20 available
		SLPx: ptrDec(decimal.NewFromInt(90)), TPPx: ptrDec(decimal.NewFromInt(120)),
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}

	mt.processPendingIntents(context.Background())

	in, err := repo.GetManualOrderIntent(context.Background(), intentID)
	if err != nil {
		t.Fatalf("get intent: %v", err)
	}
	if in.Status != "done" {
		errMsg := ""
		if in.Error != nil {
			errMsg = *in.Error
		}
		t.Fatalf("intent status = %q (error=%q), want done", in.Status, errMsg)
	}
}

// An already-open manual order's margin counts against the cap when checking a NEW one — the
// available-margin computation must sum every open order's Size, not just check the new request
// against the raw cap in isolation.
func TestManualTrader_AvailableMarginAccountsForAlreadyOpenOrders(t *testing.T) {
	repo := newFakeRepository()
	repo.accounts["manual"] = port.AccountEquity{
		Mode: "manual", EquityUSD: decimal.NewFromInt(20), AccountBalanceUSD: decimal.NewFromInt(20),
		TradingCapUSD: ptrDec(decimal.NewFromInt(20)),
	}
	// An already-open order committing 15 of the 20 cap, leaving only 5 available.
	existingID, err := repo.OpenManualOrder(context.Background(), port.ManualOrder{
		InstID: "ETH", Side: "buy", OrderType: "market", Status: "filled", Size: decimal.NewFromInt(15),
	})
	if err != nil {
		t.Fatalf("seed existing order: %v", err)
	}
	if err := repo.UpdateManualOrderStatus(context.Background(), existingID, "filled", ptrDec(decimal.NewFromInt(100)), ptrDec(decimal.NewFromInt(15)), ptrDec(decimal.NewFromInt(1))); err != nil {
		t.Fatalf("mark existing order filled: %v", err)
	}

	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1), TickSz: decimal.NewFromFloat(0.01)},
		ticker:     domain.Ticker{Last: decimal.NewFromInt(100)},
	}
	mt := newTestManualTrader(repo, exchange)

	// This new order alone (10) fits under the raw 20 cap, but not once the existing 15 is counted:
	// 10 + 15 = 25 > 20. A valid SL/TP is included so the ONLY way this can fail is the cap check —
	// without one, placeManualProtection refuses the order for having no stop/target at all, which
	// would make this test pass even with the cap check disabled (found while mutation-checking:
	// the first version of this test had exactly that vacuous-pass bug).
	intentID, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "market",
		SizeUSD: decimal.NewFromInt(10), Leverage: decimal.NewFromInt(10),
		SLPx: ptrDec(decimal.NewFromInt(90)), TPPx: ptrDec(decimal.NewFromInt(120)),
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}

	mt.processPendingIntents(context.Background())

	in, err := repo.GetManualOrderIntent(context.Background(), intentID)
	if err != nil {
		t.Fatalf("get intent: %v", err)
	}
	if in.Status != "failed" {
		errMsg := ""
		if in.Error != nil {
			errMsg = *in.Error
		}
		t.Fatalf("intent status = %q (error=%q), want failed (10 requested + 15 already open = 25 > the 20 cap)", in.Status, errMsg)
	}
	if in.Error == nil || !strings.Contains(*in.Error, "exceeds available") {
		errMsg := ""
		if in.Error != nil {
			errMsg = *in.Error
		}
		t.Fatalf("error = %q, want it to name the cap-exceeded reason specifically (not some other rejection)", errMsg)
	}
}

// The cross-margin cap guard must tighten a manual position's stop-loss so a touch can never
// realize more than the trading cap — the real scenario reported 2026-09-20: a $20 cap, cross
// margin, and a stop wide enough to draw down far more than the operator intended to risk.
func TestManualTrader_CrossMarginGuardTightensAnOverWideStop(t *testing.T) {
	repo := newFakeRepository()
	repo.accounts["manual"] = port.AccountEquity{
		Mode: "manual", EquityUSD: decimal.NewFromInt(20), AccountBalanceUSD: decimal.NewFromInt(20),
		TradingCapUSD: ptrDec(decimal.NewFromInt(20)),
	}
	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1), TickSz: decimal.NewFromFloat(0.01)},
		ticker:     domain.Ticker{Last: decimal.NewFromInt(100)},
	}
	mt := newTestManualTrader(repo, exchange)
	mt.TdMode = "cross"

	// $10 margin at 10x leverage entering at 100 (long). A naive 50-away stop would realize
	// 10*10*0.50 = $50 — well past the $20 cap. Cap-safe distance = 20/(10*10) = 20% of entry, so
	// the tightened stop must land at 80, not 50.
	intentID, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "market", TdMode: "cross",
		SizeUSD: decimal.NewFromInt(10), Leverage: decimal.NewFromInt(10),
		SLPx: ptrDec(decimal.NewFromInt(50)),
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}

	mt.processPendingIntents(context.Background())

	in, err := repo.GetManualOrderIntent(context.Background(), intentID)
	if err != nil {
		t.Fatalf("get intent: %v", err)
	}
	if in.Status != "done" || in.ManualOrderID == nil {
		errMsg := ""
		if in.Error != nil {
			errMsg = *in.Error
		}
		t.Fatalf("intent status=%s (error=%q), want done", in.Status, errMsg)
	}

	order := mustGetManualOrder(t, repo, *in.ManualOrderID)
	if order.SLPx == nil || !order.SLPx.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("SLPx = %v, want 80 (the cap-safe distance), the requested 50 was never tightened", order.SLPx)
	}
	// The protective order actually sent to the exchange must carry the TIGHTENED level, not the
	// original request — the whole point is that the exchange enforces the safe stop, not the one
	// the operator typed.
	if len(exchange.placedAlgoOrders) != 1 {
		t.Fatalf("placed %d algo orders, want 1", len(exchange.placedAlgoOrders))
	}
	if !exchange.placedAlgoOrders[0].SLTriggerPx.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("exchange-side SL trigger = %s, want 80 (tightened)", exchange.placedAlgoOrders[0].SLTriggerPx)
	}
}

// Under ISOLATED margin, the cross-margin guard must NOT apply — a liquidation there can only ever
// draw down this position's own margin, so there is no whole-account risk to guard against, and the
// operator's own stop (however wide) is left exactly as requested.
func TestManualTrader_CrossMarginGuardDoesNotApplyUnderIsolatedMargin(t *testing.T) {
	repo := newFakeRepository()
	repo.accounts["manual"] = port.AccountEquity{
		Mode: "manual", EquityUSD: decimal.NewFromInt(20), AccountBalanceUSD: decimal.NewFromInt(20),
		TradingCapUSD: ptrDec(decimal.NewFromInt(20)),
	}
	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: decimal.NewFromInt(1), TickSz: decimal.NewFromFloat(0.01)},
		ticker:     domain.Ticker{Last: decimal.NewFromInt(100)},
	}
	mt := newTestManualTrader(repo, exchange)
	mt.TdMode = "isolated"

	intentID, err := repo.CreateManualOrderIntent(context.Background(), port.ManualOrderIntent{
		InstID: "BTC", Side: "buy", OrderType: "market", TdMode: "isolated",
		SizeUSD: decimal.NewFromInt(10), Leverage: decimal.NewFromInt(10),
		SLPx: ptrDec(decimal.NewFromInt(50)), // the same over-wide stop as the cross-margin test above
	})
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}

	mt.processPendingIntents(context.Background())

	in, err := repo.GetManualOrderIntent(context.Background(), intentID)
	if err != nil {
		t.Fatalf("get intent: %v", err)
	}
	if in.Status != "done" || in.ManualOrderID == nil {
		t.Fatalf("intent status = %q, want done", in.Status)
	}
	order := mustGetManualOrder(t, repo, *in.ManualOrderID)
	if order.SLPx == nil || !order.SLPx.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("SLPx = %v, want unchanged 50 (isolated margin has no cross-cap risk to guard against)", order.SLPx)
	}
}
