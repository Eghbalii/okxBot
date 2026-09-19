package usecase

import (
	"context"
	"errors"
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
