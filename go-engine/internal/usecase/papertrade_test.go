package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// fakeRepository is an in-memory port.Repository for testing, no real Postgres needed. Guarded
// by a mutex since PaperTrader runs one goroutine per bar consumer plus one for ticks, all of
// which can call into the repository concurrently (real Postgres handles this natively; this
// fake must emulate that instead of assuming single-goroutine test access).
type fakeRepository struct {
	mu      sync.Mutex
	nextID  int64
	orders  map[int64]port.PaperOrder
	candles []port.Candle
	budgets map[string]port.TokenBudget
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{orders: make(map[int64]port.PaperOrder), budgets: make(map[string]port.TokenBudget)}
}

func (r *fakeRepository) SaveCandle(ctx context.Context, c port.Candle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.candles = append(r.candles, c)
	return nil
}
func (r *fakeRepository) CreateStrategy(ctx context.Context, s port.StrategyConfig) (int64, error) {
	return 0, nil
}
func (r *fakeRepository) GetStrategy(ctx context.Context, id int64) (port.StrategyConfig, error) {
	return port.StrategyConfig{}, nil
}
func (r *fakeRepository) ListStrategies(ctx context.Context, instID string, enabledOnly bool) ([]port.StrategyConfig, error) {
	return nil, nil
}
func (r *fakeRepository) UpdateStrategyConfig(ctx context.Context, id int64, config json.RawMessage, enabled bool) error {
	return nil
}
func (r *fakeRepository) DeleteStrategy(ctx context.Context, id int64) error        { return nil }
func (r *fakeRepository) ResetStrategyToOrigin(ctx context.Context, id int64) error { return nil }
func (r *fakeRepository) CreateAssignment(ctx context.Context, a port.StrategyAssignment) (int64, error) {
	return 0, nil
}
func (r *fakeRepository) ListAssignments(ctx context.Context, instID string, enabledOnly bool) ([]port.StrategyAssignment, error) {
	return nil, nil
}
func (r *fakeRepository) SetAssignmentEnabled(ctx context.Context, id int64, enabled bool) error {
	return nil
}
func (r *fakeRepository) DeleteAssignment(ctx context.Context, id int64) error { return nil }
func (r *fakeRepository) StrategyStatsFor(ctx context.Context, strategyID int64) (port.StrategyStats, error) {
	return port.StrategyStats{}, nil
}
func (r *fakeRepository) ListPositions(ctx context.Context, f port.PositionFilter) ([]port.PaperOrder, error) {
	return nil, nil
}
func (r *fakeRepository) OpenPaperOrder(ctx context.Context, o port.PaperOrder) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	o.ID = r.nextID
	r.orders[o.ID] = o
	return o.ID, nil
}
func (r *fakeRepository) ClosePaperOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.orders[id]
	now := o.OpenedAt
	o.ClosedAt = &now
	o.CloseReason = &reason
	cp := closePx
	o.ClosePx = &cp
	pnl := realizedPnL
	o.RealizedPnL = &pnl
	r.orders[id] = o
	return nil
}
func (r *fakeRepository) UpdatePaperOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.orders[id]
	if !ok || o.ClosedAt != nil {
		return nil
	}
	o.SLPx = slPx
	o.TPPx = tpPx
	r.orders[id] = o
	return nil
}
func (r *fakeRepository) ForkPaperOrderWithSLTP(ctx context.Context, parentID int64, slPx, tpPx *decimal.Decimal) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	parent, ok := r.orders[parentID]
	if !ok || parent.ClosedAt != nil {
		return 0, fmt.Errorf("fork: parent order %d not open", parentID)
	}
	r.nextID++
	fork := parent
	fork.ID = r.nextID
	fork.SLPx = slPx
	fork.TPPx = tpPx
	fork.ParentOrderID = &parentID
	fork.Variant = "rl_adjusted"
	r.orders[fork.ID] = fork
	return fork.ID, nil
}
func (r *fakeRepository) GetTokenBudget(ctx context.Context, instID string, initialBudgetUSD decimal.Decimal) (port.TokenBudget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tb, ok := r.budgets[instID]; ok {
		return tb, nil
	}
	tb := port.TokenBudget{InstID: instID, BudgetUSD: initialBudgetUSD, EquityUSD: initialBudgetUSD}
	r.budgets[instID] = tb
	return tb, nil
}
func (r *fakeRepository) ApplyTokenPnL(ctx context.Context, instID string, pnl decimal.Decimal) (port.TokenBudget, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tb, ok := r.budgets[instID]
	if !ok {
		return port.TokenBudget{}, false, fmt.Errorf("apply pnl: no budget row for %s", instID)
	}
	tb.EquityUSD = tb.EquityUSD.Add(pnl)
	reset := false
	if tb.EquityUSD.Sign() <= 0 {
		tb.EquityUSD = tb.BudgetUSD
		tb.ResetCount++
		reset = true
	}
	r.budgets[instID] = tb
	return tb, reset, nil
}
func (r *fakeRepository) SLTPAdjustmentStats(ctx context.Context, instID string, since time.Time) ([]port.VariantStats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	adjustedBaselineIDs := make(map[int64]bool)
	for _, o := range r.orders {
		if o.Variant == "rl_adjusted" && o.ParentOrderID != nil {
			if b, ok := r.orders[*o.ParentOrderID]; ok && (instID == "" || b.InstID == instID) && !b.OpenedAt.Before(since) {
				adjustedBaselineIDs[*o.ParentOrderID] = true
			}
		}
	}

	baseline := port.VariantStats{Variant: "baseline", RealizedPnL: decimal.Zero}
	adjusted := port.VariantStats{Variant: "rl_adjusted", RealizedPnL: decimal.Zero}
	for _, o := range r.orders {
		if o.Variant == "baseline" || o.Variant == "" {
			if !adjustedBaselineIDs[o.ID] {
				continue
			}
			accumulateVariantStats(&baseline, o)
		} else if o.Variant == "rl_adjusted" && o.ParentOrderID != nil && adjustedBaselineIDs[*o.ParentOrderID] {
			accumulateVariantStats(&adjusted, o)
		}
	}
	return []port.VariantStats{baseline, adjusted}, nil
}
func accumulateVariantStats(vs *port.VariantStats, o port.PaperOrder) {
	if o.ClosedAt == nil {
		return
	}
	vs.ClosedCount++
	if o.CloseReason != nil && *o.CloseReason == "tp" {
		vs.Wins++
	}
	if o.CloseReason != nil && *o.CloseReason == "sl" {
		vs.Losses++
	}
	if o.RealizedPnL != nil {
		vs.RealizedPnL = vs.RealizedPnL.Add(*o.RealizedPnL)
	}
}
func (r *fakeRepository) ListSLTPAdjustmentPairs(ctx context.Context, instID string) ([]port.SLTPAdjustmentPair, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.SLTPAdjustmentPair
	for _, o := range r.orders {
		if o.Variant != "rl_adjusted" || o.ParentOrderID == nil {
			continue
		}
		b, ok := r.orders[*o.ParentOrderID]
		if !ok || (instID != "" && b.InstID != instID) {
			continue
		}
		out = append(out, port.SLTPAdjustmentPair{InstID: b.InstID, BaselineOrder: b, RLAdjustedOrder: o})
	}
	return out, nil
}
func (r *fakeRepository) ListOpenPaperOrders(ctx context.Context, instID string) ([]port.PaperOrder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []port.PaperOrder
	for _, o := range r.orders {
		if o.InstID == instID && o.ClosedAt == nil {
			out = append(out, o)
		}
	}
	return out, nil
}

// fakeExchangeForCandles only needs to satisfy GetCandles for the seed call in Run — not used by
// these tests, which call handleTick/evaluateStrategies directly, but kept for completeness.
type fakeExchangeForCandles struct{ candles []domain.Candle }

func (f *fakeExchangeForCandles) GetTicker(instID string) (domain.Ticker, error) {
	return domain.Ticker{}, nil
}
func (f *fakeExchangeForCandles) GetPositions(instType string) ([]domain.Position, error) {
	return nil, nil
}
func (f *fakeExchangeForCandles) GetBalance(ccy string) ([]domain.Balance, error) { return nil, nil }
func (f *fakeExchangeForCandles) GetCandles(instID, bar string, limit int) ([]domain.Candle, error) {
	return f.candles, nil
}
func (f *fakeExchangeForCandles) PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error) {
	return nil, nil
}
func (f *fakeExchangeForCandles) SetLeverage(req domain.LeverageChange) error { return nil }

// noopConsumer satisfies port.MarketDataConsumer without ever invoking the handler — sufficient
// for tests that drive PaperTrader through its handler methods directly.
type noopConsumer struct{}

func (noopConsumer) Run(ctx context.Context, handler func(ctx context.Context, data []byte) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func newTestPaperTrader(repo port.Repository, strategies []StrategyAssignment) *PaperTrader {
	return &PaperTrader{
		InstID:       "BTC-USDT-SWAP",
		Bars:         []string{"1m", "15m"},
		CandleWindow: 100,
		Strategies:   strategies,
		Exchange:     &fakeExchangeForCandles{},
		TickConsumer: noopConsumer{},
		CandleConsumers: map[string]port.MarketDataConsumer{
			"1m":  noopConsumer{},
			"15m": noopConsumer{},
		},
		Repo:          repo,
		NotionalUSD:   dec("100"),
		MaxOpenOrders: 3,
		Logger:        testLogger(),
		candles:       map[string][]domain.Candle{"1m": nil, "15m": nil},
	}
}

func TestCloseReason_SLHitOnBuy(t *testing.T) {
	sl := dec("99")
	order := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: &sl}
	reason, hit := closeReason(order, dec("99"))
	if !hit || reason != "sl" {
		t.Errorf("expected sl hit at price==SL, got hit=%v reason=%q", hit, reason)
	}
}

func TestCloseReason_TPHitOnSell(t *testing.T) {
	tp := dec("90")
	order := port.PaperOrder{Side: "sell", EntryPx: dec("100"), TPPx: &tp}
	reason, hit := closeReason(order, dec("90"))
	if !hit || reason != "tp" {
		t.Errorf("expected tp hit at price==TP for a sell, got hit=%v reason=%q", hit, reason)
	}
}

func TestCloseReason_PriceBetweenSLAndTPDoesNotClose(t *testing.T) {
	sl, tp := dec("99"), dec("102")
	order := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp}
	_, hit := closeReason(order, dec("100.5"))
	if hit {
		t.Errorf("expected no close for a price strictly between SL and TP")
	}
}

func TestRealizedPnL_ExactNoFloatDrift(t *testing.T) {
	// Regression test for the float64->decimal.Decimal migration.
	order := port.PaperOrder{Side: "buy", EntryPx: dec("9.0"), Size: dec("100"), Leverage: dec("5")}
	pnl := realizedPnL(order, dec("9.0")) // exact round-trip close, PnL must be exactly zero
	if !pnl.IsZero() {
		t.Errorf("expected exact zero PnL for a round-trip close, got %s", pnl.String())
	}

	pnlProfit := realizedPnL(order, dec("9.9")) // +10% move
	want := dec("50")                           // (9.9-9.0)/9.0 * 100 * 5 = 0.1 * 500 = 50
	if !pnlProfit.Equal(want) {
		t.Errorf("expected exact PnL %s, got %s", want, pnlProfit.String())
	}
}

func TestBuildPaperOrder_SLTPForBuyAndSell(t *testing.T) {
	buySignal := strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}
	buyOrder := buildPaperOrder("BTC-USDT-SWAP", dec("100"), buySignal, dec("100"), 0)
	if buyOrder.SLPx == nil || !buyOrder.SLPx.Equal(dec("99")) {
		t.Errorf("expected buy SL=99, got %v", buyOrder.SLPx)
	}
	if buyOrder.TPPx == nil || !buyOrder.TPPx.Equal(dec("102")) {
		t.Errorf("expected buy TP=102, got %v", buyOrder.TPPx)
	}

	sellSignal := strategy.Signal{Side: strategy.Sell, SLPct: dec("0.01"), TPPct: dec("0.02")}
	sellOrder := buildPaperOrder("BTC-USDT-SWAP", dec("100"), sellSignal, dec("100"), 0)
	if sellOrder.SLPx == nil || !sellOrder.SLPx.Equal(dec("101")) {
		t.Errorf("expected sell SL=101, got %v", sellOrder.SLPx)
	}
	if sellOrder.TPPx == nil || !sellOrder.TPPx.Equal(dec("98")) {
		t.Errorf("expected sell TP=98, got %v", sellOrder.TPPx)
	}
}

func TestMaxOpenOrders_GatesNewSignals(t *testing.T) {
	repo := newFakeRepository()
	// Pre-fill 3 open orders (= MaxOpenOrders) for this instrument.
	for i := 0; i < 3; i++ {
		_, _ = repo.OpenPaperOrder(context.Background(), port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Size: dec("100"), Leverage: dec("1")})
	}

	alwaysBuy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}
	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "1m", Strategy: alwaysBuy}})
	pt.candles["1m"] = []domain.Candle{{Open: dec("100"), High: dec("101"), Low: dec("99"), Close: dec("100"), Volume: dec("1")}}

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 3 {
		t.Errorf("expected MaxOpenOrders to gate new signals, still expected 3 open orders, got %d", len(open))
	}
}

func TestEvaluateStrategies_OnlyRunsStrategyAssignedToThatBar(t *testing.T) {
	repo := newFakeRepository()
	alwaysBuy1m := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}
	alwaysBuy15m := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02")}}

	pt := newTestPaperTrader(repo, []StrategyAssignment{
		{Bar: "1m", Strategy: alwaysBuy1m},
		{Bar: "15m", Strategy: alwaysBuy15m},
	})
	pt.candles["1m"] = []domain.Candle{{Close: dec("100")}}

	// A 1m candle close should only trigger the 1m-assigned strategy, not the 15m one.
	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Fatalf("expected exactly 1 order opened (from the 1m strategy only), got %d", len(open))
	}
}

func TestMonitorOpenOrders_ClosesOnSLHit(t *testing.T) {
	repo := newFakeRepository()
	sl := dec("99")
	id, _ := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("100"), Leverage: dec("1"),
	})

	pt := newTestPaperTrader(repo, nil)
	if err := pt.monitorOpenOrders(context.Background(), dec("99"), testLogger()); err != nil {
		t.Fatalf("monitorOpenOrders returned error: %v", err)
	}

	closed := repo.orders[id]
	if closed.ClosedAt == nil {
		t.Fatal("expected order to be closed after SL hit")
	}
	if closed.CloseReason == nil || *closed.CloseReason != "sl" {
		t.Errorf("expected close reason 'sl', got %v", closed.CloseReason)
	}
}

// stubStrategy always returns the configured signal, ignoring the candle input.
type stubStrategy struct{ signal strategy.Signal }

func (s *stubStrategy) Name() string                                            { return "stub" }
func (s *stubStrategy) Params() []strategy.ParamSpec                            { return nil }
func (s *stubStrategy) WithParams(map[string]decimal.Decimal) strategy.Strategy { return s }
func (s *stubStrategy) Evaluate(candles []strategy.Candle) (strategy.Signal, error) {
	return s.signal, nil
}

func TestHandleCandle_ParsesAndAppendsToWindow(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)

	event := candleEvent{
		InstID: "BTC-USDT-SWAP",
		Bar:    "1m",
		Candle: []string{"1700000000000", "100", "101", "99", "100.5", "10", "", "", "1"},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	if err := pt.handleCandle(context.Background(), "1m", data, testLogger()); err != nil {
		t.Fatalf("handleCandle returned error: %v", err)
	}

	if len(pt.candles["1m"]) != 1 {
		t.Fatalf("expected 1 candle in the 1m window, got %d", len(pt.candles["1m"]))
	}
	if !pt.candles["1m"][0].Close.Equal(dec("100.5")) {
		t.Errorf("expected close=100.5, got %s", pt.candles["1m"][0].Close)
	}
	if len(pt.candles["15m"]) != 0 {
		t.Errorf("expected the 15m window untouched by a 1m candle event, got %d entries", len(pt.candles["15m"]))
	}
	if len(repo.candles) != 1 {
		t.Errorf("expected finalized candle to be persisted, got %d saved", len(repo.candles))
	}
	if repo.candles[0].Bar != "1m" {
		t.Errorf("expected persisted candle to be tagged bar=1m, got %q", repo.candles[0].Bar)
	}
}

func TestHandleCandle_DifferentBarsMaintainIndependentWindows(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)

	oneMin := candleEvent{InstID: "BTC-USDT-SWAP", Bar: "1m", Candle: []string{"1700000000000", "100", "101", "99", "100.5", "10", "", "", "1"}}
	fifteenMin := candleEvent{InstID: "BTC-USDT-SWAP", Bar: "15m", Candle: []string{"1700000000000", "200", "201", "199", "200.5", "20", "", "", "1"}}

	oneMinData, _ := json.Marshal(oneMin)
	fifteenMinData, _ := json.Marshal(fifteenMin)

	if err := pt.handleCandle(context.Background(), "1m", oneMinData, testLogger()); err != nil {
		t.Fatalf("handleCandle(1m) returned error: %v", err)
	}
	if err := pt.handleCandle(context.Background(), "15m", fifteenMinData, testLogger()); err != nil {
		t.Fatalf("handleCandle(15m) returned error: %v", err)
	}

	if len(pt.candles["1m"]) != 1 || !pt.candles["1m"][0].Close.Equal(dec("100.5")) {
		t.Errorf("expected 1m window to have exactly the 1m candle, got %v", pt.candles["1m"])
	}
	if len(pt.candles["15m"]) != 1 || !pt.candles["15m"][0].Close.Equal(dec("200.5")) {
		t.Errorf("expected 15m window to have exactly the 15m candle, got %v", pt.candles["15m"])
	}
	if len(repo.candles) != 2 {
		t.Fatalf("expected both candles persisted, got %d", len(repo.candles))
	}
}

// TestHandleCandle_ConcurrentBarsDoNotRace is a regression test for a real crash found during
// the multi-timeframe dry run: each bar gets its own consumer goroutine (see Run), so concurrent
// handleCandle calls for different bars write to the shared e.candles map at the same time —
// caught in production as "fatal error: concurrent map writes" (a Go runtime crash, not just a
// -race warning). Run with `go test -race` to actually catch a regression here; without -race
// this only proves the code doesn't deadlock/panic under load, not that it's race-free.
func TestHandleCandle_ConcurrentBarsDoNotRace(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.Bars = []string{"1m", "15m", "1H"}
	pt.candles = map[string][]domain.Candle{"1m": nil, "15m": nil, "1H": nil}

	makeEvent := func(bar string, closePx string) []byte {
		e := candleEvent{InstID: "BTC-USDT-SWAP", Bar: bar, Candle: []string{"1700000000000", "100", "101", "99", closePx, "10", "", "", "1"}}
		data, _ := json.Marshal(e)
		return data
	}

	var wg sync.WaitGroup
	for _, bar := range []string{"1m", "15m", "1H"} {
		bar := bar
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				data := makeEvent(bar, "100.5")
				if err := pt.handleCandle(context.Background(), bar, data, testLogger()); err != nil {
					t.Errorf("handleCandle(%s) returned error: %v", bar, err)
				}
			}(i)
		}
	}
	wg.Wait()

	for _, bar := range []string{"1m", "15m", "1H"} {
		if len(pt.candles[bar]) == 0 {
			t.Errorf("expected candles recorded for bar %s after concurrent writes, got none", bar)
		}
	}
}

// fakeModelClientRL returns a fixed action on every Predict call — used to test the SL/TP
// adjustment/fork path deterministically.
type fakeModelClientRL struct {
	action domain.Action
	calls  int
}

func (f *fakeModelClientRL) Predict(ctx context.Context, obs domain.Observation) (*domain.Action, error) {
	f.calls++
	a := f.action
	return &a, nil
}

func TestAdjustOpenOrdersWithRL_ForksOnNonZeroAdjustment(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {
		{Close: dec("100")}, {Close: dec("101")}, {Close: dec("102")}, {Close: dec("105")},
	}, "15m": nil}
	pt.TokenBudgetUSD = dec("10")
	model := &fakeModelClientRL{action: domain.Action{SLAdjustPct: dec("0.02"), TPAdjustPct: dec("0.02")}}
	pt.Model = model

	sl, tp := dec("95"), dec("110")
	id, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, TPPx: &tp, Size: dec("100"), Leverage: dec("1"),
	})
	if err != nil {
		t.Fatalf("open baseline order: %v", err)
	}

	pt.adjustOpenOrdersWithRL(context.Background(), "1m", dec("105"), testLogger())

	if model.calls == 0 {
		t.Fatalf("expected Predict to be called")
	}

	orders, err := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("list open orders: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("expected baseline + 1 fork = 2 open orders, got %d", len(orders))
	}

	var baseline, fork *port.PaperOrder
	for i := range orders {
		o := orders[i]
		if o.ID == id {
			baseline = &o
		} else {
			fork = &o
		}
	}
	if baseline == nil || fork == nil {
		t.Fatalf("expected to find both baseline (id=%d) and a fork among %+v", id, orders)
	}
	if !baseline.SLPx.Equal(sl) || !baseline.TPPx.Equal(tp) {
		t.Errorf("expected baseline order's SL/TP untouched, got sl=%s tp=%s", baseline.SLPx, baseline.TPPx)
	}
	if fork.Variant != "rl_adjusted" || fork.ParentOrderID == nil || *fork.ParentOrderID != id {
		t.Errorf("expected fork tagged rl_adjusted with parent %d, got variant=%s parent=%v", id, fork.Variant, fork.ParentOrderID)
	}
	// SL should have tightened (moved up from 95, toward locking profit at price 105).
	if !fork.SLPx.GreaterThan(sl) {
		t.Errorf("expected fork's SL to have tightened above %s, got %s", sl, fork.SLPx)
	}
}

func TestAdjustOpenOrdersWithRL_NoOpActionDoesNotFork(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {{Close: dec("100")}}, "15m": nil}
	pt.Model = &fakeModelClientRL{action: domain.Action{}} // zero adjustment, matches the no-op fail-safe

	sl := dec("95")
	_, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), SLPx: &sl, Size: dec("100"), Leverage: dec("1"),
	})
	if err != nil {
		t.Fatalf("open baseline order: %v", err)
	}

	pt.adjustOpenOrdersWithRL(context.Background(), "1m", dec("100"), testLogger())

	orders, err := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("list open orders: %v", err)
	}
	if len(orders) != 1 {
		t.Errorf("expected no fork for a zero-adjustment action, got %d open orders", len(orders))
	}
}

func TestAdjustOpenOrdersWithRL_SkipsWhenNoBaselineOrders(t *testing.T) {
	repo := newFakeRepository()
	pt := newTestPaperTrader(repo, nil)
	pt.candles = map[string][]domain.Candle{"1m": {{Close: dec("100")}}, "15m": nil}
	model := &fakeModelClientRL{action: domain.Action{SLAdjustPct: dec("0.02")}}
	pt.Model = model

	pt.adjustOpenOrdersWithRL(context.Background(), "1m", dec("100"), testLogger())

	if model.calls != 0 {
		t.Errorf("expected Predict never called when there are no open baseline orders, got %d calls", model.calls)
	}
}

func TestSLTPAdjustmentStats_AggregatesPairedTradesOnly(t *testing.T) {
	repo := newFakeRepository()
	ctx := context.Background()

	// Baseline #1: has a fork, both closed. Baseline wins (tp), fork loses (sl). The fork must be
	// created (ForkPaperOrderWithSLTP requires an open parent) BEFORE the baseline is closed.
	closedAt1 := time.Now()
	pnl1 := dec("5")
	tpReason := "tp"
	baseline1ID, _ := repo.OpenPaperOrder(ctx, port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Variant: "baseline"})

	forkPnl1 := dec("-2")
	slReason := "sl"
	fork1ID, err := repo.ForkPaperOrderWithSLTP(ctx, baseline1ID, ptr(dec("95")), ptr(dec("110")))
	if err != nil {
		t.Fatalf("fork baseline1: %v", err)
	}
	repo.mu.Lock()
	f1 := repo.orders[fork1ID]
	f1.ClosedAt = &closedAt1
	f1.CloseReason = &slReason
	f1.RealizedPnL = &forkPnl1
	repo.orders[fork1ID] = f1
	repo.mu.Unlock()

	repo.mu.Lock()
	b1 := repo.orders[baseline1ID]
	b1.ClosedAt = &closedAt1
	b1.CloseReason = &tpReason
	b1.RealizedPnL = &pnl1
	repo.orders[baseline1ID] = b1
	repo.mu.Unlock()

	// Baseline #2: no fork at all — must be excluded entirely from the comparison.
	pnl2 := dec("100")
	baseline2ID, _ := repo.OpenPaperOrder(ctx, port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Variant: "baseline"})
	repo.mu.Lock()
	b2 := repo.orders[baseline2ID]
	b2.ClosedAt = &closedAt1
	b2.CloseReason = &tpReason
	b2.RealizedPnL = &pnl2
	repo.orders[baseline2ID] = b2
	repo.mu.Unlock()

	stats, err := repo.SLTPAdjustmentStats(ctx, "", time.Time{})
	if err != nil {
		t.Fatalf("SLTPAdjustmentStats: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 variant rows, got %d", len(stats))
	}

	var baseline, adjusted port.VariantStats
	for _, s := range stats {
		if s.Variant == "baseline" {
			baseline = s
		} else {
			adjusted = s
		}
	}

	if baseline.ClosedCount != 1 || baseline.Wins != 1 || !baseline.RealizedPnL.Equal(dec("5")) {
		t.Errorf("expected baseline{closed=1,wins=1,pnl=5} (baseline2 excluded, no fork), got %+v", baseline)
	}
	if adjusted.ClosedCount != 1 || adjusted.Losses != 1 || !adjusted.RealizedPnL.Equal(dec("-2")) {
		t.Errorf("expected rl_adjusted{closed=1,losses=1,pnl=-2}, got %+v", adjusted)
	}
}

func TestListSLTPAdjustmentPairs_ReturnsLinkedPairsOnly(t *testing.T) {
	repo := newFakeRepository()
	ctx := context.Background()

	baselineID, _ := repo.OpenPaperOrder(ctx, port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Variant: "baseline"})
	forkID, _ := repo.ForkPaperOrderWithSLTP(ctx, baselineID, ptr(dec("95")), ptr(dec("110")))
	// An unrelated unpaired baseline order should not show up as a pair.
	_, _ = repo.OpenPaperOrder(ctx, port.PaperOrder{InstID: "BTC-USDT-SWAP", Side: "buy", EntryPx: dec("100"), Variant: "baseline"})

	pairs, err := repo.ListSLTPAdjustmentPairs(ctx, "BTC-USDT-SWAP")
	if err != nil {
		t.Fatalf("ListSLTPAdjustmentPairs: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("expected exactly 1 pair, got %d", len(pairs))
	}
	if pairs[0].BaselineOrder.ID != baselineID || pairs[0].RLAdjustedOrder.ID != forkID {
		t.Errorf("expected pair baseline=%d fork=%d, got baseline=%d fork=%d",
			baselineID, forkID, pairs[0].BaselineOrder.ID, pairs[0].RLAdjustedOrder.ID)
	}
}

func TestEvaluateStrategies_PersistsDecisionTimeObservation(t *testing.T) {
	repo := newFakeRepository()
	alwaysBuy := &stubStrategy{signal: strategy.Signal{Side: strategy.Buy, SLPct: dec("0.01"), TPPct: dec("0.02"), Confidence: dec("0.8")}}
	pt := newTestPaperTrader(repo, []StrategyAssignment{{Bar: "1m", Strategy: alwaysBuy, StrategyID: 7}})
	pt.candles["1m"] = []domain.Candle{
		{Close: dec("98")}, {Close: dec("99")}, {Close: dec("100")},
	}
	pt.ActiveTokens = []string{"BTC-USDT-SWAP", "XAU-USD-SWAP"}
	pt.TokenBudgetUSD = dec("10")

	if err := pt.evaluateStrategies(context.Background(), "1m", dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 1 {
		t.Fatalf("expected 1 opened order, got %d", len(open))
	}
	if len(open[0].FeaturesJSON) == 0 {
		t.Fatalf("expected FeaturesJSON to be populated with the decision-time observation")
	}

	var obs domain.Observation
	if err := json.Unmarshal(open[0].FeaturesJSON, &obs); err != nil {
		t.Fatalf("FeaturesJSON did not unmarshal as domain.Observation: %v", err)
	}
	if obs.SchemaVersion != domain.ObservationSchemaVersion {
		t.Errorf("expected schema version %d, got %d", domain.ObservationSchemaVersion, obs.SchemaVersion)
	}
	if obs.InstID != "BTC-USDT-SWAP" {
		t.Errorf("expected InstID BTC-USDT-SWAP, got %q", obs.InstID)
	}
	if len(obs.Timeframes) != 1 || len(obs.Timeframes[0].StrategySignals) != 1 {
		t.Fatalf("expected 1 timeframe block with 1 strategy signal, got %+v", obs.Timeframes)
	}
	if obs.Timeframes[0].StrategySignals[0].StrategyID != 7 {
		t.Errorf("expected strategy signal StrategyID=7, got %d", obs.Timeframes[0].StrategySignals[0].StrategyID)
	}
}
