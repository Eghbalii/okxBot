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

// openOneBotPosition drives the real open path once and returns the resulting position, so the
// tests below assert against a position produced by the ACTUAL open flow rather than a
// hand-constructed row — the point being that protection is placed by that flow, not bolted on.
func openOneBotPosition(t *testing.T, repo *fakeRepository, exchange *fakeExchangeClient) []port.BotOrder {
	t.Helper()
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestBotTrader(repo, exchange, model, strategies)
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
//
// SL and TP are placed as TWO SEPARATE orders (2026-09-22) — buySignal() carries both a SL and a
// TP, so this test's position gets both legs, letting it assert on each independently rather than
// on one order that would (per the bug this split fixes) silently be missing its TP side.
func TestOpenBot_RestsStopLossOnTheExchange(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}

	open := openOneBotPosition(t, repo, exchange)
	if len(open) != 1 {
		t.Fatalf("expected 1 open position, got %d", len(open))
	}

	if len(exchange.placedAlgoOrders) != 2 {
		t.Fatalf("opening a real position with both SL and TP levels must rest TWO separate protective orders, got %d",
			len(exchange.placedAlgoOrders))
	}
	var slAlgo, tpAlgo *domain.AlgoOrderRequest
	for i, a := range exchange.placedAlgoOrders {
		if a.SLTriggerPx.IsPositive() {
			slAlgo = &exchange.placedAlgoOrders[i]
		}
		if a.TPTriggerPx.IsPositive() {
			tpAlgo = &exchange.placedAlgoOrders[i]
		}
	}
	if slAlgo == nil {
		t.Fatal("one of the two orders must carry the stop-loss trigger")
	}
	if tpAlgo == nil {
		t.Fatal("one of the two orders must carry the take-profit trigger")
	}
	if slAlgo.TPTriggerPx.IsPositive() {
		t.Error("the SL order must not ALSO carry a TP trigger — this is the combined-OCO shape found to silently drop TP")
	}
	if tpAlgo.SLTriggerPx.IsPositive() {
		t.Error("the TP order must not ALSO carry an SL trigger")
	}
	// The exchange's stop must be the SAME level the position records. A protective order resting
	// at a different price than the row shows is the divergence this feature exists to prevent.
	if open[0].SLPx == nil || !slAlgo.SLTriggerPx.Equal(*open[0].SLPx) {
		t.Errorf("the exchange's stop %v must match the position's own stop %v", slAlgo.SLTriggerPx, open[0].SLPx)
	}
	if open[0].TPPx == nil || !tpAlgo.TPTriggerPx.Equal(*open[0].TPPx) {
		t.Errorf("the exchange's target %v must match the position's own target %v", tpAlgo.TPTriggerPx, open[0].TPPx)
	}
	// A protective order closes the position, so it is the OPPOSITE side of the entry.
	if slAlgo.Side != "sell" {
		t.Errorf("a protective order for a long must be a sell, got %q", slAlgo.Side)
	}
	// It must be sized to the contracts actually filled — a protective order for the wrong size
	// leaves part of the position unprotected.
	if open[0].Contracts == nil || !slAlgo.Sz.Equal(*open[0].Contracts) {
		t.Errorf("the protective order size %v must match the filled contracts %v", slAlgo.Sz, open[0].Contracts)
	}
	// Both ids must be recorded, or no later adjustment could reach them and no close could cancel them.
	if open[0].ExchangeAlgoOrderID == nil || *open[0].ExchangeAlgoOrderID == "" {
		t.Error("the resting SL order's algoId must be recorded on the position")
	}
	if open[0].ExchangeTPAlgoOrderID == nil || *open[0].ExchangeTPAlgoOrderID == "" {
		t.Error("the resting TP order's algoId must be recorded on the position")
	}
}

// A position that cannot be protected is CLOSED again rather than kept (2026-09-09 decision). This
// is the deliberate answer to "what if the SL never makes it onto the exchange" — the alternative,
// holding an unprotected leveraged position and hoping a retry succeeds, is the exposure this
// whole change removes.
func TestOpenBot_ClosesThePositionWhenProtectionCannotBePlaced(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{placeAlgoErr: errors.New("exchange rejected the algo order")}

	open := openOneBotPosition(t, repo, exchange)
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
//
// SL and TP are two SEPARATE orders (2026-09-22) — algoStatus applies to BOTH legs' GetAlgoOrder
// reads in this fake, so both read as "canceled" (vanished) and both get re-placed. Asserting 2
// placements, one per side, is what proves ensureProtection checks each leg independently rather
// than treating the position as protected once any one algoId is present.
func TestEnsureProtection_ReplacesAMissingProtectiveOrder(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatus: &domain.AlgoOrderStatus{State: "canceled"},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp, contracts, slAlgo, tpAlgo := dec("95"), dec("110"), dec("3"), "algo-sl-gone", "algo-tp-gone"
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, TPPx: &tp, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &slAlgo, ExchangeTPAlgoOrderID: &tpAlgo,
	}
	repo.realOrders[1] = order

	rt.ensureProtection(context.Background(), []port.BotOrder{order}, testLogger())

	if len(exchange.placedAlgoOrders) != 2 {
		t.Fatalf("both vanished protective order legs must be re-placed, got %d placements", len(exchange.placedAlgoOrders))
	}
	var gotSL, gotTP bool
	for _, a := range exchange.placedAlgoOrders {
		if a.SLTriggerPx.Equal(sl) {
			gotSL = true
		}
		if a.TPTriggerPx.Equal(tp) {
			gotTP = true
		}
	}
	if !gotSL {
		t.Errorf("the replacement must carry the position's own stop %v", sl)
	}
	if !gotTP {
		t.Errorf("the replacement must carry the position's own target %v", tp)
	}
}

// The ONE-legged case: only the TP leg has vanished (SL is still live). ensureProtection must
// re-place TP alone, not touch the still-good SL — re-placing a live SL would rest a second SL
// order on top of a perfectly good one.
func TestEnsureProtection_ReplacesOnlyTheMissingLeg(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatusByID: map[string]domain.AlgoOrderStatus{
			"algo-sl-live": {State: "live", SLTriggerPx: dec("95")},
			"algo-tp-gone": {State: "canceled"},
		},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp, contracts, slAlgo, tpAlgo := dec("95"), dec("110"), dec("3"), "algo-sl-live", "algo-tp-gone"
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, TPPx: &tp, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &slAlgo, ExchangeTPAlgoOrderID: &tpAlgo,
	}
	repo.realOrders[1] = order

	rt.ensureProtection(context.Background(), []port.BotOrder{order}, testLogger())

	if len(exchange.placedAlgoOrders) != 1 {
		t.Fatalf("only the missing TP leg must be re-placed, got %d placements", len(exchange.placedAlgoOrders))
	}
	if !exchange.placedAlgoOrders[0].TPTriggerPx.Equal(tp) {
		t.Errorf("the replacement must be the TP leg carrying %v, got %v", tp, exchange.placedAlgoOrders[0].TPTriggerPx)
	}
	if exchange.placedAlgoOrders[0].SLTriggerPx.IsPositive() {
		t.Error("the still-live SL must NOT have been re-placed")
	}
}

// The stranded-order case slProtectionRequest's own doc comment names as the risk the SL/TP split
// reintroduces: SL has TRIGGERED (the position is closing), and TP is still LIVE. ensureProtection
// must cancel the now-stranded TP so it cannot later fire against an unrelated future position on
// this net_mode account — the exact scenario that motivated keeping SL and TP as one OCO order in
// the first place, now handled after the fact instead of never allowed to occur.
func TestEnsureProtection_CancelsTheStrandedSideAfterItsSiblingTriggers(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatusByID: map[string]domain.AlgoOrderStatus{
			"algo-sl-fired": {State: "effective", SLTriggerPx: dec("95")},
			"algo-tp-live":  {State: "live", TPTriggerPx: dec("110")},
		},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp, contracts, slAlgo, tpAlgo := dec("95"), dec("110"), dec("3"), "algo-sl-fired", "algo-tp-live"
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, TPPx: &tp, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &slAlgo, ExchangeTPAlgoOrderID: &tpAlgo,
	}
	repo.realOrders[1] = order

	rt.ensureProtection(context.Background(), []port.BotOrder{order}, testLogger())

	if len(exchange.placedAlgoOrders) != 0 {
		t.Errorf("a triggered leg must not cause any new placement, got %d", len(exchange.placedAlgoOrders))
	}
	if len(exchange.canceledAlgoIDs) != 1 || exchange.canceledAlgoIDs[0] != tpAlgo {
		t.Errorf("the still-live TP (%s) must be cancelled once its SL sibling has fired, got cancellations %v",
			tpAlgo, exchange.canceledAlgoIDs)
	}
}

// The mirror case: TP has fired, SL is still live. The still-live SL must be cancelled.
func TestEnsureProtection_CancelsTheStrandedSLAfterTPTriggers(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		algoStatusByID: map[string]domain.AlgoOrderStatus{
			"algo-sl-live":  {State: "live", SLTriggerPx: dec("95")},
			"algo-tp-fired": {State: "effective", TPTriggerPx: dec("110")},
		},
	}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp, contracts, slAlgo, tpAlgo := dec("95"), dec("110"), dec("3"), "algo-sl-live", "algo-tp-fired"
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, TPPx: &tp, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &slAlgo, ExchangeTPAlgoOrderID: &tpAlgo,
	}
	repo.realOrders[1] = order

	rt.ensureProtection(context.Background(), []port.BotOrder{order}, testLogger())

	if len(exchange.canceledAlgoIDs) != 1 || exchange.canceledAlgoIDs[0] != slAlgo {
		t.Errorf("the still-live SL (%s) must be cancelled once its TP sibling has fired, got cancellations %v",
			slAlgo, exchange.canceledAlgoIDs)
	}
}

// A still-live protective order must be left alone. Re-placing one that is already resting would
// leave two protective orders on the same position, which on a hedge-mode account can close it
// twice — the opposite of protection.
func TestEnsureProtection_LeavesALiveProtectiveOrderAlone(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{algoStatus: &domain.AlgoOrderStatus{State: "live"}}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("3"), "algo-live"
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}

	rt.ensureProtection(context.Background(), []port.BotOrder{order}, testLogger())

	if len(exchange.placedAlgoOrders) != 0 {
		t.Errorf("a live protective order must not be duplicated, got %d new placements", len(exchange.placedAlgoOrders))
	}
}

// A FAILED verification read is not evidence the order is gone. Re-placing on an unreadable status
// would risk a duplicate protective order every time the exchange is briefly unreachable.
func TestEnsureProtection_DoesNotReplaceWhenTheCheckItselfFails(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{getAlgoErr: errors.New("timeout")}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("3"), "algo-unknown"
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}

	rt.ensureProtection(context.Background(), []port.BotOrder{order}, testLogger())

	if len(exchange.placedAlgoOrders) != 0 {
		t.Errorf("an unreadable status must not trigger a replacement, got %d placements", len(exchange.placedAlgoOrders))
	}
}

// Closing a position cancels its resting protective order. A live conditional order left behind on
// a flat account can trigger and OPEN a brand-new position — the failure mode worth an explicit
// cancel rather than relying on the exchange's own cleanup.
func TestCloseBot_CancelsTheRestingProtectiveOrder(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("3"), "algo-9"
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}
	repo.realOrders[1] = order

	if err := rt.closeBot(context.Background(), order, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("closeBot: %v", err)
	}

	if len(exchange.canceledAlgoIDs) != 1 || exchange.canceledAlgoIDs[0] != algo {
		t.Fatalf("closing must cancel the resting protective order %q, got %v", algo, exchange.canceledAlgoIDs)
	}
}

// A failed cancel must not make a completed close look failed. The position IS flat; a leftover
// conditional order is a smaller problem than a row that wrongly reports the close as unsuccessful.
func TestCloseBot_SucceedsEvenIfCancellingProtectionFails(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{cancelAlgoErr: errors.New("cancel failed")}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("3"), "algo-9"
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		SLPx: &sl, Size: dec("10"), Leverage: dec("1"),
		Contracts: &contracts, ExchangeAlgoOrderID: &algo,
	}
	repo.realOrders[1] = order

	if err := rt.closeBot(context.Background(), order, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("a failed protective-order cancel must not fail the close: %v", err)
	}
	if repo.realOrders[1].ClosedAt == nil {
		t.Error("the position must still be recorded as closed")
	}
}

// SL and TP are placed as TWO SEPARATE orders (2026-09-22, revising the original combined-OCO
// design) — the combined form was found live to silently drop the TP side on this account's
// X-Perp instruments, even though both trigger prices decoded correctly on the Go side before the
// request left this process. slProtectionRequest/tpProtectionRequest each produce a single-sided
// request; neither one's request carries the other side's trigger price.
func TestProtectionRequest_PlacesSLAndTPAsSeparateOrders(t *testing.T) {
	rt := newTestBotTrader(newFakeRepository(), &fakeExchangeClient{}, nil, nil)

	sl, tp, contracts := dec("95"), dec("110"), dec("3")
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"),
		SLPx: &sl, TPPx: &tp, Contracts: &contracts,
	}

	slReq, slOK := rt.slProtectionRequest(order)
	if !slOK {
		t.Fatal("a position with an SL level must produce an SL order")
	}
	if !slReq.SLTriggerPx.Equal(sl) {
		t.Errorf("SL request must carry the SL trigger, got %v", slReq.SLTriggerPx)
	}
	if slReq.TPTriggerPx.IsPositive() {
		t.Errorf("SL-only request must NOT carry a TP trigger, got %v — this is the exact shape that was found to silently drop TP", slReq.TPTriggerPx)
	}

	tpReq, tpOK := rt.tpProtectionRequest(order)
	if !tpOK {
		t.Fatal("a position with a TP level must produce a TP order")
	}
	if !tpReq.TPTriggerPx.Equal(tp) {
		t.Errorf("TP request must carry the TP trigger, got %v", tpReq.TPTriggerPx)
	}
	if tpReq.SLTriggerPx.IsPositive() {
		t.Errorf("TP-only request must NOT carry an SL trigger, got %v", tpReq.SLTriggerPx)
	}
}

// placeProtection places SL first and, per explicit operator instruction, prioritizes it: SL
// failing closes the position (tested via openBot's own caller), but TP failing after SL succeeds
// must NOT undo the SL placement — the position stays protected on the loss side while
// ensureProtection's next reconciliation pass retries TP.
func TestPlaceProtection_TPFailureDoesNotUndoSL(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{placeAlgoOrderFailOn: "tp"}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp, contracts := dec("95"), dec("110"), dec("3")
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"),
		SLPx: &sl, TPPx: &tp, Contracts: &contracts,
	}
	repo.realOrders[1] = order

	slAlgoID, tpAlgoID, err := rt.placeProtection(context.Background(), order, true, true, testLogger())
	if err != nil {
		t.Fatalf("a TP failure alone must not fail placeProtection: %v", err)
	}
	if slAlgoID == "" {
		t.Error("SL must have been placed despite the TP failure")
	}
	if tpAlgoID != "" {
		t.Error("TP must NOT have an algoId — it failed to place")
	}
}

// The reconciliation loop must only re-place the side that is actually missing — re-placing a
// side that is already live would rest a second order on top of a perfectly good one.
func TestPlaceProtection_WantFlagsControlWhichLegIsPlaced(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, tp, contracts := dec("95"), dec("110"), dec("3")
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", EntryPx: dec("100"),
		SLPx: &sl, TPPx: &tp, Contracts: &contracts,
	}
	repo.realOrders[1] = order

	slAlgoID, tpAlgoID, err := rt.placeProtection(context.Background(), order, false, true, testLogger())
	if err != nil {
		t.Fatalf("placeProtection: %v", err)
	}
	if slAlgoID != "" {
		t.Error("wantSL=false must not place an SL order")
	}
	if tpAlgoID == "" {
		t.Error("wantTP=true must place a TP order")
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
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("3"), "algo-1"
	repo.realOrders[1] = port.BotOrder{
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
// 'closing' rather than 'opening' here is deliberate: ListBotPositions filters 'opening' out on
// its own, so a test using it would pass with or without the guard and prove nothing.
func TestRunUpdates_SkipsPositionsThatAreNotHoldingExposure(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionUpdate, SLPx: dec("98")}}
	rt := newTestBotTrader(repo, exchange, model, nil)
	rt.candles = map[string][]domain.Candle{"1m": realTraderWindow("100")}

	sl := dec("95")
	repo.realOrders[1] = port.BotOrder{
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
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("0.000003527"), dec("5"), "algo-1"
	repo.realOrders[1] = port.BotOrder{
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
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("95"), dec("5"), "algo-1"
	repo.realOrders[1] = port.BotOrder{
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
func TestCloseBot_SecondCloseDoesNotOverwriteTheFirst(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	rt := newTestBotTrader(repo, exchange, nil, nil)

	contracts := dec("5")
	order := port.BotOrder{
		ID: 1, InstID: rt.InstID, Side: "buy", Status: "filled", EntryPx: dec("100"),
		Size: dec("10"), Leverage: dec("1"), Contracts: &contracts,
	}
	repo.realOrders[1] = order

	if err := rt.closeBot(context.Background(), order, dec("110"), "tp", testLogger()); err != nil {
		t.Fatalf("first close: %v", err)
	}
	first := repo.realOrders[1]

	// A second close of the SAME order, as a racing pass would attempt. It must not error (the
	// position is closed, which is what the caller wanted) and must not change the outcome.
	if err := rt.closeBot(context.Background(), order, dec("90"), "sl", testLogger()); err != nil {
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
	rt := newTestBotTrader(repo, exchange, nil, nil)

	sl, contracts, algo := dec("2443.14"), dec("1"), "algo-1"
	order := port.BotOrder{
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
