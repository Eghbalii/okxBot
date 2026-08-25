package usecase

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/rez/okxBot/go-engine/internal/domain"
	"github.com/rez/okxBot/go-engine/internal/port"
	"github.com/rez/okxBot/go-engine/internal/strategy"
)

// fakeRepository is an in-memory port.Repository for testing, no real Postgres needed.
type fakeRepository struct {
	nextID  int64
	orders  map[int64]port.PaperOrder
	candles []port.Candle
}

func newFakeRepository() *fakeRepository {
	return &fakeRepository{orders: make(map[int64]port.PaperOrder)}
}

func (r *fakeRepository) SaveCandle(ctx context.Context, c port.Candle) error {
	r.candles = append(r.candles, c)
	return nil
}
func (r *fakeRepository) CreateStrategy(ctx context.Context, s port.StrategyConfig) (int64, error) {
	return 0, nil
}
func (r *fakeRepository) ListStrategies(ctx context.Context, instID string, enabledOnly bool) ([]port.StrategyConfig, error) {
	return nil, nil
}
func (r *fakeRepository) OpenPaperOrder(ctx context.Context, o port.PaperOrder) (int64, error) {
	r.nextID++
	o.ID = r.nextID
	r.orders[o.ID] = o
	return o.ID, nil
}
func (r *fakeRepository) ClosePaperOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal) error {
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
func (r *fakeRepository) ListOpenPaperOrders(ctx context.Context, instID string) ([]port.PaperOrder, error) {
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

func newTestPaperTrader(repo port.Repository, strategies []strategy.Strategy) *PaperTrader {
	return &PaperTrader{
		InstID:         "BTC-USDT-SWAP",
		Bar:            "1m",
		CandleWindow:   100,
		Strategies:     strategies,
		Exchange:       &fakeExchangeForCandles{},
		TickConsumer:   noopConsumer{},
		CandleConsumer: noopConsumer{},
		Repo:           repo,
		NotionalUSD:    dec("100"),
		MaxOpenOrders:  3,
		Logger:         testLogger(),
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
	buyOrder := buildPaperOrder("BTC-USDT-SWAP", dec("100"), buySignal, dec("100"))
	if buyOrder.SLPx == nil || !buyOrder.SLPx.Equal(dec("99")) {
		t.Errorf("expected buy SL=99, got %v", buyOrder.SLPx)
	}
	if buyOrder.TPPx == nil || !buyOrder.TPPx.Equal(dec("102")) {
		t.Errorf("expected buy TP=102, got %v", buyOrder.TPPx)
	}

	sellSignal := strategy.Signal{Side: strategy.Sell, SLPct: dec("0.01"), TPPct: dec("0.02")}
	sellOrder := buildPaperOrder("BTC-USDT-SWAP", dec("100"), sellSignal, dec("100"))
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
	pt := newTestPaperTrader(repo, []strategy.Strategy{alwaysBuy})
	pt.candles = []domain.Candle{{Open: dec("100"), High: dec("101"), Low: dec("99"), Close: dec("100"), Volume: dec("1")}}

	if err := pt.evaluateStrategies(context.Background(), dec("100"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	open, _ := repo.ListOpenPaperOrders(context.Background(), "BTC-USDT-SWAP")
	if len(open) != 3 {
		t.Errorf("expected MaxOpenOrders to gate new signals, still expected 3 open orders, got %d", len(open))
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

func (s *stubStrategy) Name() string { return "stub" }
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

	if err := pt.handleCandle(context.Background(), data, testLogger()); err != nil {
		t.Fatalf("handleCandle returned error: %v", err)
	}

	if len(pt.candles) != 1 {
		t.Fatalf("expected 1 candle in window, got %d", len(pt.candles))
	}
	if !pt.candles[0].Close.Equal(dec("100.5")) {
		t.Errorf("expected close=100.5, got %s", pt.candles[0].Close)
	}
	if len(repo.candles) != 1 {
		t.Errorf("expected finalized candle to be persisted, got %d saved", len(repo.candles))
	}
}
