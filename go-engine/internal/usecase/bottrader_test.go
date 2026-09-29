package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

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

func newTestBotTrader(repo port.Repository, exchange *fakeExchangeClient, model port.ModelClient, strategies []StrategyAssignment) *BotTrader {
	return &BotTrader{
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
		Mode:                "bot",
		AccountInitialUSD:   dec("1000"),
		MaxLeverage:         dec("10"),
		MaxPositionPct:      dec("0.5"),
		MaxTotalExposurePct: dec("0.9"),
		RLClamps: conductor.Clamps{
			MinSLDistPct: dec("0.001"),
			MaxSLDistPct: dec("0.5"),
			MaxLossPct:   dec("0.5"),
		},
		// v8 requires a BTC reference block on every observation (docs/RL_V8_PLAN.md). Wired here
		// rather than per-test because a missing feed makes buildObservation fail outright —
		// correct in production, but it would turn every unrelated test into a BTC-window test.
		BTCCandles: func(string) ([]domain.Candle, bool) { return testCandleWindow(64000), true },
	}
}

func buySignal() strategy.Signal {
	return strategy.Signal{Side: strategy.Buy, Confidence: dec("0.8"), SLPct: dec("0.02"), TPPct: dec("0.04")}
}

func realTraderCandle(price string) domain.Candle {
	return domain.Candle{Open: dec(price), High: dec(price), Low: dec(price), Close: dec(price), Volume: dec("1")}
}

// realTraderWindow is a candle window long enough to build a v8 observation, ENDING at `price`.
//
// A single candle no longer suffices: v8 needs MinCandlesForIndicators candles and exactly
// domain.ReturnsWindow returns, and it refuses rather than padding (docs/RL_V8_PLAN.md). The window
// ends at the requested price so every existing assertion about entry, stop and target prices is
// unaffected — only the history behind that price is new.
func realTraderWindow(price string) []domain.Candle {
	f, _ := dec(price).Float64()
	w := testCandleWindow(f)
	w[len(w)-1] = realTraderCandle(price)
	return w
}

// TestOpenBot_ModelOpenPlacesBotOrderAndPersists confirms a model "open" answer results in
// exactly one real PlaceOrder call and one persisted bot_orders row with status="filled" (the
// fake exchange's default order status has no explicit fill data, so waitForFill's no-ExchangeOrderID
// fallback treats it as immediately filled).
func TestOpenBot_ModelOpenPlacesBotOrderAndPersists(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

	// GetBalance backs buildObservation's AccountEquityUSD (ground truth from the exchange).
	exchange.balances = []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	if len(exchange.placedOrders) != 1 {
		t.Fatalf("expected exactly 1 bot order placed, got %d", len(exchange.placedOrders))
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

// TestOpenBot_ModelSkipPlacesNoOrder confirms a model "skip" answer declines the signal — no
// exchange call, no persisted row.
func TestOpenBot_ModelSkipPlacesNoOrder(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionSkip}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

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

// TestOpenBot_NoModelDeclinesEntirely confirms a nil Model (not yet wired, or misconfigured)
// never places a bot order — real trading must not default to "open" when it has no answer.
func TestOpenBot_NoModelDeclinesEntirely(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, nil, strategies)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

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
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

	// Pre-seed an already-open real position for this instrument directly in the fake.
	repo.realOrders[999] = port.BotOrder{ID: 999, InstID: rt.InstID, Status: "filled", Side: "buy", EntryPx: dec("99"), Size: dec("10"), Leverage: dec("1")}
	repo.nextBotID = 999

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}
	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no new order while a real position is already open, got %d", len(exchange.placedOrders))
	}
}

// TestEvaluateStrategies_PendingOrderDoesNotBlockASecondOpen confirms a still-pending (not yet
// filled) bot order does NOT occupy the one-position-per-token slot — CLAUDE.md real-trading
// readiness plan, 2026-09-04: a pending row is visible on the panel but is not yet a real position.
func TestEvaluateStrategies_PendingOrderDoesNotBlockASecondOpen(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.3"), LeverageFrac: dec("0.2")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

	repo.realOrders[999] = port.BotOrder{ID: 999, InstID: rt.InstID, Status: "pending", Side: "buy", EntryPx: dec("99"), Size: dec("10"), Leverage: dec("1")}
	repo.nextBotID = 999

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}
	if len(exchange.placedOrders) != 1 {
		t.Errorf("expected the pending order to NOT block a new open, got %d orders placed", len(exchange.placedOrders))
	}
}

// TestApplyBotAdjustment_AmendsTheExchangeThenPersists confirms the SL/TP edit reaches the
// EXCHANGE, not just the database (2026-09-09 request). This test previously asserted the exact
// opposite — that an adjustment made ZERO exchange calls — which is the behavior bot order 33
// exposed as unsafe: a stop moved only locally leaves OKX holding the old one.
func TestApplyBotAdjustment_AmendsTheExchangeThenPersists(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl := dec("95")
	algoID := "algo-7"
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl,
		Size: dec("10"), Leverage: dec("1"), ExchangeAlgoOrderID: &algoID,
	}
	repo.realOrders[1] = order

	// Proposed SL=98 is a 3% move from the current 95 at price 100 — no per-step size cap anymore
	// (removed 2026-09-04, explicit operator decision), so it applies in full since it's in the
	// risk-reducing direction for a long (98 > 95).
	action := &domain.Action{Action: domain.ActionUpdate, SLPx: dec("98")}
	rt.applyBotAdjustment(context.Background(), order, action, dec("100"), testLogger())

	if len(exchange.amendedAlgoOrders) != 1 {
		t.Fatalf("expected the adjustment to amend the resting exchange order once, got %d", len(exchange.amendedAlgoOrders))
	}
	amend := exchange.amendedAlgoOrders[0]
	if amend.AlgoID != algoID {
		t.Errorf("amended the wrong algo order: want %q, got %q", algoID, amend.AlgoID)
	}
	if !amend.SLTriggerPx.Equal(dec("98")) {
		t.Errorf("the exchange must receive the NEW stop 98, got %v", amend.SLTriggerPx)
	}
	updated := repo.realOrders[1]
	if updated.SLPx == nil || !updated.SLPx.Equal(dec("98")) {
		t.Errorf("expected SL applied in full at 98 (no size cap), got %v", updated.SLPx)
	}
	adjustments, _ := repo.ListBotOrderAdjustments(context.Background(), 1)
	if len(adjustments) != 1 {
		t.Fatalf("expected exactly 1 adjustment row, got %d", len(adjustments))
	}
	if adjustments[0].Source != "model" {
		t.Errorf("expected source=model, got %q", adjustments[0].Source)
	}
}

// A failed exchange amend must leave the LOCAL levels unchanged too. The two states that matter
// are "both old" and "both new"; the state this guards against is the exchange holding the old
// stop while the database claims the new one, which would hide an unprotected level behind a row
// that looks correct.
func TestApplyBotAdjustment_ExchangeAmendFailureLeavesLevelsUnchanged(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{amendAlgoErr: context.DeadlineExceeded}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl := dec("95")
	algoID := "algo-7"
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl,
		Size: dec("10"), Leverage: dec("1"), ExchangeAlgoOrderID: &algoID,
	}
	repo.realOrders[1] = order

	action := &domain.Action{Action: domain.ActionUpdate, SLPx: dec("98")}
	rt.applyBotAdjustment(context.Background(), order, action, dec("100"), testLogger())

	updated := repo.realOrders[1]
	if updated.SLPx == nil || !updated.SLPx.Equal(dec("95")) {
		t.Errorf("a failed exchange amend must leave the stored stop at 95, got %v", updated.SLPx)
	}
	if adjustments, _ := repo.ListBotOrderAdjustments(context.Background(), 1); len(adjustments) != 0 {
		t.Errorf("a failed amend must record no adjustment row, got %d", len(adjustments))
	}
}

// TestCloseBot_ExchangeFailureDoesNotCloseInDB confirms exchange-first ordering: if PlaceOrder
// (the flattening order) fails, CloseBotOrder must never be called — a DB failure-to-flatten
// must never leave the system believing a still-open real position is closed.
// 2026-09-29: closeBot's exchange-flatten path now uses ClosePosition (no client-supplied size)
// followed by a GetPositions poll for confirmation, replacing the old PlaceOrder+waitForFill.
// A ClosePosition error alone is not fatal (it may just mean "already closed" — see
// domain.ClosePositionRequest's doc), but if the position is STILL reported open afterward, that
// is a genuine stuck close and must not be recorded as done.
func TestCloseBot_ExchangeFailureDoesNotCloseInDB(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		closePositionErr:     context.DeadlineExceeded,
		noAutoFlattenOnClose: true, // ClosePosition fails AND the position stays open on the exchange
		positions:            []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("10")}},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	rt.closeSettlePollTimeoutOverride = 50 * time.Millisecond

	order := port.BotOrder{ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.realOrders[1] = order

	err := rt.closeBot(context.Background(), order, dec("110"), "sl", testLogger())
	if err == nil {
		t.Fatal("expected closeBot to return an error when the position is still open after the poll timeout")
	}
	if repo.realOrders[1].ClosedAt != nil {
		t.Error("expected the order to remain open in the DB when the exchange close failed")
	}
}

// TestCloseBot_SucceedsAndReportsTerminal confirms a successful close places exactly one
// flattening order, closes the DB row, and (with a model configured) delivers the terminal call.
func TestCloseBot_SucceedsAndReportsTerminal(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{}
	rt := newTestBotTrader(repo, exchange, model, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}
	// The terminal call needs a buildable observation like any other: v8 refuses to send a short
	// one, so a trade closed with no candle window trains nothing (docs/RL_V8_PLAN.md).
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("110")}
	exchange.balances = []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}

	order := port.BotOrder{ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.realOrders[1] = order

	if err := rt.closeBot(context.Background(), order, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("closeBot returned error: %v", err)
	}
	// 2026-09-29: the flatten is now a ClosePosition call (no client-supplied side/size — see
	// domain.ClosePositionRequest's doc), not a PlaceOrder.
	if len(exchange.closePositionCalls) != 1 {
		t.Fatalf("expected exactly 1 close-position call, got %d", len(exchange.closePositionCalls))
	}
	if exchange.closePositionCalls[0].InstID != rt.execInstID() {
		t.Errorf("expected close-position for %q, got %q", rt.execInstID(), exchange.closePositionCalls[0].InstID)
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
	rt := newTestBotTrader(repo, exchange, nil, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	sl := dec("95")
	order := port.BotOrder{
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
	rt := newTestBotTrader(repo, exchange, nil, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	order := port.BotOrder{ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
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
	rt := newTestBotTrader(repo, exchange, nil, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	rt.reconcile(context.Background(), testLogger())

	if halted, _ := rt.RiskManager.Halted(); !halted {
		t.Error("expected an untracked exchange position to halt trading")
	}
}

// TestReconcile_UntrackedExchangePositionIsRecordedForThePanel is the fix for a real incident
// (2026-09-28): halting alone stopped new trading but left the untracked position itself
// completely invisible — nothing wrote a row, so it never reached the Positions page where an
// operator could actually see and act on it. This asserts the position now gets its own
// status='untracked' bot_orders row with the exchange's own side/entry/size/leverage, alongside
// (not instead of) the halt.
func TestReconcile_UntrackedExchangePositionIsRecordedForThePanel(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		positions: []domain.Position{{
			InstID: "BTC-USDT-SWAP", Pos: dec("-2"), PosSide: "short",
			AvgPx: dec("50000"), Lever: dec("10"), NotionalUsd: dec("100"),
		}},
		balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	rt.reconcile(context.Background(), testLogger())

	var found *port.BotOrder
	for _, o := range repo.realOrders {
		if o.Status == "untracked" {
			cp := o
			found = &cp
		}
	}
	if found == nil {
		t.Fatalf("expected an untracked bot_orders row to be written, got %d rows: %+v", len(repo.realOrders), repo.realOrders)
	}
	if found.Side != "sell" {
		t.Errorf("Side = %q, want sell (remote reports short/-2)", found.Side)
	}
	if !found.EntryPx.Equal(dec("50000")) {
		t.Errorf("EntryPx = %s, want 50000 (exchange's own avgPx)", found.EntryPx)
	}
	if !found.Leverage.Equal(dec("10")) {
		t.Errorf("Leverage = %s, want 10", found.Leverage)
	}
	// Size must be MARGIN (notional / leverage), never the exchange's own leveraged notional
	// directly — the exact bug a real incident found (2026-09-28): the first version of this fix
	// wrote NotionalUsd straight into Size, reporting ~5x the real committed capital.
	if !found.Size.Equal(dec("10")) {
		t.Errorf("Size = %s, want 10 (notional 100 / leverage 10 = margin, not the raw leveraged notional)", found.Size)
	}
	if found.ClosedAt != nil {
		t.Error("an untracked position must be recorded OPEN, not closed")
	}
}

// TestUntrackedMargin_DividesNotionalByLeverage pins the exact fix for the real PUMP incident
// (2026-09-28): OKX reported NotionalUsd=10.01, Lever=4.88 for a position whose real committed
// margin was ~$2.05 — the first version of this fix stored the $10.01 figure directly, which read
// as roughly 5x the real capital and could never have come from this account's own sizing rule
// (equity / active slots). Uses the exact real-world numbers rather than a round ratio so a
// regression back to "store NotionalUsd as-is" cannot coincidentally pass.
func TestUntrackedMargin_DividesNotionalByLeverage(t *testing.T) {
	remote := &domain.Position{NotionalUsd: dec("10.01"), Lever: dec("4.88")}
	got := untrackedMargin(remote)
	want := dec("10.01").Div(dec("4.88"))
	if !got.Equal(want) {
		t.Errorf("untrackedMargin() = %s, want %s (10.01 / 4.88)", got, want)
	}
	// Sanity: the wrong (pre-fix) answer would have been 10.01 outright — assert we are not that.
	if got.Equal(dec("10.01")) {
		t.Error("untrackedMargin() returned the raw leveraged notional unchanged — leverage was not applied")
	}
}

// TestUntrackedMargin_NonPositiveLeverageFallsBackToNotional guards the divide-by-zero case: OKX
// should never report zero/negative leverage for a genuinely open position, but a panic here would
// be a strictly worse failure than reporting an inflated size for one malformed reading.
func TestUntrackedMargin_NonPositiveLeverageFallsBackToNotional(t *testing.T) {
	remote := &domain.Position{NotionalUsd: dec("50"), Lever: dec("0")}
	got := untrackedMargin(remote)
	if !got.Equal(dec("50")) {
		t.Errorf("untrackedMargin() with zero leverage = %s, want 50 (fallback to notional, no panic)", got)
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
	rt := newTestBotTrader(repo, exchange, nil, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}
	repo.realOrders[1] = port.BotOrder{ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}

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
// plan §6 calls out by name: BotTrader does not own its account balance the way PaperTrader owns
// its shared "paper" row — the exchange is ground truth. This asserts the two sources deliberately
// DISAGREE and the observation still reports the exchange's number, not Repo.GetAccountEquity's —
// the one spot flagged as easy to get wrong by careless reuse of PaperTrader's buildObservation.
func TestBuildObservation_UsesExchangeBalanceNotRepoBookkeeping(t *testing.T) {
	repo := newFakeRepository()
	// Seed a DIFFERENT balance in the repo's own bookkeeping row than what the exchange reports —
	// if buildObservation ever fell back to (or blended with) this, the test would catch it.
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("42")}

	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("777")}}}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	rt.candles = map[string][]domain.Candle{"1m": testCandleWindow(100)}

	obs, err := rt.buildObservation(context.Background(), "1m", dec("100"), testLogger())
	if err != nil {
		t.Fatalf("buildObservation: %v", err)
	}

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
	rt := &BotTrader{SafeMoneyUSD: dec("20")}
	got := rt.tradableEquity(dec("40"))
	if !got.Equal(dec("20")) {
		t.Errorf("expected tradableEquity(40) with SafeMoneyUSD=20 to be 20, got %s", got)
	}
}

// TestTradableEquity_FloorsAtZero confirms a balance below the reserve reports as zero equity, not
// negative — negative would misleadingly read as a drained/liquidated account rather than merely
// under the configured reserve.
func TestTradableEquity_FloorsAtZero(t *testing.T) {
	rt := &BotTrader{SafeMoneyUSD: dec("20")}
	got := rt.tradableEquity(dec("15"))
	if !got.IsZero() {
		t.Errorf("expected tradableEquity(15) with SafeMoneyUSD=20 to floor at 0, got %s", got)
	}
}

// TestTradableEquity_ZeroSafeMoneyIsIdentity confirms the default (SafeMoneyUSD unset) preserves
// today's behavior of using the full reported balance.
func TestTradableEquity_ZeroSafeMoneyIsIdentity(t *testing.T) {
	rt := &BotTrader{}
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
	rt := newTestBotTrader(repo, exchange, nil, nil)
	rt.SafeMoneyUSD = dec("20")
	rt.candles = map[string][]domain.Candle{"1m": testCandleWindow(100)}

	obs, err := rt.buildObservation(context.Background(), "1m", dec("100"), testLogger())
	if err != nil {
		t.Fatalf("buildObservation: %v", err)
	}

	if !obs.AccountEquityUSD.Equal(dec("20")) {
		t.Errorf("expected AccountEquityUSD=20 (40 exchange balance - 20 safe money), got %s", obs.AccountEquityUSD)
	}
}

// TestOpenBot_FillConfirmedImmediatelyRecordsEntryPx confirms the fast path (the expected case
// for a market order against a liquid perpetual, CLAUDE.md §27.5): a "filled" status with an
// AvgPx overwrites the naive candle-close entry price with the exchange's own reported fill price,
// and the persisted row's Status reads "filled".
func TestOpenBot_FillConfirmedImmediatelyRecordsEntryPx(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		balances:    []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
		orderStatus: &domain.OrderStatus{State: "filled", AvgPx: dec("100.05"), AccFillSz: dec("1"), Sz: dec("1")},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.FillTimeout = 50 * time.Millisecond
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

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

// TestOpenBot_NeverFilledCancelsAndOpensNothing confirms the timeout path: an order that never
// fills is canceled, no OPEN position exists (openPositions still returns empty since it filters to
// filled/partial only), but the pending row STAYS in bot_orders with status="canceled" — the
// deliberate answer to "does a timed-out attempt stay visible" (CLAUDE.md real-trading readiness
// plan, 2026-09-04).
func TestOpenBot_NeverFilledCancelsAndOpensNothing(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		balances:    []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
		orderStatus: &domain.OrderStatus{State: "live", AccFillSz: dec("0"), Sz: dec("1")},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.FillTimeout = 50 * time.Millisecond
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

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

// TestOpenBot_PendingRowVisibleBeforeFillResolves confirms the pending row is inserted BEFORE
// waitForFill resolves — CLAUDE.md real-trading readiness plan, 2026-09-04's core requirement
// ("show pending on the panel"). Simulated by checking that OpenBotOrder was called with
// status="pending" as an intermediate state, independent of what the fill eventually resolves to.
func TestOpenBot_PendingRowVisibleBeforeFillResolves(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		balances:    []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
		orderStatus: &domain.OrderStatus{State: "filled", AvgPx: dec("100"), AccFillSz: dec("1"), Sz: dec("1")},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.FillTimeout = 50 * time.Millisecond
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	if repo.nextBotID != 1 {
		t.Fatalf("expected exactly one bot_orders row inserted, got nextBotID=%d", repo.nextBotID)
	}
	// By the time evaluateStrategies returns, the row has already transitioned to "filled" — this
	// test's own value is structural (openBot's insert-then-update-status sequencing exists at
	// all, exercised by every open test in this file), not a distinct runtime assertion beyond what
	// TestOpenBot_ModelOpenPlacesBotOrderAndPersists already checks.
	if repo.realOrders[1].Status != "filled" {
		t.Errorf("expected the row to have transitioned to filled, got %q", repo.realOrders[1].Status)
	}
}

// TestOpenBot_PartialFillRecordsActualSize confirms a partial fill within the timeout window is
// recorded as a real, smaller-than-intended position (proportional to what actually filled), not
// left ambiguous or recorded at the originally requested size (CLAUDE.md §27.5), with
// Status="partial".
func TestOpenBot_PartialFillRecordsActualSize(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
		// Sz here mirrors the requested contract size waitForFill observes on the final (timed-out)
		// poll; AccFillSz is half of it, so the caller should record half the requested USD notional.
		orderStatus: &domain.OrderStatus{State: "live", AvgPx: dec("100"), AccFillSz: dec("2.5"), Sz: dec("5")},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5")}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.FillTimeout = 50 * time.Millisecond
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

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
	// from what the exchange actually filled rather than by scaling the request. Real trading's
	// leverage is now fixed at BotFixedLeverage (2026-09-29, rl-sizing bypassed for BotTrader),
	// not the model's LeverageFrac:
	//
	//   AccFillSz(2.5) x CtVal(1, unset in this fixture) x AvgPx(100) = $250 notional
	//   margin = 250 / BotFixedLeverage(10) = $25
	//
	// Computed independently here rather than read back from open[0].Size, which would make the
	// check tautological.
	lev := dec(fmt.Sprint(BotFixedLeverage))
	wantSize := dec("2.5").Mul(dec("100")).Div(lev)
	if !open[0].Size.Sub(wantSize).Abs().LessThan(dec("0.01")) {
		t.Errorf("expected recorded size ~%s (margin backing the filled 2.5 contracts at 100), got %s", wantSize, open[0].Size)
	}
	if !open[0].EntryPx.Equal(dec("100")) {
		t.Errorf("expected EntryPx=100 (the confirmed AvgPx), got %s", open[0].EntryPx)
	}
}

// TestCloseBot_UnfilledFlattenDoesNotMarkClosed confirms the flatten leg's own fill-timeout path:
// if the closing order doesn't fully fill, the DB row must NOT be marked closed — a partially- or
// un-flattened position is still real exposure on the exchange (CLAUDE.md §27.5).
// 2026-09-29: "unfilled flatten" is now represented by the position remaining reported OPEN on
// GetPositions past the poll timeout (close-position has no order id to poll/cancel via the old
// GetOrder/CancelOrder mechanism — see pollUntilFlat).
func TestCloseBot_UnfilledFlattenDoesNotMarkClosed(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		noAutoFlattenOnClose: true,
		positions:            []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("10")}},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	rt.closeSettlePollTimeoutOverride = 50 * time.Millisecond

	order := port.BotOrder{ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}
	repo.realOrders[1] = order

	err := rt.closeBot(context.Background(), order, dec("110"), "sl", testLogger())
	if err == nil {
		t.Fatal("expected closeBot to return an error when the position never settles flat")
	}
	if repo.realOrders[1].ClosedAt != nil {
		t.Error("expected the order to remain open in the DB when the flatten didn't confirm fill")
	}
}

// TestHandleTick_RunUpdatesThrottled covers the RLAdjustInterval throttle added 2026-09-05 during
// the first real-trading activation: BotTrader.handleTick previously called runUpdates
// unconditionally on EVERY tick with no throttle at all (unlike PaperTrader's identical
// shouldRunRLAdjust gate), which measured live as a continuous, unnecessary Postgres query load
// across 10 real instruments at OKX's live tick rate. A burst of ticks within one interval must
// only trigger one Predict call.
func TestHandleTick_RunUpdatesThrottled(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClientRL{action: domain.Action{}}
	rt := newTestBotTrader(repo, exchange, model, nil)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	sl := dec("95")
	repo.realOrders[1] = port.BotOrder{
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
	rt := newTestBotTrader(repo, exchange, model, nil)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	sl := dec("95")
	repo.realOrders[1] = port.BotOrder{
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
	rt := newTestBotTrader(repo, exchange, model, nil)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	sl := dec("95")
	repo.realOrders[1] = port.BotOrder{
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
	if _, err := repo.GetAccountEquity(ctx, "bot", dec("40")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := repo.SetTradingCap(ctx, "bot", dec("20")); err != nil {
		t.Fatalf("set cap: %v", err)
	}

	// SafeMoneyUSD deliberately zero — the cap alone must produce the reserve.
	e := &BotTrader{Repo: repo}
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
	if _, err := repo.GetAccountEquity(ctx, "bot", dec("40")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	e := &BotTrader{Repo: repo, SafeMoneyUSD: dec("15")}
	if got := e.tradableEquityFor(ctx, dec("40")); !got.Equal(dec("25")) {
		t.Fatalf("no cap set, want safe-money fallback 25, got %s", got)
	}
}

// Regression for the first bot order ever placed (id 3, SOL short, 2026-09-08): it opened with a
// stop and NO take-profit. The model answered with a non-zero SLPx and a ZERO TPPx, and the old
// code treated "the model returned levels" as all-or-nothing — so the model's stop replaced the
// strategy's while the strategy's target was dropped rather than kept, and nothing downstream
// re-supplied one. Since BotTrader watches SL/TP in-process, that position could only ever end at
// its stop, at the 6h timeout, or by hand.
func TestOpenBot_ModelStopWithoutTargetStillGetsATarget(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	// The exact shape that produced order 3: a stop from the model, no target.
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
		SLPx: dec("98"), TPPx: dec("0"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}
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
func TestOpenBot_NoTargetAnywhereStillGetsOneFromStopDistance(t *testing.T) {
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
	rt := newTestBotTrader(repo, exchange, model, strategies)
	// The derivation is a RATIO of the stop distance, so it only applies where one is configured —
	// production sets this (paper_trading.rl_clamps.min_tp_sl_ratio); the shared harness does not.
	rt.RLClamps.MinTPSLRatio = dec("1.5")
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}
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
// no matter how far it moved: UpdateBotOrderPnLExtremes was implemented in internal/postgres and
// declared on the port, but BotTrader never called it — PaperTrader has trackPnLExtremes,
// BotTrader had no counterpart (found 2026-09-08).
func TestMonitorOpenPositions_TracksPnLExtremes(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	// A long at 100 with levels far enough out that no tick below closes it.
	sl, tp := dec("50"), dec("200")
	orderID, err := repo.OpenBotOrder(ctx, port.BotOrder{
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
// This was previously "correct" only by accident — closeBotWith did call ApplyRealizedPnL, but it
// always failed on a foreign key bot order ids cannot satisfy, and the error was swallowed. This
// test makes the behavior intentional so dropping that constraint (migration 000026) cannot
// silently reintroduce the double-count.
func TestCloseBot_DoesNotApplyRealizedPnLToTheAccount(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	rt.AccountInitialUSD = dec("40")

	if _, err := repo.GetAccountEquity(ctx, "bot", dec("40")); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open real order: %v", err)
	}
	o, err := repo.GetBotOrder(ctx, id)
	if err != nil {
		t.Fatalf("get real order: %v", err)
	}

	before, err := repo.GetAccountEquity(ctx, "bot", dec("40"))
	if err != nil {
		t.Fatalf("read account: %v", err)
	}

	if err := rt.closeBot(ctx, o, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("closeBot: %v", err)
	}

	after, err := repo.GetAccountEquity(ctx, "bot", dec("40"))
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
func TestCloseBot_FailedFlattenLeavesPositionOpenAndRecordsError(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		noAutoFlattenOnClose: true,
		positions:            []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("10")}},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	rt.closeSettlePollTimeoutOverride = 50 * time.Millisecond

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetBotOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := rt.closeBot(ctx, o, dec("110"), "tp", testLogger()); err == nil {
		t.Fatal("expected the close to fail when the exchange rejects the flatten")
	}

	after, err := repo.GetBotOrder(ctx, id)
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
// 2026-09-29: the exchange's own close numbers now come from closeFactsFromExchange (reading
// whichever protective algo order leg fired), not from polling the flatten order's own GetOrder
// status — close-position has no order id to poll that way. This test sets up the TP leg as
// having fired and naming a resulting order, which is what closeBotWith now reads after
// ClosePosition + a successful GetPositions poll confirm the position is flat.
func TestCloseBot_PrefersExchangeReportedNumbers(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatus: &domain.AlgoOrderStatus{State: "effective", ActualSide: "tp", OrdID: "flatten-ord-1"},
		orderStatus: &domain.OrderStatus{
			State: "filled", AvgPx: dec("103.9"), AccFillSz: dec("1"), Sz: dec("1"),
			Pnl: dec("0.25"), Fee: dec("-0.02"),
		},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	realOrder := repo.realOrders[id]
	realOrder.ExchangeAlgoOrderID = algoOrderID("SL-1")
	realOrder.ExchangeTPAlgoOrderID = algoOrderID("TP-1")
	repo.realOrders[id] = realOrder
	o, err := repo.GetBotOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Triggered by a tick at 110, but the exchange actually filled at 103.9.
	if err := rt.closeBot(ctx, o, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("closeBot: %v", err)
	}

	after, err := repo.GetBotOrder(ctx, id)
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
	rt := newTestBotTrader(repo, exchange, nil, nil)
	rt.RLEarlyClose = false

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetBotOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	rt.closeEarly(ctx, o, dec("105"), testLogger())

	after, err := repo.GetBotOrder(ctx, id)
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
	rt := newTestBotTrader(repo, exchange, nil, nil)
	rt.RLEarlyClose = true

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetBotOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	rt.closeEarly(ctx, o, dec("105"), testLogger())

	after, err := repo.GetBotOrder(ctx, id)
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
func TestOpenBot_StaleWrongSideStopIsReplacedNotRefused(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}
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
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}
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
// SetBotOrderClosing wrote while the flatten was pending. Real order 5 (SOL) showed the bug: the
// flatten filled on OKX, closed_at was set, no error was recorded, and the exchange reported flat —
// but status stayed 'closing' forever, so the panel displayed a completed close as stuck.
func TestCloseBot_ConfirmedCloseClearsTheClosingStatus(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetBotOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := rt.closeBot(ctx, o, dec("110"), "manual", testLogger()); err != nil {
		t.Fatalf("closeBot: %v", err)
	}

	after, err := repo.GetBotOrder(ctx, id)
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
// otherwise: SetBotOrderClosing overwrites status with 'closing' on the way in, so by the time the
// exchange confirms there is nothing left to restore. Recording "this opened partially filled"
// durably needs its own column; status on a closed row is display-only.
func TestCloseBot_ConfirmedCloseSettlesEvenAPartialFill(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "partial", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetBotOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := rt.closeBot(ctx, o, dec("110"), "manual", testLogger()); err != nil {
		t.Fatalf("closeBot: %v", err)
	}

	after, err := repo.GetBotOrder(ctx, id)
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
// 2026-09-29: close-position (the new exchange-flatten mechanism, replacing PlaceOrder) returns no
// order id of its own — domain.ClosePositionResult carries only instId/posSide, by design, since
// it never places an ordinary order this system could look up via GetOrderRaw. The close leg's raw
// record is now captured only when the EXCHANGE's own protective algo order is what actually fired
// (its resulting order does have an id) — this test exercises exactly that case, rather than a
// close-position-driven close, which legitimately has no raw record to capture any more.
func TestCloseBot_CapturesExchangeRecordsForBothLegs(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatus: &domain.AlgoOrderStatus{State: "effective", ActualSide: "sl", OrdID: "flatten-ord-1"},
	}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}
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

	if err := rt.closeBot(ctx, open[0], dec("110"), "sl", testLogger()); err != nil {
		t.Fatalf("closeBot: %v", err)
	}
	after, err := repo.GetBotOrder(ctx, open[0].ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if len(after.ExchangeCloseRaw) == 0 {
		t.Fatal("the close leg's exchange record must be captured once the fired algo leg's own order is read")
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
func TestCloseBot_ExchangeRecordFailureDoesNotAffectTheClose(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{getOrderRawErr: fmt.Errorf("okx: rate limited")}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp := dec("90"), dec("110")
	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	o, err := repo.GetBotOrder(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if err := rt.closeBot(ctx, o, dec("110"), "manual", testLogger()); err != nil {
		t.Fatalf("a failed record capture must not fail the close: %v", err)
	}
	after, err := repo.GetBotOrder(ctx, id)
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
// 2026-09-29: TestCloseBot_ClosesTheContractsActuallyOpened and
// TestCloseBot_LegacyRowFallsBackToEntryPriceSizing (both here previously) guarded against an
// under-close from a locally re-derived flatten size — a bug class that closeBotWith's switch to
// close-position (domain.ClosePositionRequest) has made structurally impossible: close-position
// carries no client-supplied size at all, so there is no derivation left to get wrong. Removed
// rather than reworked onto a mechanism the bug no longer applies to.

// A position the exchange no longer reports (its own resting stop/take-profit already closed it)
// must be reflected locally on the very next reconciliation poll, not wait for a tick (2026-09-09
// request: closing must be verified against the exchange, not only driven by the local tick feed).
//
// 2026-09-29 (explicit operator instruction, see ReconcileWith's own comment on the remote != nil
// branch): reconcile no longer compares a still-open remote position's mark price against its own
// SL/TP and closes it itself — the exchange's own resting protective order is the only mechanism
// that closes a position for reaching its stop or target. What reconcile still must do is catch
// the position going away on the exchange's side (remote == nil) and close the local row to match,
// which is exercised here.
func TestReconcile_ClosesALocallyOpenPositionTheExchangeNoLongerReports(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	entry := dec("0.00000362")
	sl := dec("0.0000035659802313184517469")
	tp := dec("0.000003820187702322006")
	contracts := dec("5")
	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: entry, SLPx: &sl, TPPx: &tp,
		Size: dec("1.83"), Leverage: dec("9.8719062805175779"), Contracts: &contracts,
		// Opened well outside staleCloseGrace so reconcile doesn't defer this as a just-filled
		// position it hasn't seen confirmed yet (see reconcile's own comment on that branch).
		Status: "filled", OpenedAt: time.Now().Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// The exchange reports flat: its own stop already fired and closed the position.
	exchange.positions = nil

	rt.reconcile(ctx, testLogger())

	after, err := repo.GetBotOrder(ctx, id)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.ClosedAt == nil {
		t.Fatal("a position the exchange no longer reports must be closed locally by the " +
			"reconciliation poll — the tick-driven check does not run while the process is down, " +
			"which is exactly when this matters")
	}
}

// A position comfortably inside its levels must be left alone — this check exists to catch a missed
// stop, not to second-guess a healthy position on every poll.
func TestReconcile_LeavesAHealthyPositionOpen(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp := dec("90"), dec("110")
	contracts := dec("1")
	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID: rt.InstID, Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp,
		Size: dec("10"), Leverage: dec("1"), Contracts: &contracts,
		Status: "filled", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	exchange.positions = []domain.Position{{
		InstID: rt.execInstID(), PosSide: "long", Pos: dec("1"),
		AvgPx: dec("100"), MarkPx: dec("101"), // between the levels
	}}

	rt.reconcile(ctx, testLogger())

	after, err := repo.GetBotOrder(ctx, id)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.ClosedAt != nil {
		t.Fatal("a position inside its own levels must not be closed by reconciliation")
	}
}

// TestNetRealizedPnL_PrefersTheExchangeAndSubtractsItsFee covers the defect found on 2026-09-10:
// realized_pnl was ALWAYS computed locally even when OKX's own figures had just been fetched two
// lines above and stored in adjacent columns.
//
// The two sides measure different things, which is what made the disagreement look like a puzzle:
// OKX's pnl is GROSS (price move only, fee reported separately and negative), while the local
// realizedPnL subtracts its own ESTIMATED fee from its own gross. Neither column held the net
// number the panel shows and the model trains on.
func TestNetRealizedPnL_PrefersTheExchangeAndSubtractsItsFee(t *testing.T) {
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}

	gross := dec("0.0221")
	fee := dec("-0.008667") // OKX reports the fee as a negative charge
	got := netRealizedPnL(o, dec("101"), &gross, &fee)

	// Real order 42's own numbers: 0.0221 gross - 0.008667 fee = 0.013433 net.
	if want := dec("0.013433"); !got.Equal(want) {
		t.Errorf("expected the exchange's gross minus its fee (%s), got %s", want, got)
	}
}

// TestNetRealizedPnL_AddsTheFeeRatherThanSubtractingIt guards the sign. OKX reports the fee as a
// negative number, so it must be ADDED; subtracting would credit the fee and overstate every single
// trade by twice its cost — a bug that would look plausible on every row and be wrong on all of them.
func TestNetRealizedPnL_AddsTheFeeRatherThanSubtractingIt(t *testing.T) {
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}

	gross := dec("1.0")
	fee := dec("-0.25")
	got := netRealizedPnL(o, dec("110"), &gross, &fee)

	if !got.Equal(dec("0.75")) {
		t.Errorf("a negative fee must reduce the result to 0.75, got %s (subtracting it would give 1.25)", got)
	}
	if got.GreaterThan(gross) {
		t.Errorf("net pnl (%s) must never exceed gross (%s) when a fee was charged", got, gross)
	}
}

// TestNetRealizedPnL_FallsBackToLocalWhenTheExchangeReportedNothing: a close OKX did not report on
// (a skipExchange close whose algo order could not be read) still needs a number, and the local
// estimate is much closer to the truth than no fee at all.
func TestNetRealizedPnL_FallsBackToLocalWhenTheExchangeReportedNothing(t *testing.T) {
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}

	got := netRealizedPnL(o, dec("110"), nil, nil)
	want := realizedPnL(o, dec("110"))
	if !got.Equal(want) {
		t.Errorf("with no exchange figures the local calculation must be used: want %s, got %s", want, got)
	}
	if got.IsZero() {
		t.Error("the fallback must produce a real number, not zero")
	}
}

// TestNetRealizedPnL_ExchangeGrossWithNoFeeReported keeps "OKX did not tell us the fee" distinct
// from "the fee was zero": the gross figure is still better than a local estimate because it is
// derived from the true fill price.
func TestNetRealizedPnL_ExchangeGrossWithNoFeeReported(t *testing.T) {
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1")}

	gross := dec("0.486")
	got := netRealizedPnL(o, dec("104"), &gross, nil)
	if !got.Equal(gross) {
		t.Errorf("expected the exchange's gross to be used unchanged when no fee was reported, got %s", got)
	}
}

// TestCloseBotWith_StoresTheExchangeNetAsRealizedPnL is the end-to-end guard: the stored
// realized_pnl must be the exchange-derived net, not the local estimate. Real order 37 is the case
// that made this matter — stored as a 0.0159 LOSS when the exchange's own figures make it a 0.0071
// loss, off by roughly a full round-trip fee.
func TestCloseBotWith_StoresTheExchangeNetAsRealizedPnL(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}

	o := port.BotOrder{
		ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy",
		EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1"),
	}
	repo.realOrders[1] = o

	gross := dec("0.0017")
	fee := dec("-0.008776")
	facts := &exchangeCloseFacts{PnL: &gross, Fee: &fee}
	if err := rt.closeBotWith(context.Background(), o, dec("101"), "sl", true, facts, testLogger()); err != nil {
		t.Fatalf("closeBotWith: %v", err)
	}

	stored := repo.realOrders[1].RealizedPnL
	if stored == nil {
		t.Fatal("expected realized pnl to be stored")
	}
	if want := dec("-0.007076"); !stored.Equal(want) {
		t.Errorf("expected the exchange-derived net %s to be stored, got %s", want, stored)
	}
}

// algoOrderID is the helper the protection tests need — a BotOrder only counts as protected when
// it actually carries the exchange's algo id.
func algoOrderID(id string) *string { return &id }

// decPtr keeps the protection-test fixtures readable; the surrounding tests use a local variable
// per level, which does not scale to fixtures that set several.
func decPtr(v string) *decimal.Decimal {
	d := dec(v)
	return &d
}

// TestMonitorOpenPositions_SkipsTheFlattenWhenTheExchangeAlreadyClosedIt covers the race behind
// every genuine exchange error on this deployment (2026-09-11): the exchange's own stop fires, and
// a moment later this process's tick monitor sees the same touch and asks OKX to close a position
// that no longer exists — rejected with sCode=51169. The position was always correctly closed; the
// duplicate flatten just recorded an alarming last_error against a trade that went exactly right.
func TestMonitorOpenPositions_SkipsTheFlattenWhenTheExchangeAlreadyClosedIt(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		// "effective" is OKX reporting the conditional order has triggered.
		algoStatus: &domain.AlgoOrderStatus{State: "effective", ActualSide: "sl", OrdID: "OKX-CLOSE-1"},
		orderStatus: &domain.OrderStatus{
			State: "filled", AvgPx: dec("95"), AccFillSz: dec("1"), Sz: dec("1"),
			Pnl: dec("-5"), Fee: dec("-0.02"),
		},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}
	repo.realOrders[1] = port.BotOrder{
		ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy",
		EntryPx: dec("100"), SLPx: decPtr("96"), Size: dec("10"), Leverage: dec("1"),
		ExchangeAlgoOrderID: algoOrderID("ALGO-1"),
	}

	// A price below the stop: the local touch check fires.
	if err := rt.monitorOpenPositions(context.Background(), dec("95"), testLogger()); err != nil {
		t.Fatalf("monitorOpenPositions: %v", err)
	}

	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected NO flatten order when the exchange already closed the position, got %d", len(exchange.placedOrders))
	}
	if repo.realOrders[1].ClosedAt == nil {
		t.Fatal("the position must still be recorded as closed — the exchange closed it, we only record it")
	}
	if got := repo.realOrders[1].CloseReason; got == nil || *got != "sl" {
		t.Errorf("expected close reason sl, got %v", got)
	}
	// The exchange's own numbers, not a locally derived guess.
	if got := repo.realOrders[1].ExchangeRealizedPnL; got == nil || !got.Equal(dec("-5")) {
		t.Errorf("expected the exchange's realized pnl to be recorded, got %v", got)
	}
}

// TestMonitorOpenPositions_StillFlattensWhenTheProtectiveOrderIsStillResting is the other half:
// a resting ("live") order has NOT fired, so this system's close is the one that must act.
func TestMonitorOpenPositions_StillFlattensWhenTheProtectiveOrderIsStillResting(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatus: &domain.AlgoOrderStatus{State: "live"},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}
	repo.realOrders[1] = port.BotOrder{
		ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy",
		EntryPx: dec("100"), SLPx: decPtr("96"), Size: dec("10"), Leverage: dec("1"),
		ExchangeAlgoOrderID: algoOrderID("ALGO-1"),
	}

	if err := rt.monitorOpenPositions(context.Background(), dec("95"), testLogger()); err != nil {
		t.Fatalf("monitorOpenPositions: %v", err)
	}

	// 2026-09-29: the flatten is now a ClosePosition call, not a PlaceOrder.
	if len(exchange.closePositionCalls) == 0 {
		t.Error("a still-resting protective order has not fired, so this system must send the flatten")
	}
}

// TestMonitorOpenPositions_FlattensWhenTheProtectiveOrderCannotBeRead is the safety property. An
// unreadable status is "don't know", never "already handled" — declining to flatten on a failed
// API call would leave real exposure open on nothing more than a network blip.
// 2026-09-29: when the protective order's own state can't be read, the fallback is no longer a
// blind flatten — a fresh GetPositions check decides. This is the "genuinely still open" half of
// that fix: the exchange confirms the position is still live, so flattening is correct.
func TestMonitorOpenPositions_FlattensWhenTheProtectiveOrderCannotBeReadButPositionStillOpen(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		getAlgoErr: errors.New("gateway timeout"),
		positions:  []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("10")}},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}
	repo.realOrders[1] = port.BotOrder{
		ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy",
		EntryPx: dec("100"), SLPx: decPtr("96"), Size: dec("10"), Leverage: dec("1"),
		ExchangeAlgoOrderID: algoOrderID("ALGO-1"),
	}

	if err := rt.monitorOpenPositions(context.Background(), dec("95"), testLogger()); err != nil {
		t.Fatalf("monitorOpenPositions: %v", err)
	}

	// 2026-09-29: the flatten is now a ClosePosition call, not a PlaceOrder.
	if len(exchange.closePositionCalls) == 0 {
		t.Error("an unreadable protective order, with the exchange confirming the position is still open, must fall back to a normal flatten")
	}
}

// TestMonitorOpenPositions_ManualCloseIsNotDeferredToTheExchange: a manual close is this system
// deciding to exit, and no resting SL/TP order is going to have done that for us — so the guard
// must not intercept it even when a protective order happens to read as effective.
func TestMonitorOpenPositions_ManualCloseIsNotDeferredToTheExchange(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatus: &domain.AlgoOrderStatus{State: "effective", ActualSide: "sl", OrdID: "X"},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)
	repo.accounts["bot"] = port.AccountEquity{Mode: "bot", InitialUSD: dec("1000"), EquityUSD: dec("1000")}
	repo.realOrders[1] = port.BotOrder{
		ID: 1, InstID: rt.InstID, Status: "filled", Side: "buy",
		EntryPx: dec("100"), SLPx: decPtr("90"), Size: dec("10"), Leverage: dec("1"),
		ExchangeAlgoOrderID:  algoOrderID("ALGO-1"),
		ManualCloseRequested: true,
	}

	// A price nowhere near the stop, so only the manual request can trigger the close.
	if err := rt.monitorOpenPositions(context.Background(), dec("100"), testLogger()); err != nil {
		t.Fatalf("monitorOpenPositions: %v", err)
	}

	// 2026-09-29: the flatten is now a ClosePosition call, not a PlaceOrder.
	if len(exchange.closePositionCalls) == 0 {
		t.Error("a manual close must always send its own flatten")
	}
}

// The cross-margin cap guard must tighten a real bot position's stop-loss so a touch can never
// realize more than the mode's trading cap — the same scenario reported 2026-09-20 for manual
// trading, applied here to BotTrader's own open path (openBot).
//
// The pre-existing MaxLossPct clamp (newTestBotTrader's own RLClamps) already tightens the model's
// proposed 50 to 95 (5% distance, MaxLossPct=0.5/leverage=10) regardless of margin mode — measured
// directly before writing this test (margin sizes to $500 here — MaxPositionPct=0.5 of the $1000
// raw exchange balance the observation reads, independent of the $1 trading cap below, which only
// bounds SIZING'S OWN equity input, not this clamp), since a value that assumed the model's raw
// proposal survives unclamped into the new guard would test nothing real. The $1 trading cap is
// chosen deliberately far tighter than what MaxLossPct alone would produce, so this test exercises
// the NEW guard specifically: without it, this would read 95, not the cap-safe 99.98.
func TestOpenBot_CrossMarginGuardTightensAnOverWideStop(t *testing.T) {
	repo := newFakeRepository()
	repo.accounts["bot"] = port.AccountEquity{
		Mode: "bot", EquityUSD: dec("1000"), AccountBalanceUSD: dec("1000"),
		TradingCapUSD: func() *decimal.Decimal { d := dec("1"); return &d }(),
	}
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("1.0"), LeverageFrac: dec("1.0"), SLPx: dec("50"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.TdMode = "cross"
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

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
	if open[0].SLPx == nil {
		t.Fatal("expected a non-nil stop-loss")
	}
	// Margin=$100 (equity/BotFixedLeverage split, 2026-09-29 fixed sizing — SizePct is no longer
	// consulted), Leverage=10 (BotFixedLeverage, fixed rather than model-chosen): cap-safe distance
	// = 1/(100*10) = 0.1% of entry, i.e. 99.9 for a long — strictly tighter than the pre-existing
	// clamp's own 95.
	if !open[0].SLPx.Equal(dec("99.9")) {
		t.Fatalf("SLPx = %s, want 99.9 (the $1-cap-safe distance, tighter than the pre-existing clamp's own 95)", open[0].SLPx)
	}
}

// Under isolated margin, BotTrader's cross-margin guard must not apply — mirrors
// TestManualTrader_CrossMarginGuardDoesNotApplyUnderIsolatedMargin for the real-order open path.
// Uses the SAME tiny $1 cap as the cross-margin test above: if the guard incorrectly applied under
// isolated margin too, this would also read 99.98, not the pre-existing clamp's own 95.
func TestOpenBot_CrossMarginGuardDoesNotApplyUnderIsolatedMargin(t *testing.T) {
	repo := newFakeRepository()
	repo.accounts["bot"] = port.AccountEquity{
		Mode: "bot", EquityUSD: dec("1000"), AccountBalanceUSD: dec("1000"),
		TradingCapUSD: func() *decimal.Decimal { d := dec("1"); return &d }(),
	}
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("1.0"), LeverageFrac: dec("1.0"), SLPx: dec("50"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
	rt.TdMode = "isolated"
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

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
	if open[0].SLPx == nil || !open[0].SLPx.Equal(dec("95")) {
		t.Fatalf("SLPx = %v, want unchanged 95 (the pre-existing clamp's own result — isolated margin has no cross-cap risk to guard against on top of it)", open[0].SLPx)
	}
}
