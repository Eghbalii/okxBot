package usecase

import (
	"context"
	"fmt"
	"encoding/json"
	"testing"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/risk"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

func newTestRiskManager() *risk.Manager {
	limits := risk.Limits{
		MaxLeverage:             dec("20"),
		MaxPositionNotionalUSD:  dec("100"),
		MaxDailyDrawdownPct:     dec("50"),
		MinLiquidationBufferPct: dec("1"),
	}
	return risk.NewManager(limits, dec("1000"))
}

func newTestRealTrader(repo port.Repository, exchange *fakeExchangeClient, model port.ModelClient, strategies []StrategyAssignment) *RealTrader {
	return &RealTrader{
		InstID:              "BTC-USDT-SWAP",
		Bars:                []string{"1m"},
		CandleWindow:        100,
		Strategies:          strategies,
		Repo:                repo,
		Exchange:            exchange,
		Model:               model,
		RiskManager:         newTestRiskManager(),
		TdMode:              "cross",
		PosMode:             "net",
		Mode:                "real",
		AccountInitialUSD:   dec("1000"),
		MaxLeverage:         dec("10"),
		MaxPositionPct:      dec("0.5"),
		MaxTotalExposurePct: dec("0.9"),
		RLClamps: conductor.Clamps{
			MinSLDistPct: dec("0.001"),
			MaxSLDistPct: dec("0.5"),
			MaxLossPct:   dec("0.5"),
		},
	}
}

func buySignal() strategy.Signal {
	return strategy.Signal{Side: strategy.Buy, Confidence: dec("0.8"), SLPct: dec("0.02"), TPPct: dec("0.04")}
}

func realTraderCandle(price string) domain.Candle {
	return domain.Candle{Open: dec(price), High: dec(price), Low: dec(price), Close: dec(price), Volume: dec("1")}
}

// TestOpenReal_ModelOpenPlacesRealOrderAndPersists confirms a model "open" answer results in
// exactly one real PlaceOrder call and one persisted real_orders row with status="filled" (the
// fake exchange's default order status has no explicit fill data, so waitForFill's no-ExchangeOrderID
// fallback treats it as immediately filled).
func TestOpenReal_ModelOpenPlacesRealOrderAndPersists(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	// GetBalance backs buildObservation's AccountEquityUSD (ground truth from the exchange).
	exchange.balances = []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	if len(exchange.placedOrders) != 1 {
		t.Fatalf("expected exactly 1 real order placed, got %d", len(exchange.placedOrders))
	}
	if exchange.placedOrders[0].Side != "buy" {
		t.Errorf("expected a buy order, got %q", exchange.placedOrders[0].Side)
	}

	open, err := rt.openPositions(context.Background())
	if err != nil {
		t.Fatalf("openPositions: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected 1 persisted open real position, got %d", len(open))
	}
	if open[0].Status != "filled" {
		t.Errorf("expected Status=filled, got %q", open[0].Status)
	}
	if open[0].SLPx == nil {
		t.Error("expected a non-nil stop-loss on the persisted order")
	}
}

// TestOpenReal_ModelSkipPlacesNoOrder confirms a model "skip" answer declines the signal — no
// exchange call, no persisted row.
func TestOpenReal_ModelSkipPlacesNoOrder(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionSkip}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no orders placed on a model skip, got %d", len(exchange.placedOrders))
	}
	open, _ := rt.openPositions(context.Background())
	if len(open) != 0 {
		t.Errorf("expected no persisted rows on a model skip, got %d", len(open))
	}
}

// TestOpenReal_NoModelDeclinesEntirely confirms a nil Model (not yet wired, or misconfigured)
// never places a real order — real trading must not default to "open" when it has no answer.
func TestOpenReal_NoModelDeclinesEntirely(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, nil, strategies)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}
	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no orders placed with no model configured, got %d", len(exchange.placedOrders))
	}
}

// TestEvaluateStrategies_OnePositionPerTokenBlocksASecondOpen confirms an already-open real
// position on this token blocks a second strategy from opening another one (net PosMode) — the
// opposite-side-signal-is-never-a-flip rule (CLAUDE.md §27.3).
func TestEvaluateStrategies_OnePositionPerTokenBlocksASecondOpen(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.3"), LeverageFrac: dec("0.2")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	// Pre-seed an already-open real position for this instrument directly in the fake.
	repo.realOrders[999] = port.RealOrder{ID: 999, InstID: rt.InstID, Status: "filled", Side: "buy", EntryPx: dec("99"), Size: dec("10"), Leverage: dec("1")}
	repo.nextRealID = 999

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}
	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no new order while a real position is already open, got %d", len(exchange.placedOrders))
	}
}

// TestEvaluateStrategies_PendingOrderDoesNotBlockASecondOpen confirms a still-pending (not yet
// filled) real order does NOT occupy the one-position-per-token slot — CLAUDE.md real-trading
// readiness plan, 2026-09-04: a pending row is visible on the panel but is not yet a real position.
func TestEvaluateStrategies_PendingOrderDoesNotBlockASecondOpen(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.3"), LeverageFrac: dec("0.2")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	repo.realOrders[999] = port.RealOrder{ID: 999, InstID: rt.InstID, Status: "pending", Side: "buy", EntryPx: dec("99"), Size: dec("10"), Leverage: dec("1")}
	repo.nextRealID = 999

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}
	if len(exchange.placedOrders) != 1 {
		t.Errorf("expected the pending order to NOT block a new open, got %d orders placed", len(exchange.placedOrders))
	}
}

// TestApplyRealAdjustment_EditsInPlaceNoExchangeCall confirms the no-fork SL/TP edit is purely
// local: computeAdjustedLevels' result is persisted via UpdateRealOrderSLTP + one
// RecordRealOrderAdjustment row, with ZERO exchange calls — the corrected 2026-09-03 design (no
// resting algo order to amend).
func TestApplyRealAdjustment_EditsInPlaceNoExchangeCall(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl := dec("95")
	order := port.RealOrder{ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("10"), Leverage: dec("1")}
	repo.realOrders[1] = order

	// Proposed SL=98 is a 3% move from the current 95 at price 100 — no per-step size cap anymore
	// (removed 2026-09-04, explicit operator decision), so it applies in full since it's in the
	// risk-reducing direction for a long (98 > 95), same as PaperTrader.applyAdjustment would
	// produce via the identical shared computeAdjustedLevels.
	action := &domain.Action{Action: domain.ActionUpdate, SLPx: dec("98")}
	rt.applyRealAdjustment(context.Background(), order, action, dec("100"), testLogger())

	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected zero exchange calls for an in-place SL/TP edit, got %d", len(exchange.placedOrders))
	}
	updated := repo.realOrders[1]
	if updated.SLPx == nil || !updated.SLPx.Equal(dec("98")) {
		t.Errorf("expected SL applied in full at 98 (no size cap), got %v", updated.SLPx)
	}
	adjustments, _ := repo.ListRealOrderAdjustments(context.Background(), 1)
	if len(adjustments) != 1 {
		t.Fatalf("expected exactly 1 adjustment row, got %d", len(adjustments))
	}
	if adjustments[0].Source != "model" {
		t.Errorf("expected source=model, got %q", adjustments[0].Source)
	}
}

// TestCloseReal_ExchangeFailureDoesNotCloseInDB confirms exchange-first ordering: if PlaceOrder
// (the flattening order) fails, CloseRealOrder must never be called — a DB failure-to-flatten
// must never leave the system believing a still-open real position is closed.
func TestCloseReal_ExchangeFailureDoesNotCloseInDB(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{placeOrderErr: context.DeadlineExceeded}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	order := port.RealOrder{ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.realOrders[1] = order

	err := rt.closeReal(context.Background(), order, dec("110"), "sl", testLogger())
	if err == nil {
		t.Fatal("expected closeReal to return an error when the flattening order fails")
	}
	if repo.realOrders[1].ClosedAt != nil {
		t.Error("expected the order to remain open in the DB when the exchange close failed")
	}
}

// TestCloseReal_SucceedsAndReportsTerminal confirms a successful close places exactly one
// flattening order, closes the DB row, and (with a model configured) delivers the terminal call.
func TestCloseReal_SucceedsAndReportsTerminal(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{}
	rt := newTestRealTrader(repo, exchange, model, nil)
	repo.accounts["real"] = port.AccountEquity{Mode: "real", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	order := port.RealOrder{ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.realOrders[1] = order

	if err := rt.closeReal(context.Background(), order, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("closeReal returned error: %v", err)
	}
	if len(exchange.placedOrders) != 1 {
		t.Fatalf("expected exactly 1 flattening order, got %d", len(exchange.placedOrders))
	}
	if exchange.placedOrders[0].Side != "sell" {
		t.Errorf("expected a sell order to flatten a long, got %q", exchange.placedOrders[0].Side)
	}
	if repo.realOrders[1].ClosedAt == nil {
		t.Error("expected the order to be closed in the DB")
	}
	if model.lastObs.Category != domain.CategoryClosedTP {
		t.Errorf("expected the terminal call to carry CategoryClosedTP, got %q", model.lastObs.Category)
	}
}

// TestMonitorOpenPositions_ManualCloseTakesPriorityOverSLTPTouch confirms a manual close request
// wins over a coincidental SL/TP touch on the same tick (CLAUDE.md real-trading readiness plan,
// 2026-09-04 — the Close button's real wiring, mirroring PaperTrader's own priority order).
func TestMonitorOpenPositions_ManualCloseTakesPriorityOverSLTPTouch(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)
	repo.accounts["real"] = port.AccountEquity{Mode: "real", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	sl := dec("95")
	order := port.RealOrder{
		ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy", EntryPx: dec("100"), SLPx: &sl,
		Size: dec("10"), Leverage: dec("1"), ManualCloseRequested: true,
	}
	repo.realOrders[1] = order

	// Price of 90 would normally trigger an SL touch — manual close must win instead.
	if err := rt.monitorOpenPositions(context.Background(), dec("90"), testLogger()); err != nil {
		t.Fatalf("monitorOpenPositions returned error: %v", err)
	}

	closed := repo.realOrders[1]
	if closed.ClosedAt == nil {
		t.Fatal("expected the order to be closed")
	}
	if closed.CloseReason == nil || *closed.CloseReason != conductor.CloseReasonManual {
		t.Errorf("expected close reason %q, got %v", conductor.CloseReasonManual, closed.CloseReason)
	}
}

// TestReconcile_ExchangeReportsFlatClosesLocallyOpenPosition confirms the reconciliation poll
// routes a drift ("exchange shows flat, we think it's open") through the normal close path with
// the exchange leg skipped — no PlaceOrder call, since there is nothing left to flatten.
func TestReconcile_ExchangeReportsFlatClosesLocallyOpenPosition(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		positions: nil, // exchange reports no open position
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	rt := newTestRealTrader(repo, exchange, nil, nil)
	repo.accounts["real"] = port.AccountEquity{Mode: "real", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	order := port.RealOrder{ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.realOrders[1] = order

	rt.reconcile(context.Background(), testLogger())

	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no flattening order when the exchange already reports flat, got %d", len(exchange.placedOrders))
	}
	if repo.realOrders[1].ClosedAt == nil {
		t.Error("expected the locally-stale open position to be closed after reconciliation")
	}
	if repo.realOrders[1].CloseReason == nil || *repo.realOrders[1].CloseReason != conductor.CloseReasonManual {
		t.Errorf("expected close reason %q, got %v", conductor.CloseReasonManual, repo.realOrders[1].CloseReason)
	}
}

// TestReconcile_UntrackedExchangePositionHalts confirms an exchange-reported position this system
// has no local record of trips the risk manager's halt rather than being silently ignored.
func TestReconcile_UntrackedExchangePositionHalts(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		positions: []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("1"), PosSide: "long"}},
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	rt := newTestRealTrader(repo, exchange, nil, nil)
	repo.accounts["real"] = port.AccountEquity{Mode: "real", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	rt.reconcile(context.Background(), testLogger())

	if halted, _ := rt.RiskManager.Halted(); !halted {
		t.Error("expected an untracked exchange position to halt trading")
	}
}

// TestReconcile_MatchingStateIsANoOp confirms local state agreeing with the exchange produces no
// close, no halt, and no spurious order.
func TestReconcile_MatchingStateIsANoOp(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		positions: []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("1"), PosSide: "long"}},
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	rt := newTestRealTrader(repo, exchange, nil, nil)
	repo.accounts["real"] = port.AccountEquity{Mode: "real", InitialUSD: dec("1000"), EquityUSD: dec("1000")}
	repo.realOrders[1] = port.RealOrder{ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}

	rt.reconcile(context.Background(), testLogger())

	if halted, reason := rt.RiskManager.Halted(); halted {
		t.Errorf("expected no halt on matching state, got reason %q", reason)
	}
	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no orders placed during a no-drift reconciliation, got %d", len(exchange.placedOrders))
	}
	if repo.realOrders[1].ClosedAt != nil {
		t.Error("expected the matching position to remain open")
	}
}

// TestBuildObservation_UsesExchangeBalanceNotRepoBookkeeping is the dedicated test CLAUDE.md §27's
// plan §6 calls out by name: RealTrader does not own its account balance the way PaperTrader owns
// its shared "paper" row — the exchange is ground truth. This asserts the two sources deliberately
// DISAGREE and the observation still reports the exchange's number, not Repo.GetAccountEquity's —
// the one spot flagged as easy to get wrong by careless reuse of PaperTrader's buildObservation.
func TestBuildObservation_UsesExchangeBalanceNotRepoBookkeeping(t *testing.T) {
	repo := newFakeRepository()
	// Seed a DIFFERENT balance in the repo's own bookkeeping row than what the exchange reports —
	// if buildObservation ever fell back to (or blended with) this, the test would catch it.
	repo.accounts["real"] = port.AccountEquity{Mode: "real", InitialUSD: dec("1000"), EquityUSD: dec("42")}

	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("777")}}}
	rt := newTestRealTrader(repo, exchange, nil, nil)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	obs := rt.buildObservation(context.Background(), "1m", dec("100"), testLogger())

	if !obs.AccountEquityUSD.Equal(dec("777")) {
		t.Errorf("expected AccountEquityUSD=777 (from Exchange.GetBalance), got %s — "+
			"a value of 42 would mean it fell back to Repo.GetAccountEquity's bookkeeping row instead",
			obs.AccountEquityUSD)
	}
}

// TestTradableEquity_SubtractsSafeMoney confirms the reserve is subtracted from the raw exchange
// balance (real-trading readiness plan, 2026-09-04 operator decision: capital that must stay
// untouched even if every open position were liquidated, since isolated margin never draws on it).
func TestTradableEquity_SubtractsSafeMoney(t *testing.T) {
	rt := &RealTrader{SafeMoneyUSD: dec("20")}
	got := rt.tradableEquity(dec("40"))
	if !got.Equal(dec("20")) {
		t.Errorf("expected tradableEquity(40) with SafeMoneyUSD=20 to be 20, got %s", got)
	}
}

// TestTradableEquity_FloorsAtZero confirms a balance below the reserve reports as zero equity, not
// negative — negative would misleadingly read as a drained/liquidated account rather than merely
// under the configured reserve.
func TestTradableEquity_FloorsAtZero(t *testing.T) {
	rt := &RealTrader{SafeMoneyUSD: dec("20")}
	got := rt.tradableEquity(dec("15"))
	if !got.IsZero() {
		t.Errorf("expected tradableEquity(15) with SafeMoneyUSD=20 to floor at 0, got %s", got)
	}
}

// TestTradableEquity_ZeroSafeMoneyIsIdentity confirms the default (SafeMoneyUSD unset) preserves
// today's behavior of using the full reported balance.
func TestTradableEquity_ZeroSafeMoneyIsIdentity(t *testing.T) {
	rt := &RealTrader{}
	got := rt.tradableEquity(dec("777"))
	if !got.Equal(dec("777")) {
		t.Errorf("expected tradableEquity(777) with no SafeMoneyUSD to be unchanged, got %s", got)
	}
}

// TestBuildObservation_SubtractsSafeMoneyFromExchangeBalance confirms buildObservation applies the
// reserve before the model ever sees AccountEquityUSD, so sizing can never draw against it.
func TestBuildObservation_SubtractsSafeMoneyFromExchangeBalance(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("40")}}}
	rt := newTestRealTrader(repo, exchange, nil, nil)
	rt.SafeMoneyUSD = dec("20")
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	obs := rt.buildObservation(context.Background(), "1m", dec("100"), testLogger())

	if !obs.AccountEquityUSD.Equal(dec("20")) {
		t.Errorf("expected AccountEquityUSD=20 (40 exchange balance - 20 safe money), got %s", obs.AccountEquityUSD)
	}
}

// TestOpenReal_FillConfirmedImmediatelyRecordsEntryPx confirms the fast path (the expected case
// for a market order against a liquid perpetual, CLAUDE.md §27.5): a "filled" status with an
// AvgPx overwrites the naive candle-close entry price with the exchange's own reported fill price,
// and the persisted row's Status reads "filled".
func TestOpenReal_FillConfirmedImmediatelyRecordsEntryPx(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		balances:    []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
		orderStatus: &domain.OrderStatus{State: "filled", AvgPx: dec("100.05"), AccFillSz: dec("1"), Sz: dec("1")},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.FillTimeout = 50 * time.Millisecond
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := rt.openPositions(context.Background())
	if len(open) != 1 {
		t.Fatalf("expected 1 persisted open real position, got %d", len(open))
	}
	if !open[0].EntryPx.Equal(dec("100.05")) {
		t.Errorf("expected EntryPx=100.05 (the confirmed fill price), got %s", open[0].EntryPx)
	}
	if open[0].Status != "filled" {
		t.Errorf("expected Status=filled, got %q", open[0].Status)
	}
	if len(exchange.cancelOrderCalls) != 0 {
		t.Errorf("expected no cancel calls on an immediate fill, got %d", len(exchange.cancelOrderCalls))
	}
}

// TestOpenReal_NeverFilledCancelsAndOpensNothing confirms the timeout path: an order that never
// fills is canceled, no OPEN position exists (openPositions still returns empty since it filters to
// filled/partial only), but the pending row STAYS in real_orders with status="canceled" — the
// deliberate answer to "does a timed-out attempt stay visible" (CLAUDE.md real-trading readiness
// plan, 2026-09-04).
func TestOpenReal_NeverFilledCancelsAndOpensNothing(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		balances:    []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
		orderStatus: &domain.OrderStatus{State: "live", AccFillSz: dec("0"), Sz: dec("1")},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.FillTimeout = 50 * time.Millisecond
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	if len(exchange.cancelOrderCalls) != 1 {
		t.Fatalf("expected exactly 1 cancel call, got %d", len(exchange.cancelOrderCalls))
	}
	open, _ := rt.openPositions(context.Background())
	if len(open) != 0 {
		t.Errorf("expected no OPEN position for a never-filled order, got %d", len(open))
	}
	if len(repo.realOrders) != 1 {
		t.Fatalf("expected the pending row to still exist (canceled, not deleted), got %d rows", len(repo.realOrders))
	}
	for _, o := range repo.realOrders {
		if o.Status != "canceled" {
			t.Errorf("expected Status=canceled on the surviving row, got %q", o.Status)
		}
	}
}

// TestOpenReal_PendingRowVisibleBeforeFillResolves confirms the pending row is inserted BEFORE
// waitForFill resolves — CLAUDE.md real-trading readiness plan, 2026-09-04's core requirement
// ("show pending on the panel"). Simulated by checking that OpenRealOrder was called with
// status="pending" as an intermediate state, independent of what the fill eventually resolves to.
func TestOpenReal_PendingRowVisibleBeforeFillResolves(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		balances:    []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
		orderStatus: &domain.OrderStatus{State: "filled", AvgPx: dec("100"), AccFillSz: dec("1"), Sz: dec("1")},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.FillTimeout = 50 * time.Millisecond
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	if repo.nextRealID != 1 {
		t.Fatalf("expected exactly one real_orders row inserted, got nextRealID=%d", repo.nextRealID)
	}
	// By the time evaluateStrategies returns, the row has already transitioned to "filled" — this
	// test's own value is structural (openReal's insert-then-update-status sequencing exists at
	// all, exercised by every open test in this file), not a distinct runtime assertion beyond what
	// TestOpenReal_ModelOpenPlacesRealOrderAndPersists already checks.
	if repo.realOrders[1].Status != "filled" {
		t.Errorf("expected the row to have transitioned to filled, got %q", repo.realOrders[1].Status)
	}
}

// TestOpenReal_PartialFillRecordsActualSize confirms a partial fill within the timeout window is
// recorded as a real, smaller-than-intended position (proportional to what actually filled), not
// left ambiguous or recorded at the originally requested size (CLAUDE.md §27.5), with
// Status="partial".
func TestOpenReal_PartialFillRecordsActualSize(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
		// Sz here mirrors the requested contract size waitForFill observes on the final (timed-out)
		// poll; AccFillSz is half of it, so the caller should record half the requested USD notional.
		orderStatus: &domain.OrderStatus{State: "live", AvgPx: dec("100"), AccFillSz: dec("2.5"), Sz: dec("5")},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.FillTimeout = 50 * time.Millisecond
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := rt.openPositions(context.Background())
	if len(open) != 1 {
		t.Fatalf("expected 1 persisted position (the partially-filled portion), got %d", len(open))
	}
	if open[0].Status != "partial" {
		t.Errorf("expected Status=partial, got %q", open[0].Status)
	}
	// Size is MARGIN throughout this codebase, not notional — realizedPnLWithFunding derives the
	// notional as Size x Leverage, so the two cannot both be Size. Since 2026-09-09 it is derived
	// from what the exchange actually filled rather than by scaling the request:
	//
	//   AccFillSz(2.5) x CtVal(1, unset in this fixture) x AvgPx(100) = $250 notional
	//   margin = 250 / leverage(5.5, from LeverageFrac 0.5 against MaxLeverage 10) = $45.45
	//
	// Computed independently here rather than read back from open[0].Size, which would make the
	// check tautological.
	lev := dec("1").Add(dec("0.5").Mul(dec("10").Sub(dec("1")))) // 5.5
	wantSize := dec("2.5").Mul(dec("100")).Div(lev)
	if !open[0].Size.Sub(wantSize).Abs().LessThan(dec("0.01")) {
		t.Errorf("expected recorded size ~%s (margin backing the filled 2.5 contracts at 100), got %s", wantSize, open[0].Size)
	}
	if !open[0].EntryPx.Equal(dec("100")) {
		t.Errorf("expected EntryPx=100 (the confirmed AvgPx), got %s", open[0].EntryPx)
	}
}

// TestCloseReal_UnfilledFlattenDoesNotMarkClosed confirms the flatten leg's own fill-timeout path:
// if the closing order doesn't fully fill, the DB row must NOT be marked closed — a partially- or
// un-flattened position is still real exposure on the exchange (CLAUDE.md §27.5).
func TestCloseReal_UnfilledFlattenDoesNotMarkClosed(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		orderStatus: &domain.OrderStatus{State: "live", AccFillSz: dec("0"), Sz: dec("1")},
	}
	rt := newTestRealTrader(repo, exchange, nil, nil)
	rt.FillTimeout = 50 * time.Millisecond

	order := port.RealOrder{ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.realOrders[1] = order

	err := rt.closeReal(context.Background(), order, dec("110"), "sl", testLogger())
	if err == nil {
		t.Fatal("expected closeReal to return an error when the flatten order doesn't fill")
	}
	if repo.realOrders[1].ClosedAt != nil {
		t.Error("expected the order to remain open in the DB when the flatten didn't confirm fill")
	}
	if len(exchange.cancelOrderCalls) != 1 {
		t.Errorf("expected the unfilled flatten order to be canceled, got %d cancel calls", len(exchange.cancelOrderCalls))
	}
}

// TestHandleTick_RunUpdatesThrottled covers the RLAdjustInterval throttle added 2026-09-05 during
// the first real-trading activation: RealTrader.handleTick previously called runUpdates
// unconditionally on EVERY tick with no throttle at all (unlike PaperTrader's identical
// shouldRunRLAdjust gate), which measured live as a continuous, unnecessary Postgres query load
// across 10 real instruments at OKX's live tick rate. A burst of ticks within one interval must
// only trigger one Predict call.
func TestHandleTick_RunUpdatesThrottled(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClientRL{action: domain.Action{}}
	rt := newTestRealTrader(repo, exchange, model, nil)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}
	repo.accounts["real"] = port.AccountEquity{Mode: "real", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	sl := dec("95")
	repo.realOrders[1] = port.RealOrder{
		ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy", EntryPx: dec("100"), SLPx: &sl,
		Size: dec("10"), Leverage: dec("1"),
	}

	for i := 0; i < 5; i++ {
		tick, _ := json.Marshal(tickEvent{InstID: rt.InstID, Last: "103.5"})
		if err := rt.handleTick(context.Background(), tick, testLogger()); err != nil {
			t.Fatalf("handleTick #%d: %v", i, err)
		}
	}

	if model.calls != 1 {
		t.Errorf("expected exactly 1 Predict call across a burst of ticks within RLAdjustInterval, got %d", model.calls)
	}
}

// TestRunUpdates_SkipsManualOverrideEntirely covers the 2026-09-06 request: once an operator has
// edited a position's SL/TP by hand (handleAdjustPosition, which sets ManualOverride), the model
// must never be consulted about that order again — not to move the levels a second time, and not
// to close it early (rl_early_close). Asserted at the strongest level available: zero Predict
// calls, not merely "the levels didn't change" (which a model returning ActionNone would also
// produce, without proving the lockout actually happened).
//
// Calls runUpdates directly rather than through handleTick, and seeds the conductor's per-order
// PnL baseline first via a throwaway ShouldUpdate call. Two earlier versions of this test looked
// right and passed but proved nothing, each defeated by a different gate upstream of the guard
// under test: ShouldUpdate returns false unconditionally on an order's first-ever sighting (it
// only seeds the baseline then), and handleTick's own RLAdjustInterval throttle blocks a second
// call issued immediately after the first. Both were only caught by mutation-testing — deleting
// the ManualOverride guard and confirming the test then failed — which is why every fix in this
// codebase gets that check before being trusted.
func TestRunUpdates_SkipsManualOverrideEntirely(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClientRL{action: domain.Action{Action: domain.ActionClose}}
	rt := newTestRealTrader(repo, exchange, model, nil)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}
	repo.accounts["real"] = port.AccountEquity{Mode: "real", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	sl := dec("95")
	repo.realOrders[1] = port.RealOrder{
		ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy", EntryPx: dec("100"), SLPx: &sl,
		Size: dec("10"), Leverage: dec("1"), ManualOverride: true, OpenedAt: time.Now(),
	}

	ctx := context.Background()
	// Seed the conductor's baseline for order 1 directly, bypassing ShouldUpdate's own
	// always-false-on-first-sighting behavior so the guard under test is what actually gets
	// exercised on the runUpdates call below.
	rt.conductor().ShouldUpdate(1, dec("0"), time.Now().Add(-time.Hour))

	rt.runUpdates(ctx, "1m", dec("110"), testLogger()) // a real 10% PnL move from entry 100

	if model.calls != 0 {
		t.Errorf("expected zero Predict calls for a manually-overridden order, got %d", model.calls)
	}
	if repo.realOrders[1].ClosedAt != nil {
		t.Error("expected the manually-overridden order to remain open — the model's ActionClose must never reach it")
	}
}

// TestHandleTick_RunUpdatesFiresOnFirstTick confirms the throttle doesn't suppress the FIRST tick
// — an open position must still get an update pass promptly, not only after some initial delay.
func TestHandleTick_RunUpdatesFiresOnFirstTick(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClientRL{action: domain.Action{}}
	rt := newTestRealTrader(repo, exchange, model, nil)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}
	repo.accounts["real"] = port.AccountEquity{Mode: "real", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	sl := dec("95")
	repo.realOrders[1] = port.RealOrder{
		ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy", EntryPx: dec("100"), SLPx: &sl,
		Size: dec("10"), Leverage: dec("1"),
	}

	tick, _ := json.Marshal(tickEvent{InstID: rt.InstID, Last: "103.5"})
	if err := rt.handleTick(context.Background(), tick, testLogger()); err != nil {
		t.Fatalf("handleTick: %v", err)
	}

	if model.calls != 1 {
		t.Errorf("expected Predict called once on the very first tick, got %d calls", model.calls)
	}
}

// Sizing must draw against the operator's stored trading cap, not the full exchange balance and
// not the config-level SafeMoneyUSD reserve (2026-09-08). This is what lets safe_money_usd be
// retired: without it, zeroing that config value would silently hand the model the whole balance.
func TestTradableEquityFor_PrefersStoredCapOverSafeMoney(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "real", dec("40")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := repo.SetTradingCap(ctx, "real", dec("20")); err != nil {
		t.Fatalf("set cap: %v", err)
	}

	// SafeMoneyUSD deliberately zero — the cap alone must produce the reserve.
	e := &RealTrader{Repo: repo}
	if got := e.tradableEquityFor(ctx, dec("40")); !got.Equal(dec("20")) {
		t.Fatalf("tradable against a $20 cap on a $40 balance: want 20, got %s", got)
	}

	// The balance grew by 5 (realized profit). The reserve stays at 20, so tradable becomes 25 —
	// the requested behavior. Pinning at the flat cap would report 20 and discard the gain.
	if got := e.tradableEquityFor(ctx, dec("45")); !got.Equal(dec("25")) {
		t.Fatalf("tradable after +5 profit: want 25, got %s", got)
	}
}

// With no cap stored, behavior falls back to SafeMoneyUSD exactly as before — accounts that never
// set a cap are unaffected by the change above.
func TestTradableEquityFor_FallsBackToSafeMoneyWithoutCap(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	if _, err := repo.GetAccountEquity(ctx, "real", dec("40")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	e := &RealTrader{Repo: repo, SafeMoneyUSD: dec("15")}
	if got := e.tradableEquityFor(ctx, dec("40")); !got.Equal(dec("25")) {
		t.Fatalf("no cap set, want safe-money fallback 25, got %s", got)
	}
}

// Regression for the first real order ever placed (id 3, SOL short, 2026-09-08): it opened with a
// stop and NO take-profit. The model answered with a non-zero SLPx and a ZERO TPPx, and the old
// code treated "the model returned levels" as all-or-nothing — so the model's stop replaced the
// strategy's while the strategy's target was dropped rather than kept, and nothing downstream
// re-supplied one. Since RealTrader watches SL/TP in-process, that position could only ever end at
// its stop, at the 6h timeout, or by hand.
func TestOpenReal_ModelStopWithoutTargetStillGetsATarget(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	// The exact shape that produced order 3: a stop from the model, no target.
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
		SLPx: dec("98"), TPPx: dec("0"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}
	exchange.balances = []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, err := rt.openPositions(context.Background())
	if err != nil {
		t.Fatalf("openPositions: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected 1 persisted open real position, got %d", len(open))
	}
	if open[0].TPPx == nil {
		t.Fatal("a real position must never open without a take-profit: it could then only exit at its stop, at the timeout, or by hand")
	}
	if !open[0].TPPx.GreaterThan(open[0].EntryPx) {
		t.Fatalf("a long's target must sit above entry, got tp=%s entry=%s", open[0].TPPx, open[0].EntryPx)
	}
	// The model's stop must still win over the strategy's — the per-side fallback fills the gap,
	// it does not discard the answer the model actually gave.
	if open[0].SLPx == nil || !open[0].SLPx.Equal(dec("98")) {
		t.Fatalf("expected the model's own stop (98) to be used, got %v", open[0].SLPx)
	}
}

// The second layer of the same fix: when NEITHER the model nor the strategy supplies a target,
// EnsureTarget derives one from the stop distance. The per-side fallback above cannot help here —
// there is nothing to fall back TO — so without this a position would still open with no target.
func TestOpenReal_NoTargetAnywhereStillGetsOneFromStopDistance(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
		SLPx: dec("98"), TPPx: dec("0"),
	}}
	// A signal carrying a stop but no target at all (stoch_cross does exactly this in production).
	sig := buySignal()
	sig.TPPct = dec("0")
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: sig}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	// The derivation is a RATIO of the stop distance, so it only applies where one is configured —
	// production sets this (paper_trading.rl_clamps.min_tp_sl_ratio); the shared harness does not.
	rt.RLClamps.MinTPSLRatio = dec("1.5")
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}
	exchange.balances = []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, err := rt.openPositions(context.Background())
	if err != nil {
		t.Fatalf("openPositions: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected 1 persisted open real position, got %d", len(open))
	}
	if open[0].TPPx == nil {
		t.Fatal("with no target from either source, one must still be derived from the stop distance")
	}
	if !open[0].TPPx.GreaterThan(open[0].EntryPx) {
		t.Fatalf("a long's derived target must sit above entry, got tp=%s entry=%s", open[0].TPPx, open[0].EntryPx)
	}
}

// The panel's Max/Min columns read pnl_max_pct/pnl_min_pct, which sat at 0 for every real position
// no matter how far it moved: UpdateRealOrderPnLExtremes was implemented in internal/postgres and
// declared on the port, but RealTrader never called it — PaperTrader has trackPnLExtremes,
// RealTrader had no counterpart (found 2026-09-08).
func TestMonitorOpenPositions_TracksPnLExtremes(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	// A long at 100 with levels far enough out that no tick below closes it.
	sl, tp := dec("50"), dec("200")
	orderID, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open real order: %v", err)
	}

	// Runs to 110 (+10% at 1x), then back down to 95 (-5%).
	for _, px := range []string{"110", "95"} {
		if err := rt.monitorOpenPositions(ctx, dec(px), testLogger()); err != nil {
			t.Fatalf("monitorOpenPositions at %s: %v", px, err)
		}
	}

	open, err := rt.openPositions(ctx)
	if err != nil {
		t.Fatalf("openPositions: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected the position to still be open, got %d", len(open))
	}
	got := open[0]
	if got.ID != orderID {
		t.Fatalf("unexpected order id %d", got.ID)
	}
	// The peak must survive the later move against it — that a trade reached +10% and gave it back
	// is precisely the distinction these columns exist to record.
	if !got.PnLMaxPct.Equal(dec("0.1")) {
		t.Errorf("PnLMaxPct: want 0.1 (the peak, not the latest), got %s", got.PnLMaxPct)
	}
	if !got.PnLMinPct.Equal(dec("-0.05")) {
		t.Errorf("PnLMinPct: want -0.05, got %s", got.PnLMinPct)
	}
}

// Real mode must NOT apply a closed trade's PnL to the stored balance: the exchange's own reported
// balance is ground truth and already reflects it, and the reconciliation poll's
// RecordExchangeBalance is what records that change. Doing both counts the same profit or loss
// twice against a real account.
//
// This was previously "correct" only by accident — closeRealWith did call ApplyRealizedPnL, but it
// always failed on a foreign key real order ids cannot satisfy, and the error was swallowed. This
// test makes the behavior intentional so dropping that constraint (migration 000026) cannot
// silently reintroduce the double-count.
func TestCloseReal_DoesNotApplyRealizedPnLToTheAccount(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)
	rt.AccountInitialUSD = dec("40")

	if _, err := repo.GetAccountEquity(ctx, "real", dec("40")); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open real order: %v", err)
	}
	o, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get real order: %v", err)
	}

	before, err := repo.GetAccountEquity(ctx, "real", dec("40"))
	if err != nil {
		t.Fatalf("read account: %v", err)
	}

	if err := rt.closeReal(ctx, o, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("closeReal: %v", err)
	}

	after, err := repo.GetAccountEquity(ctx, "real", dec("40"))
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	if !after.EquityUSD.Equal(before.EquityUSD) {
		t.Fatalf("closing a real position must not move the stored balance (the exchange balance already "+
			"reflects it; RecordExchangeBalance observes that): was %s, now %s", before.EquityUSD, after.EquityUSD)
	}
	if !after.AccountBalanceUSD.Equal(before.AccountBalanceUSD) {
		t.Fatalf("closing a real position must not move AccountBalanceUSD: was %s, now %s",
			before.AccountBalanceUSD, after.AccountBalanceUSD)
	}
}

// A close must not be recorded until the exchange confirms the flatten filled. Real order 3 was
// written as closed with nothing having verified OKX agreed — if the flatten had failed, the
// database would have said "flat" while a real position stayed open on the exchange.
func TestCloseReal_FailedFlattenLeavesPositionOpenAndRecordsError(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{placeOrderErr: fmt.Errorf("okx: insufficient margin")}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := rt.closeReal(ctx, o, dec("110"), "tp", testLogger()); err == nil {
		t.Fatal("expected the close to fail when the exchange rejects the flatten")
	}

	after, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.ClosedAt != nil {
		t.Fatal("a position whose flatten failed must NOT be recorded closed — it may still be open on the exchange")
	}
	if after.LastError == nil {
		t.Fatal("the failure must be recorded on the order so the panel can raise it to a human")
	}
}

// The exchange's own fill price, realized PnL and fee are what get stored — not the tick price that
// merely triggered the close, and not a locally computed PnL. Order 3 recorded close_px=102 from a
// stale trigger tick while the market was at 103.9.
func TestCloseReal_PrefersExchangeReportedNumbers(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	exchange.orderStatusQueue = []domain.OrderStatus{{
		State: "filled", AvgPx: dec("103.9"), AccFillSz: dec("1"), Sz: dec("1"),
		Pnl: dec("0.25"), Fee: dec("-0.02"),
	}}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Triggered by a tick at 110, but the exchange actually filled at 103.9.
	if err := rt.closeReal(ctx, o, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("closeReal: %v", err)
	}

	after, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.ClosedAt == nil {
		t.Fatal("a confirmed flatten must record the close")
	}
	if after.ExchangeClosePx == nil || !after.ExchangeClosePx.Equal(dec("103.9")) {
		t.Fatalf("ExchangeClosePx: want the exchange's own fill price 103.9, got %v", after.ExchangeClosePx)
	}
	if after.ClosePx == nil || !after.ClosePx.Equal(dec("103.9")) {
		t.Fatalf("ClosePx must be the confirmed fill price, not the trigger tick: got %v", after.ClosePx)
	}
	if after.ExchangeRealizedPnL == nil || !after.ExchangeRealizedPnL.Equal(dec("0.25")) {
		t.Fatalf("ExchangeRealizedPnL: want 0.25 from the exchange, got %v", after.ExchangeRealizedPnL)
	}
	if after.ExchangeFee == nil || !after.ExchangeFee.Equal(dec("-0.02")) {
		t.Fatalf("ExchangeFee: want -0.02 from the exchange, got %v", after.ExchangeFee)
	}
}

// A missing exchange figure must stay distinguishable from a genuine zero, so the panel knows when
// to fall back to the locally computed value.
func TestExchangeCloseNumbers_NilWhenNotReported(t *testing.T) {
	pnl, fee := exchangeCloseNumbers(domain.OrderStatus{State: "filled"})
	if pnl != nil || fee != nil {
		t.Fatalf("unreported figures must be nil, got pnl=%v fee=%v", pnl, fee)
	}
	pnl, fee = exchangeCloseNumbers(domain.OrderStatus{State: "filled", Pnl: dec("1.5"), Fee: dec("-0.1")})
	if pnl == nil || !pnl.Equal(dec("1.5")) || fee == nil || !fee.Equal(dec("-0.1")) {
		t.Fatalf("reported figures must pass through, got pnl=%v fee=%v", pnl, fee)
	}
}

// Real trading must ignore the model's early-close action unless its OWN switch is on — enabling
// early close for paper-trading research must never silently enable it against real capital
// (2026-09-08 request).
func TestCloseEarly_IgnoredWhenDisabled(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)
	rt.RLEarlyClose = false

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	rt.closeEarly(ctx, o, dec("105"), testLogger())

	after, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.ClosedAt != nil {
		t.Fatal("early close must be ignored while disabled — the position runs to its own SL/TP or timeout")
	}
	if len(exchange.placedOrders) != 0 {
		t.Fatalf("an ignored early close must place no exchange order, got %d", len(exchange.placedOrders))
	}
}

// With the switch on, the same request does close the position — the gate is the flag, not a
// permanent refusal.
func TestCloseEarly_ClosesWhenEnabled(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)
	rt.RLEarlyClose = true

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	rt.closeEarly(ctx, o, dec("105"), testLogger())

	after, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.ClosedAt == nil {
		t.Fatal("early close must go through when enabled")
	}
	if after.CloseReason == nil || *after.CloseReason != conductor.CloseReasonRLEarly {
		t.Fatalf("close reason: want %q, got %v", conductor.CloseReasonRLEarly, after.CloseReason)
	}
}

// A strategy's stop is routinely on the wrong side of entry by the time the order actually opens:
// the signal is computed on a closed candle and the live price moves before the open, so a long
// whose price slipped below the signal's stop arrives with a stop ABOVE entry. Apply drops such a
// level, and because EnsureStop only fills a NIL one, running EnsureStop first left the stale stop
// in place to be dropped with nothing to replace it — refusing the open. Observed live rejecting
// every PUMP signal for 20 minutes while the strategy emitted a perfectly good stop each time.
func TestOpenReal_StaleWrongSideStopIsReplacedNotRefused(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}
	exchange.balances = []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}

	// An ABSOLUTE stop, as the structural strategies emit (CLAUDE.md §16.8) — vwap_reversion and
	// friends report a real price level, not a percentage. A percentage could never land on the
	// wrong side, since ResolveLevels derives it from the same open price; only a level fixed
	// BEFORE the price moved can. Here the stop was computed at 100 and the order opens at 95, so
	// the stop now sits ABOVE entry for a long.
	// Production clamp values, not the shared harness's looser ones: MinSLDistPct of 0.005 is what
	// makes Apply reject a stop that has ended up on the wrong side, and the harness's 0.001 is
	// slack enough to hide the whole failure.
	rt.RLClamps.MinSLDistPct = dec("0.005")
	rt.RLClamps.MaxSLDistPct = dec("0.05")
	rt.RLClamps.MaxLossPct = dec("0.15")
	rt.RLClamps.MinTPSLRatio = dec("1.5")

	sig := buySignal()
	// Clear the percentages: buildPaperOrder derives levels from SLPct/TPPct when present, which
	// resolve against the OPEN price and so can never land on the wrong side. Only an absolute
	// level, fixed before the price moved, reproduces the case.
	sig.SLPct = dec("0")
	sig.TPPct = dec("0")
	sig.SLPx = dec("98")
	sig.TPPx = dec("104")
	rt.Strategies = []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: sig}, StrategyID: 1, Kind: "stub"}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("95"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies: %v", err)
	}

	open, err := rt.openPositions(context.Background())
	if err != nil {
		t.Fatalf("openPositions: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("the open must proceed with a replacement stop, not be refused; got %d positions", len(open))
	}
	if open[0].SLPx == nil {
		t.Fatal("a stop must have been filled in after the stale one was dropped")
	}
	if !open[0].SLPx.LessThan(open[0].EntryPx) {
		t.Fatalf("a long's stop must sit below entry, got sl=%s entry=%s", open[0].SLPx, open[0].EntryPx)
	}
}

// A roster change must reach an engine that is ALREADY running. The flag used to be captured once
// at construction, so a token auto-disabled seconds after startup kept opening positions until
// someone restarted the process.
func TestSetOpensDisabled_TakesEffectOnARunningEngine(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}
	exchange.balances = []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}

	rt.SetOpensDisabled(true)
	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies: %v", err)
	}
	if len(exchange.placedOrders) != 0 {
		t.Fatalf("a disabled token must open nothing, got %d orders", len(exchange.placedOrders))
	}

	// And re-enabling on the running engine restores opens, so a recovering account brings its
	// tokens back without a restart either.
	rt.SetOpensDisabled(false)
	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies after re-enable: %v", err)
	}
	if len(exchange.placedOrders) != 1 {
		t.Fatalf("re-enabling must restore opens, got %d orders", len(exchange.placedOrders))
	}
}

// A confirmed close must leave the row in a settled state, not the in-flight 'closing' marker
// SetRealOrderClosing wrote while the flatten was pending. Real order 5 (SOL) showed the bug: the
// flatten filled on OKX, closed_at was set, no error was recorded, and the exchange reported flat —
// but status stayed 'closing' forever, so the panel displayed a completed close as stuck.
func TestCloseReal_ConfirmedCloseClearsTheClosingStatus(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := rt.closeReal(ctx, o, dec("110"), "manual", testLogger()); err != nil {
		t.Fatalf("closeReal: %v", err)
	}

	after, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.ClosedAt == nil {
		t.Fatal("a confirmed flatten must record the close")
	}
	if after.Status == "closing" {
		t.Fatal("a confirmed close must not leave the row reading 'closing' — the panel shows that as stuck")
	}
	if after.Status != "filled" {
		t.Fatalf("status after a confirmed close: want filled, got %q", after.Status)
	}
}

// The open-side fill state does NOT survive a close, and this documents that rather than pretending
// otherwise: SetRealOrderClosing overwrites status with 'closing' on the way in, so by the time the
// exchange confirms there is nothing left to restore. Recording "this opened partially filled"
// durably needs its own column; status on a closed row is display-only.
func TestCloseReal_ConfirmedCloseSettlesEvenAPartialFill(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "partial", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := rt.closeReal(ctx, o, dec("110"), "manual", testLogger()); err != nil {
		t.Fatalf("closeReal: %v", err)
	}

	after, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.Status == "closing" {
		t.Fatal("a confirmed close must never leave the row reading 'closing'")
	}
	if after.Status != "filled" {
		t.Fatalf("status after a confirmed close: want filled, got %q", after.Status)
	}
}

// The exchange's own record for each leg is captured ONCE, when that leg reaches a terminal state
// and its record is final (2026-09-09 request: "don't call it each time — when the system reads
// it, save the JSON there"). Every later view is then a row read rather than a live API call
// against a rate-limit budget shared with real trading.
func TestCloseReal_CapturesExchangeRecordsForBothLegs(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("100")}}
	exchange.balances = []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}

	if err := rt.evaluateStrategies(ctx, "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies: %v", err)
	}
	open, err := rt.openPositions(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("expected 1 open position, got %d (err %v)", len(open), err)
	}
	if len(open[0].ExchangeOpenRaw) == 0 {
		t.Fatal("the open leg's exchange record must be captured once the fill is confirmed")
	}

	if err := rt.closeReal(ctx, open[0], dec("110"), "manual", testLogger()); err != nil {
		t.Fatalf("closeReal: %v", err)
	}
	after, err := repo.GetRealOrder(ctx, open[0].ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if len(after.ExchangeCloseRaw) == 0 {
		t.Fatal("the close leg's exchange record must be captured once the flatten is confirmed")
	}
	// Both legs must be stored, and stored separately — a capture that overwrote the other leg
	// would leave the order with only half its history.
	if len(after.ExchangeOpenRaw) == 0 {
		t.Fatal("capturing the close must not clear the open leg's record")
	}
	if string(after.ExchangeOpenRaw) == string(after.ExchangeCloseRaw) {
		t.Fatal("the two legs are different orders and must not store the same record")
	}
}

// Capturing an audit record is best-effort by design: a failure must degrade the audit trail, never
// the trade it describes.
func TestCloseReal_ExchangeRecordFailureDoesNotAffectTheClose(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{getOrderRawErr: fmt.Errorf("okx: rate limited")}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := rt.closeReal(ctx, o, dec("110"), "manual", testLogger()); err != nil {
		t.Fatalf("a failed record capture must not fail the close: %v", err)
	}
	after, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.ClosedAt == nil {
		t.Fatal("the close must be recorded even when the exchange record could not be captured")
	}
	if len(after.ExchangeCloseRaw) != 0 {
		t.Fatal("a failed capture must store nothing rather than a partial or fabricated record")
	}
}

// The flatten must close exactly the contracts the exchange filled, never a count re-derived from
// a price (2026-09-09 incident). Real order 11's numbers: opened 2 BTC contracts at 78863.3, and
// the close path — sizing the same margin at the CURRENT price of 79258.5 — computed 1. It closed
// 1, left 1 live on OKX behind a row recording a complete close, and the untracked position that
// produced halted real trading for three hours. ETH (5 of 6) and DOGE (20 of 21) failed the same
// way: whenever price moves in a position's favour, the same margin buys fewer contracts.
func TestCloseReal_ClosesTheContractsActuallyOpened(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	inst := domain.Instrument{CtVal: dec("0.0001"), LotSz: dec("1"), MinSz: dec("1")}
	exchange := &fakeExchangeClient{instrument: &inst}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	contracts := dec("2")
	sl, tp := dec("70000"), dec("90000")
	id, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("78863.3"), SLPx: &sl, TPPx: &tp,
		Size: dec("1.6124186773046364"), Leverage: dec("9.7819879055023189"),
		Contracts: &contracts, Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Closing at a price ABOVE entry — the favourable move that triggered the under-close.
	if err := rt.closeReal(ctx, o, dec("79258.5"), "tp", testLogger()); err != nil {
		t.Fatalf("closeReal: %v", err)
	}

	if len(exchange.placedOrders) != 1 {
		t.Fatalf("expected exactly one flattening order, got %d", len(exchange.placedOrders))
	}
	if got := exchange.placedOrders[0].Sz; !got.Equal(dec("2")) {
		t.Fatalf("flatten size: want 2 contracts (what was opened), got %s — a short flatten leaves "+
			"a live remainder on the exchange behind a row that claims to be flat", got)
	}
}

// Rows opened before the contract count was recorded have nothing better to fall back on, but the
// fallback must at least size at the ENTRY price — the price the position was actually opened at —
// rather than the current one.
func TestCloseReal_LegacyRowFallsBackToEntryPriceSizing(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	inst := domain.Instrument{CtVal: dec("0.0001"), LotSz: dec("1"), MinSz: dec("1")}
	exchange := &fakeExchangeClient{instrument: &inst}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, tp := dec("70000"), dec("90000")
	id, err := repo.OpenRealOrder(ctx, port.RealOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("78863.3"), SLPx: &sl, TPPx: &tp,
		Size: dec("1.6124186773046364"), Leverage: dec("9.7819879055023189"),
		Status: "filled", OpenedAt: time.Now(), // no Contracts — a pre-migration row
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetRealOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := rt.closeReal(ctx, o, dec("79258.5"), "tp", testLogger()); err != nil {
		t.Fatalf("closeReal: %v", err)
	}
	if got := exchange.placedOrders[0].Sz; !got.Equal(dec("2")) {
		t.Fatalf("legacy fallback must size at the entry price: want 2, got %s", got)
	}
}
