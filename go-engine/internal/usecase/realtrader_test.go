package usecase

import (
	"context"
	"testing"

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
// exactly one real PlaceOrder call and one persisted row carrying the exchange's own order ID.
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
	if open[0].Mode != "real" {
		t.Errorf("expected Mode=real, got %q", open[0].Mode)
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

	// Pre-seed an already-open real position for this instrument/mode directly in the fake.
	repo.orders[999] = port.PaperOrder{ID: 999, InstID: rt.InstID, Mode: "real", Side: "buy", EntryPx: dec("99"), Size: dec("10"), Leverage: dec("1")}
	repo.nextID = 999

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}
	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no new order while a real position is already open, got %d", len(exchange.placedOrders))
	}
}

// TestApplyRealAdjustment_EditsInPlaceNoExchangeCall confirms the no-fork SL/TP edit is purely
// local: computeAdjustedLevels' result is persisted via UpdatePaperOrderSLTP + one
// RecordPaperOrderAdjustment row, with ZERO exchange calls — the corrected 2026-09-03 design (no
// resting algo order to amend).
func TestApplyRealAdjustment_EditsInPlaceNoExchangeCall(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl := dec("95")
	order := port.PaperOrder{ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("10"), Leverage: dec("1")}
	repo.orders[1] = order

	// Proposed SL=98 is a 3% move from the current 95 at price 100, which exceeds RatchetSLTP's
	// single-step cap (MaxSLTPAdjustPct=2%) — the ratchet clamps it to 97 (95 + 2% of 100), same as
	// PaperTrader.applyAdjustment would produce via the identical shared computeAdjustedLevels.
	action := &domain.Action{Action: domain.ActionUpdate, SLPx: dec("98")}
	rt.applyRealAdjustment(context.Background(), order, action, dec("100"), testLogger())

	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected zero exchange calls for an in-place SL/TP edit, got %d", len(exchange.placedOrders))
	}
	updated := repo.orders[1]
	if updated.SLPx == nil || !updated.SLPx.Equal(dec("97")) {
		t.Errorf("expected SL ratchet-clamped to 97, got %v", updated.SLPx)
	}
	adjustments, _ := repo.ListPaperOrderAdjustments(context.Background(), 1)
	if len(adjustments) != 1 {
		t.Fatalf("expected exactly 1 adjustment row, got %d", len(adjustments))
	}
	if adjustments[0].Source != "model" {
		t.Errorf("expected source=model, got %q", adjustments[0].Source)
	}
}

// TestCloseReal_ExchangeFailureDoesNotCloseInDB confirms exchange-first ordering: if PlaceOrder
// (the flattening order) fails, ClosePaperOrder must never be called — a DB failure-to-flatten
// must never leave the system believing a still-open real position is closed.
func TestCloseReal_ExchangeFailureDoesNotCloseInDB(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{placeOrderErr: context.DeadlineExceeded}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	order := port.PaperOrder{ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.orders[1] = order

	err := rt.closeReal(context.Background(), order, dec("110"), "sl", testLogger())
	if err == nil {
		t.Fatal("expected closeReal to return an error when the flattening order fails")
	}
	if repo.orders[1].ClosedAt != nil {
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

	order := port.PaperOrder{ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.orders[1] = order

	if err := rt.closeReal(context.Background(), order, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("closeReal returned error: %v", err)
	}
	if len(exchange.placedOrders) != 1 {
		t.Fatalf("expected exactly 1 flattening order, got %d", len(exchange.placedOrders))
	}
	if exchange.placedOrders[0].Side != "sell" {
		t.Errorf("expected a sell order to flatten a long, got %q", exchange.placedOrders[0].Side)
	}
	if repo.orders[1].ClosedAt == nil {
		t.Error("expected the order to be closed in the DB")
	}
	if model.lastObs.Category != domain.CategoryClosedTP {
		t.Errorf("expected the terminal call to carry CategoryClosedTP, got %q", model.lastObs.Category)
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

	order := port.PaperOrder{ID: 1, InstID: rt.InstID, Mode: "real", Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.orders[1] = order

	rt.reconcile(context.Background(), testLogger())

	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no flattening order when the exchange already reports flat, got %d", len(exchange.placedOrders))
	}
	if repo.orders[1].ClosedAt == nil {
		t.Error("expected the locally-stale open position to be closed after reconciliation")
	}
	if repo.orders[1].CloseReason == nil || *repo.orders[1].CloseReason != conductor.CloseReasonManual {
		t.Errorf("expected close reason %q, got %v", conductor.CloseReasonManual, repo.orders[1].CloseReason)
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
	repo.orders[1] = port.PaperOrder{ID: 1, InstID: rt.InstID, Mode: "real", Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}

	rt.reconcile(context.Background(), testLogger())

	if halted, reason := rt.RiskManager.Halted(); halted {
		t.Errorf("expected no halt on matching state, got reason %q", reason)
	}
	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no orders placed during a no-drift reconciliation, got %d", len(exchange.placedOrders))
	}
	if repo.orders[1].ClosedAt != nil {
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
