package usecase

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// openOneRealPosition drives the real open path once and returns the resulting position, so the
// tests below assert against a position produced by the ACTUAL open flow rather than a
// hand-constructed row — the point being that protection is placed by that flow, not bolted on.
func openOneRealPosition(t *testing.T, repo *fakeRepository, exchange *fakeExchangeClient) []port.RealOrder {
	t.Helper()
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}
	exchange.balances = []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies: %v", err)
	}
	open, err := rt.openPositions(context.Background())
	if err != nil {
		t.Fatalf("openPositions: %v", err)
	}
	return open
}

// The whole point of this feature: opening a real position must rest its stop on the EXCHANGE, not
// merely store it in a column this process watches. Real order 33 opened with a stop that existed
// only in the database, which is what this asserts can no longer happen.
func TestOpenReal_RestsStopLossOnTheExchange(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}

	open := openOneRealPosition(t, repo, exchange)
	if len(open) != 1 {
		t.Fatalf("expected 1 open position, got %d", len(open))
	}

	if len(exchange.placedAlgoOrders) != 1 {
		t.Fatalf("opening a real position must rest exactly one protective order on the exchange, got %d",
			len(exchange.placedAlgoOrders))
	}
	algo := exchange.placedAlgoOrders[0]
	if !algo.SLTriggerPx.IsPositive() {
		t.Error("the protective order must carry a stop-loss trigger price")
	}
	// The exchange's stop must be the SAME level the position records. A protective order resting
	// at a different price than the row shows is the divergence this feature exists to prevent.
	if open[0].SLPx == nil || !algo.SLTriggerPx.Equal(*open[0].SLPx) {
		t.Errorf("the exchange's stop %v must match the position's own stop %v", algo.SLTriggerPx, open[0].SLPx)
	}
	// A protective order closes the position, so it is the OPPOSITE side of the entry.
	if algo.Side != "sell" {
		t.Errorf("a protective order for a long must be a sell, got %q", algo.Side)
	}
	// It must be sized to the contracts actually filled — a protective order for the wrong size
	// leaves part of the position unprotected.
	if open[0].Contracts == nil || !algo.Sz.Equal(*open[0].Contracts) {
		t.Errorf("the protective order size %v must match the filled contracts %v", algo.Sz, open[0].Contracts)
	}
	// And its id must be recorded, or no later adjustment could reach it and no close could cancel it.
	if open[0].ExchangeAlgoOrderID == nil || *open[0].ExchangeAlgoOrderID == "" {
		t.Error("the resting order's algoId must be recorded on the position")
	}
}

// A position that cannot be protected is CLOSED again rather than kept (2026-09-09 decision). This
// is the deliberate answer to "what if the SL never makes it onto the exchange" — the alternative,
// holding an unprotected leveraged position and hoping a retry succeeds, is the exposure this
// whole change removes.
func TestOpenReal_ClosesThePositionWhenProtectionCannotBePlaced(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{placeAlgoErr: errors.New("exchange rejected the algo order")}

	open := openOneRealPosition(t, repo, exchange)
	if len(open) != 0 {
		t.Fatalf("an unprotectable position must not be left open, got %d open", len(open))
	}
	// Two orders reached the exchange: the entry, then the flatten that undid it.
	if len(exchange.placedOrders) != 2 {
		t.Fatalf("expected the entry to be flattened immediately (2 orders), got %d", len(exchange.placedOrders))
	}
	if exchange.placedOrders[1].Side != "sell" {
		t.Errorf("the second order must flatten the long, got side %q", exchange.placedOrders[1].Side)
	}
}

// Verification, not blind trust: a position whose protective order has vanished from the exchange
// gets a new one. An algo order can be cancelled from OKX's own UI or lost to a margin-mode change,
// neither of which produces any signal in this process.
func TestEnsureProtection_ReplacesAMissingProtectiveOrder(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatus: &domain.AlgoOrderStatus{State: "canceled"},
	}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, tp, contracts, algo := dec("95"), dec("110"), dec("3"), "algo-gone"
	order := port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, TPPx: &tp, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}
	repo.realOrders[1] = order

	rt.ensureProtection(context.Background(), []port.RealOrder{order}, testLogger())

	if len(exchange.placedAlgoOrders) != 1 {
		t.Fatalf("a vanished protective order must be re-placed, got %d placements", len(exchange.placedAlgoOrders))
	}
	if !exchange.placedAlgoOrders[0].SLTriggerPx.Equal(sl) {
		t.Errorf("the replacement must carry the position's own stop %v, got %v", sl, exchange.placedAlgoOrders[0].SLTriggerPx)
	}
}

// A still-live protective order must be left alone. Re-placing one that is already resting would
// leave two protective orders on the same position, which on a hedge-mode account can close it
// twice — the opposite of protection.
func TestEnsureProtection_LeavesALiveProtectiveOrderAlone(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{algoStatus: &domain.AlgoOrderStatus{State: "live"}}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("3"), "algo-live"
	order := port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}

	rt.ensureProtection(context.Background(), []port.RealOrder{order}, testLogger())

	if len(exchange.placedAlgoOrders) != 0 {
		t.Errorf("a live protective order must not be duplicated, got %d new placements", len(exchange.placedAlgoOrders))
	}
}

// A FAILED verification read is not evidence the order is gone. Re-placing on an unreadable status
// would risk a duplicate protective order every time the exchange is briefly unreachable.
func TestEnsureProtection_DoesNotReplaceWhenTheCheckItselfFails(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{getAlgoErr: errors.New("timeout")}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("3"), "algo-unknown"
	order := port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}

	rt.ensureProtection(context.Background(), []port.RealOrder{order}, testLogger())

	if len(exchange.placedAlgoOrders) != 0 {
		t.Errorf("an unreadable status must not trigger a replacement, got %d placements", len(exchange.placedAlgoOrders))
	}
}

// Closing a position cancels its resting protective order. A live conditional order left behind on
// a flat account can trigger and OPEN a brand-new position — the failure mode worth an explicit
// cancel rather than relying on the exchange's own cleanup.
func TestCloseReal_CancelsTheRestingProtectiveOrder(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("3"), "algo-9"
	order := port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}
	repo.realOrders[1] = order

	if err := rt.closeReal(context.Background(), order, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("closeReal: %v", err)
	}

	if len(exchange.canceledAlgoIDs) != 1 || exchange.canceledAlgoIDs[0] != algo {
		t.Fatalf("closing must cancel the resting protective order %q, got %v", algo, exchange.canceledAlgoIDs)
	}
}

// A failed cancel must not make a completed close look failed. The position IS flat; a leftover
// conditional order is a smaller problem than a row that wrongly reports the close as unsuccessful.
func TestCloseReal_SucceedsEvenIfCancellingProtectionFails(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{cancelAlgoErr: errors.New("cancel failed")}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("3"), "algo-9"
	order := port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}
	repo.realOrders[1] = order

	if err := rt.closeReal(context.Background(), order, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("a failed protective-order cancel must not fail the close: %v", err)
	}
	if repo.realOrders[1].ClosedAt == nil {
		t.Error("the position must still be recorded as closed")
	}
}

// Both levels ride on ONE order, so OKX's own OCO handling cancels the loser when the winner fires.
// Two separate orders would leave the losing side resting after the position closed.
func TestProtectionRequest_CarriesBothLevelsOnOneOrder(t *testing.T) {
	rt := newTestRealTrader(newFakeRepository(), &fakeExchangeClient{}, nil, nil)

	sl, tp, contracts := dec("95"), dec("110"), dec("3")
	order := port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"),
		SLPx: &sl, TPPx: &tp, Contracts: &contracts,
	}

	req, ok := rt.protectionRequest(order)
	if !ok {
		t.Fatal("a position with both levels must produce a protective order")
	}
	if !req.SLTriggerPx.Equal(sl) || !req.TPTriggerPx.Equal(tp) {
		t.Errorf("one order must carry both levels, got sl=%v tp=%v", req.SLTriggerPx, req.TPTriggerPx)
	}
}

// Two reconciliation passes racing — the periodic poll and a private-WebSocket-triggered one — must
// close a stale position exactly ONCE. Both passes read the local and remote state and then act on
// the difference, so without serialization each can observe the same still-open row and each flatten
// it, which on a real account is a second unwanted position in the opposite direction.
//
// Run under -race; mutation-checked by removing reconcileMu (fails with 2 closes).
func TestReconcile_ConcurrentPassesCloseAStalePositionOnlyOnce(t *testing.T) {
	repo := newFakeRepository()
	// The exchange reports flat while the local row is still open — the stale-position case.
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("3"), "algo-1"
	repo.realOrders[1] = port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}

	// Release both goroutines from the same channel so they contend for real, rather than running
	// one after the other and passing regardless of any locking.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rt.ReconcileNow(context.Background(), testLogger())
		}()
	}
	close(start)
	wg.Wait()

	closed := 0
	for _, o := range repo.realOrders {
		if o.ClosedAt != nil {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("the stale position must be closed exactly once, got %d closed rows", closed)
	}
}

// A position whose flatten is already in flight ('closing') is still listed as open — the real
// Postgres query includes that status, since the exchange position genuinely still exists until
// the flatten fills. Asking the model to move its levels then is pointless work on a position
// already on its way out, and would amend an order about to be cancelled.
//
// 'closing' rather than 'opening' here is deliberate: ListRealPositions filters 'opening' out on
// its own, so a test using it would pass with or without the guard and prove nothing.
func TestRunUpdates_SkipsPositionsThatAreNotHoldingExposure(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionUpdate, SLPx: dec("98")}}
	rt := newTestRealTrader(repo, exchange, model, nil)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

	sl := dec("95")
	repo.realOrders[1] = port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "closing", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"), ExchangeAlgoOrderID: algoIDPtr("a1"),
	}

	// The conductor returns false the first time it sees an order (it has no prior PnL to compare
	// against), so one priming pass is needed before the update path is genuinely reachable —
	// without it this test would pass whether the guard exists or not.
	rt.conductor().ShouldUpdate(1, dec("0"), time.Now().Add(-time.Hour))

	rt.runUpdates(context.Background(), "1m", dec("100"), testLogger())

	// The model must not even be CONSULTED. amendProtection would refuse an order with no resting
	// protective order anyway, so asserting only on the amend count would pass with or without the
	// guard — the behavior that actually differs is whether a decision was requested at all.
	if model.predictCalls != 0 {
		t.Errorf("a closing position must not be sent to the model, got %d predict calls", model.predictCalls)
	}
	if len(exchange.amendedAlgoOrders) != 0 {
		t.Errorf("a closing position must not produce an exchange amend, got %d", len(exchange.amendedAlgoOrders))
	}
	if repo.realOrders[1].SLPx == nil || !repo.realOrders[1].SLPx.Equal(sl) {
		t.Errorf("a closing position's stored stop must be untouched, got %v", repo.realOrders[1].SLPx)
	}
}

// algoIDPtr is a small helper for fixtures that need a resting-order id.
func algoIDPtr(id string) *string { return &id }

// The bug behind real orders 39 and 40 (2026-09-09): OKX's own stop-loss closed the position, and
// the reconciliation poll recorded it as a MANUAL close at the position's ENTRY price with no
// close data at all. The reason was wrong, and because a close price equal to entry implies a trade
// that went nowhere, the recorded loss was ~15x smaller than the real one (-0.018 against OKX's
// actual -0.265).
//
// The exchange knew all of this and was never asked.
func TestReconcile_RecordsAnExchangeStopLossAsSLWithItsRealNumbers(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		// Flat on the exchange, with the protective order showing it fired.
		algoStatus: &domain.AlgoOrderStatus{
			State: "effective", ActualSide: "sl", OrdID: "close-ord-1", AlgoID: "algo-1",
		},
		// The order the trigger created: the real fill price, PnL and fee.
		orderStatus: &domain.OrderStatus{
			State: "filled", AvgPx: dec("0.000003527"), AccFillSz: dec("5"),
			Pnl: dec("-0.265"), Fee: dec("-0.0088"),
		},
	}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("0.000003527"), dec("5"), "algo-1"
	repo.realOrders[1] = port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("0.00000358"),
		SLPx: &sl, Size: dec("1.81"), Leverage: dec("9.88"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}

	rt.reconcile(context.Background(), testLogger())

	got := repo.realOrders[1]
	if got.ClosedAt == nil {
		t.Fatal("the position must be closed locally once the exchange reports flat")
	}
	if got.CloseReason == nil || *got.CloseReason != "sl" {
		t.Errorf("a stop-loss the exchange executed must be recorded as sl, got %v", got.CloseReason)
	}
	// The close price must be the exchange's FILL price, not the entry price.
	if got.ClosePx == nil || !got.ClosePx.Equal(dec("0.000003527")) {
		t.Errorf("close price must come from the exchange (0.000003527), got %v", got.ClosePx)
	}
	if got.ExchangeRealizedPnL == nil || !got.ExchangeRealizedPnL.Equal(dec("-0.265")) {
		t.Errorf("the exchange's own realized PnL must be recorded, got %v", got.ExchangeRealizedPnL)
	}
	if got.ExchangeFee == nil || !got.ExchangeFee.Equal(dec("-0.0088")) {
		t.Errorf("the exchange's own fee must be recorded, got %v", got.ExchangeFee)
	}
}

// A position that vanished from the exchange WITHOUT its protective order firing is still a manual
// close — someone closed it in the OKX app, or it was liquidated. The fix must not relabel those,
// which is why the old behavior is kept as the fallback rather than removed.
func TestReconcile_KeepsManualWhenTheProtectiveOrderDidNotFire(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatus: &domain.AlgoOrderStatus{State: "canceled", AlgoID: "algo-1"},
	}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("5"), "algo-1"
	repo.realOrders[1] = port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}

	rt.reconcile(context.Background(), testLogger())

	if got := repo.realOrders[1]; got.CloseReason == nil || *got.CloseReason != "manual" {
		t.Errorf("a close the protective order did not cause must stay manual, got %v", got.CloseReason)
	}
}

// Closing is idempotent. Real order 38 was closed twice by two reconciliation passes 3 seconds
// apart, and the second write replaced a -0.014 loss with a +0.472 gain — both wrong, but the
// second should never have landed. The guard is in the database, so it holds across engines and
// across processes, which an in-process mutex cannot.
func TestCloseReal_SecondCloseDoesNotOverwriteTheFirst(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	contracts := dec("5")
	order := port.RealOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		Size: dec("10"), Leverage: dec("1"), Contracts: &contracts,
	}
	repo.realOrders[1] = order

	if err := rt.closeReal(context.Background(), order, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("first close: %v", err)
	}
	first := repo.realOrders[1]

	// A second close of the SAME order, as a racing pass would attempt. It must not error (the
	// position is closed, which is what the caller wanted) and must not change the outcome.
	if err := rt.closeReal(context.Background(), order, dec("90"), "sl", testLogger()); err != nil {
		t.Fatalf("a second close must be a no-op, not an error: %v", err)
	}
	second := repo.realOrders[1]

	if first.CloseReason == nil || second.CloseReason == nil || first.ClosePx == nil || second.ClosePx == nil {
		t.Fatalf("both reads must show a closed order, got first=%v/%v second=%v/%v",
			first.CloseReason, first.ClosePx, second.CloseReason, second.ClosePx)
	}
	if *second.CloseReason != *first.CloseReason || !second.ClosePx.Equal(*first.ClosePx) {
		t.Fatalf("the first close must stand: was (%s @ %v), became (%s @ %v)",
			*first.CloseReason, *first.ClosePx, *second.CloseReason, *second.ClosePx)
	}
}

// OKX reports state="effective" the moment a protective order fires, but fills in WHICH side fired
// (actualSide) a beat later. Real order 43 (2026-09-10) was read in exactly that window and
// recorded as a manual close at its entry price — minutes after the code meant to prevent that
// shipped. The read is retried rather than accepting the first empty answer.
func TestCloseFacts_RetriesUntilTheExchangeReportsWhichSideFired(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatusQueue: []domain.AlgoOrderStatus{
			{State: "effective", AlgoID: "algo-1"},                                       // fired, side not filled in yet
			{State: "effective", AlgoID: "algo-1", ActualSide: "sl", OrdID: "close-ord"}, // now it is
		},
		orderStatus: &domain.OrderStatus{
			State: "filled", AvgPx: dec("2443.14"), AccFillSz: dec("1"),
			Pnl: dec("-0.31"), Fee: dec("-0.004"),
		},
	}
	rt := newTestRealTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("2443.14"), dec("1"), "algo-1"
	order := port.RealOrder{
		ID: 43, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("2456.11"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("9.46"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}

	reason, closePx, exPnL, _ := rt.closeFactsFromExchange(order, testLogger())

	if reason != "sl" {
		t.Errorf("the retry must recover the real reason, got %q", reason)
	}
	if !closePx.Equal(dec("2443.14")) {
		t.Errorf("close price must be the exchange's fill, got %v", closePx)
	}
	if exPnL == nil || !exPnL.Equal(dec("-0.31")) {
		t.Errorf("the exchange's PnL must be recorded, got %v", exPnL)
	}
}
