package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// tenEngineDriver builds a driver over a roster of engines sharing one exchange and repository,
// mirroring cmd/trader's own construction (one RealTrader per instrument, one exchange client).
func tenEngineDriver(t *testing.T, symbols []string, exchange *fakeExchangeClient) (*ReconcileDriver, *fakeRepository) {
	t.Helper()
	repo := newFakeRepository()
	repo.accounts["real"] = port.AccountEquity{
		Mode: "real", InitialUSD: dec("1000"), EquityUSD: dec("1000"), AccountBalanceUSD: dec("1000"),
	}

	engines := make(map[string]*RealTrader, len(symbols))
	for _, sym := range symbols {
		rt := newTestRealTrader(repo, exchange, nil, nil)
		rt.InstID = sym
		rt.ExecInstID = sym
		engines[sym] = rt
	}
	return &ReconcileDriver{
		Engines:   engines,
		Exchange:  exchange,
		Logger:    testLogger(),
		InstType:  "SWAP",
		SettleCcy: "USDT",
	}, repo
}

func tenSymbols() []string {
	return []string{"BTC", "ETH", "SOL", "PEPE", "TRUMP", "ZEC", "ENA", "HYPE", "DOGE", "XAU"}
}

// TestReconcileAll_FetchesAccountDataOnceForTheWholeRoster is the whole point of the driver
// (CLAUDE.md §38.2): GetPositions/GetBalance are account-wide, so a 10-token roster previously
// issued 20 calls per cycle to learn what 2 calls carry, and OKX rate-limited it. The call COUNT is
// the behavior under test — the positions themselves are identical either way, which is exactly why
// this regressed unnoticed for so long.
func TestReconcileAll_FetchesAccountDataOnceForTheWholeRoster(t *testing.T) {
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	driver, _ := tenEngineDriver(t, tenSymbols(), exchange)

	driver.ReconcileAll(context.Background())

	if got := exchange.PositionsCalls(); got != 1 {
		t.Errorf("expected exactly 1 GetPositions call for a 10-token roster, got %d", got)
	}
	if got := exchange.BalanceCalls(); got != 1 {
		t.Errorf("expected exactly 1 GetBalance call for a 10-token roster, got %d", got)
	}
}

// TestReconcileAll_StillReconcilesEveryEngine guards the obvious way to "fix" the call count
// wrongly — reconciling one engine and skipping the rest would also produce 1 call each.
func TestReconcileAll_StillReconcilesEveryEngine(t *testing.T) {
	symbols := tenSymbols()
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	driver, repo := tenEngineDriver(t, symbols, exchange)

	// Every instrument has a locally-open position the exchange reports nothing for, so a
	// reconciled engine closes its row and a skipped one leaves it open.
	for i, sym := range symbols {
		id := int64(i + 1)
		repo.realOrders[id] = port.RealOrder{
			ID: id, InstID: sym, Status: "filled", Side: "buy",
			EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1"),
		}
	}

	driver.ReconcileAll(context.Background())

	for i, sym := range symbols {
		id := int64(i + 1)
		if repo.realOrders[id].ClosedAt == nil {
			t.Errorf("%s: expected the locally-stale position to be closed, but it is still open", sym)
		}
	}
}

// TestReconcileAll_RecordsEquityOncePerPass covers the second half of the waste: recordEquityReal
// writes ONE shared account row, so ten engines calling it made ten transactions for one row's work
// — and when the delta was nonzero, whichever engine ran first stamped its own instID on the
// history row, attributing an account-wide balance change to one arbitrary token.
func TestReconcileAll_RecordsEquityOncePerPass(t *testing.T) {
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1250")}}}
	driver, repo := tenEngineDriver(t, tenSymbols(), exchange)

	driver.ReconcileAll(context.Background())

	// The COUNT, not the resulting rows: a redundant call is invisible in the data because the
	// second one sees a zero delta and writes no history row — which is exactly why ten engines
	// each opening a transaction against one shared account row went unnoticed.
	if got := repo.recordExchangeBalanceCalls; got != 1 {
		t.Errorf("expected exactly 1 equity write for one balance change across 10 engines, got %d", got)
	}
	if got := len(repo.equityPoints); got != 1 {
		t.Fatalf("expected exactly 1 equity history row, got %d", got)
	}
	if got := repo.equityPoints[0].DeltaUSD; !got.Equal(dec("250")) {
		t.Errorf("expected the full +250 delta recorded once, got %s", got)
	}
}

// TestReconcileAll_SkipsThePassWhenPositionsCannotBeRead is the safety property. Every branch of
// the drift comparison derives from GetPositions, and an absent response reads as "the exchange
// reports flat" — the branch that CLOSES local positions. A failed fetch must skip the pass, never
// be treated as an empty one.
func TestReconcileAll_SkipsThePassWhenPositionsCannotBeRead(t *testing.T) {
	exchange := &fakeExchangeClient{
		getPositionsErr: errors.New("50011 Too Many Requests"),
		balances:        []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	driver, repo := tenEngineDriver(t, tenSymbols(), exchange)
	repo.realOrders[1] = port.RealOrder{
		ID: 1, InstID: "BTC", Status: "filled", Side: "buy",
		EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1"),
	}

	driver.ReconcileAll(context.Background())

	if repo.realOrders[1].ClosedAt != nil {
		t.Error("a failed positions read must not close a local position: an unreadable exchange is not a flat one")
	}
}

// TestReconcileInstrument_FetchesFreshDataForOneEngine covers the WebSocket push path. A push means
// that instrument genuinely changed, so it acts on a fresh snapshot rather than a poll-interval-old
// one — reusing the stale snapshot would give up the latency the socket exists to provide — but it
// still costs 2 account calls, not one per engine.
func TestReconcileInstrument_FetchesFreshDataForOneEngine(t *testing.T) {
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	driver, repo := tenEngineDriver(t, tenSymbols(), exchange)
	repo.realOrders[1] = port.RealOrder{
		ID: 1, InstID: "ETH", Status: "filled", Side: "buy",
		EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1"),
	}
	repo.realOrders[2] = port.RealOrder{
		ID: 2, InstID: "SOL", Status: "filled", Side: "buy",
		EntryPx: dec("100"), Size: dec("10"), Leverage: dec("1"),
	}

	if !driver.ReconcileInstrument(context.Background(), "ETH") {
		t.Fatal("expected ReconcileInstrument to report it handled a known instrument")
	}

	if got := exchange.PositionsCalls(); got != 1 {
		t.Errorf("expected 1 GetPositions call for a single pushed instrument, got %d", got)
	}
	if repo.realOrders[1].ClosedAt == nil {
		t.Error("expected the pushed instrument's stale position to be reconciled")
	}
	if repo.realOrders[2].ClosedAt != nil {
		t.Error("a push for ETH must not reconcile SOL: only the named instrument changed")
	}
}

// TestReconcileInstrument_UnknownInstrumentIsNotAnError covers an event for an instrument this
// process does not trade (another roster, or one disabled since) — nothing to reconcile, and no
// account call worth spending on it.
func TestReconcileInstrument_UnknownInstrumentIsNotAnError(t *testing.T) {
	exchange := &fakeExchangeClient{balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}}
	driver, _ := tenEngineDriver(t, tenSymbols(), exchange)

	if driver.ReconcileInstrument(context.Background(), "NOT-TRADED") {
		t.Error("expected ReconcileInstrument to report it did not handle an unknown instrument")
	}
	if got := exchange.PositionsCalls(); got != 0 {
		t.Errorf("expected no account call for an untraded instrument, got %d", got)
	}
}

// TestPositionFor_TreatsAZeroSizeRowAsFlat guards a real OKX behavior: it keeps returning a
// position row after the position closes, with Pos zeroed. Treating that as open would make every
// closed position look like it was still running, which is the branch that halts trading.
func TestPositionFor_TreatsAZeroSizeRowAsFlat(t *testing.T) {
	snap := AccountSnapshot{Positions: []domain.Position{
		{InstID: "BTC", Pos: dec("0"), PosSide: "long"},
		{InstID: "ETH", Pos: dec("3"), PosSide: "long"},
	}}

	if got := snap.PositionFor("BTC"); got != nil {
		t.Errorf("expected a zero-size position row to read as flat, got %+v", got)
	}
	if got := snap.PositionFor("ETH"); got == nil {
		t.Error("expected a real open position to be found")
	}
	if got := snap.PositionFor("SOL"); got != nil {
		t.Errorf("expected an instrument with no row at all to read as flat, got %+v", got)
	}
}

// TestFetchAccountSnapshot_BalanceFailureIsNotFatal: a balance read failure costs the equity
// timeline one sample, but must never stop the pass — the protective-order verification that shares
// it is what bounds how long a position can run unprotected.
func TestFetchAccountSnapshot_BalanceFailureIsNotFatal(t *testing.T) {
	exchange := &fakeExchangeClient{
		positions: []domain.Position{{InstID: "BTC", Pos: dec("1"), PosSide: "long"}},
		balances:  nil, // no balance row reported
	}

	snap, err := FetchAccountSnapshot(exchange, "SWAP", "USDT")
	if err != nil {
		t.Fatalf("expected a missing balance not to fail the snapshot, got %v", err)
	}
	if snap.HasBalance {
		t.Error("expected HasBalance false so an empty response is never recorded as a drained account")
	}
	if len(snap.Positions) != 1 {
		t.Errorf("expected positions to still be present, got %d", len(snap.Positions))
	}
}
