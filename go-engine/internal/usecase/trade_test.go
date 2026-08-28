package usecase

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/risk"
)

// fakeExchangeClient is a hand-rolled port.ExchangeClient that records PlaceOrder/SetLeverage
// calls and returns configured responses, so trading logic can be tested without a live OKX
// connection.
type fakeExchangeClient struct {
	ticker    domain.Ticker
	positions []domain.Position
	balances  []domain.Balance

	placedOrders   []domain.OrderRequest
	leverageCalls  []domain.LeverageChange
	placeOrderErr  error
	setLeverageErr error
}

func (f *fakeExchangeClient) GetTicker(instID string) (domain.Ticker, error) {
	return f.ticker, nil
}
func (f *fakeExchangeClient) GetPositions(instType string) ([]domain.Position, error) {
	return f.positions, nil
}
func (f *fakeExchangeClient) GetBalance(ccy string) ([]domain.Balance, error) {
	return f.balances, nil
}
func (f *fakeExchangeClient) GetCandles(instID, bar string, limit int) ([]domain.Candle, error) {
	return nil, nil
}
func (f *fakeExchangeClient) PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error) {
	f.placedOrders = append(f.placedOrders, req)
	if f.placeOrderErr != nil {
		return nil, f.placeOrderErr
	}
	return &domain.OrderResult{SCode: "0"}, nil
}
func (f *fakeExchangeClient) SetLeverage(req domain.LeverageChange) error {
	f.leverageCalls = append(f.leverageCalls, req)
	return f.setLeverageErr
}

// fakeModelClient is a hand-rolled port.ModelClient returning a configured Action.
type fakeModelClient struct {
	action  domain.Action
	lastObs domain.Observation // captured so tests can assert what the model was actually asked
}

func (f *fakeModelClient) Predict(ctx context.Context, obs domain.Observation) (*domain.Action, error) {
	f.lastObs = obs
	a := f.action
	return &a, nil
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func newTestTrader(exchange *fakeExchangeClient, model *fakeModelClient, riskManager *risk.Manager) *Trader {
	return &Trader{
		InstID:      "BTC-USDT-SWAP",
		Exchange:    exchange,
		Model:       model,
		RiskManager: riskManager,
		TdMode:      "cross",
		PosMode:     "net",
		MinOrderUSD: dec("10"),
	}
}

func TestStep_HaltedRiskManagerSkipsExecution(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker:   domain.Ticker{Last: dec("50000")},
		balances: []domain.Balance{{Ccy: "USDT", Eq: dec("500")}}, // large drawdown from dayStart
	}
	model := &fakeModelClient{action: domain.Action{TargetExposure: dec("1"), LeverageFrac: dec("0.5")}}

	limits := risk.Limits{
		MaxLeverage: dec("5"), MaxPositionNotionalUSD: dec("1000"),
		MaxDailyDrawdownPct: dec("5"), MinLiquidationBufferPct: dec("1"),
	}
	riskManager := risk.NewManager(limits, dec("1000")) // dayStart 1000, current 500 => 50% drawdown, halts

	trader := newTestTrader(exchange, model, riskManager)
	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step returned error: %v", err)
	}

	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no orders placed while halted, got %d", len(exchange.placedOrders))
	}
	if len(exchange.leverageCalls) != 0 {
		t.Errorf("expected no leverage changes while halted, got %d", len(exchange.leverageCalls))
	}
}

func TestStep_LeverageClampedToMax(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker:    domain.Ticker{Last: dec("50000")},
		positions: []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("0"), Lever: dec("1")}},
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	// leverageFrac=1.0 => targetLeverage = 1 + 1*(maxLeverage-1) = maxLeverage exactly; use a
	// frac that would overshoot if not clamped by testing against MaxLeverage directly instead.
	model := &fakeModelClient{action: domain.Action{TargetExposure: dec("0.5"), LeverageFrac: dec("1")}}

	limits := risk.Limits{
		MaxLeverage: dec("5"), MaxPositionNotionalUSD: dec("1000"),
		MaxDailyDrawdownPct: dec("50"), MinLiquidationBufferPct: dec("1"),
	}
	riskManager := risk.NewManager(limits, dec("1000"))

	trader := newTestTrader(exchange, model, riskManager)
	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step returned error: %v", err)
	}

	if len(exchange.leverageCalls) != 1 {
		t.Fatalf("expected exactly 1 leverage call, got %d", len(exchange.leverageCalls))
	}
	if !exchange.leverageCalls[0].Lever.Equal(limits.MaxLeverage) {
		t.Errorf("expected leverage clamped to %s, got %s", limits.MaxLeverage, exchange.leverageCalls[0].Lever)
	}
}

func TestStep_RejectedWhenLiquidationBufferTooThin(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker:    domain.Ticker{Last: dec("50000")},
		positions: []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("0"), Lever: dec("1")}},
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	// leverageFrac=1 with MaxLeverage=20 => targetLeverage=20 => liqBufferPct=100/20=5%,
	// below MinLiquidationBufferPct=15 => Approve must reject.
	model := &fakeModelClient{action: domain.Action{TargetExposure: dec("0.5"), LeverageFrac: dec("1")}}

	limits := risk.Limits{
		MaxLeverage: dec("20"), MaxPositionNotionalUSD: dec("1000"),
		MaxDailyDrawdownPct: dec("50"), MinLiquidationBufferPct: dec("15"),
	}
	riskManager := risk.NewManager(limits, dec("1000"))

	trader := newTestTrader(exchange, model, riskManager)
	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step returned error: %v", err)
	}

	if len(exchange.leverageCalls) != 0 {
		t.Errorf("expected leverage change rejected, but SetLeverage was called %d times", len(exchange.leverageCalls))
	}
	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected no order placed when action rejected, got %d", len(exchange.placedOrders))
	}
}

func TestStep_OrderBelowMinSizeSkipped(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker:    domain.Ticker{Last: dec("50000")},
		positions: []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("0"), Lever: dec("1")}},
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	// Tiny target exposure => tiny target notional => delta below MinOrderUSD (10).
	model := &fakeModelClient{action: domain.Action{TargetExposure: dec("0.001"), LeverageFrac: dec("0")}}

	limits := risk.Limits{
		MaxLeverage: dec("5"), MaxPositionNotionalUSD: dec("1000"),
		MaxDailyDrawdownPct: dec("50"), MinLiquidationBufferPct: dec("1"),
	}
	riskManager := risk.NewManager(limits, dec("1000"))

	trader := newTestTrader(exchange, model, riskManager)
	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step returned error: %v", err)
	}

	if len(exchange.placedOrders) != 0 {
		t.Errorf("expected order below MinOrderUSD to be skipped, got %d orders placed", len(exchange.placedOrders))
	}
}

func TestStep_ShortTargetSetsHedgePosSide(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker:    domain.Ticker{Last: dec("50000")},
		positions: []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("0"), Lever: dec("1")}},
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	model := &fakeModelClient{action: domain.Action{TargetExposure: dec("-0.5"), LeverageFrac: dec("0")}}

	limits := risk.Limits{
		MaxLeverage: dec("5"), MaxPositionNotionalUSD: dec("1000"),
		MaxDailyDrawdownPct: dec("50"), MinLiquidationBufferPct: dec("1"),
	}
	riskManager := risk.NewManager(limits, dec("1000"))

	trader := newTestTrader(exchange, model, riskManager)
	trader.PosMode = "long_short"
	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step returned error: %v", err)
	}

	if len(exchange.placedOrders) != 1 {
		t.Fatalf("expected exactly 1 order placed, got %d", len(exchange.placedOrders))
	}
	if exchange.placedOrders[0].PosSide != "short" {
		t.Errorf("expected posSide=short for negative target exposure, got %q", exchange.placedOrders[0].PosSide)
	}
	if exchange.placedOrders[0].Side != "sell" {
		t.Errorf("expected side=sell for a new short from flat, got %q", exchange.placedOrders[0].Side)
	}
}

func TestStep_ExactNotionalNoFloatDrift(t *testing.T) {
	// Regression test for the float64->decimal.Decimal migration: entry/target notional math
	// must be exact, not off by a float-epsilon.
	exchange := &fakeExchangeClient{
		ticker:    domain.Ticker{Last: dec("9.0")},
		positions: []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("0"), Lever: dec("1")}},
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	model := &fakeModelClient{action: domain.Action{TargetExposure: dec("1"), LeverageFrac: dec("0")}}

	limits := risk.Limits{
		MaxLeverage: dec("5"), MaxPositionNotionalUSD: dec("900"),
		MaxDailyDrawdownPct: dec("50"), MinLiquidationBufferPct: dec("1"),
	}
	riskManager := risk.NewManager(limits, dec("1000"))

	trader := newTestTrader(exchange, model, riskManager)
	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step returned error: %v", err)
	}

	if len(exchange.placedOrders) != 1 {
		t.Fatalf("expected exactly 1 order placed, got %d", len(exchange.placedOrders))
	}
	// targetNotional = 1 * 900 = 900; sz = 900 / 9.0 = 100 exactly.
	wantSz := dec("100")
	if !exchange.placedOrders[0].Sz.Equal(wantSz) {
		t.Errorf("expected sz=%s exactly (no float drift), got %s", wantSz, exchange.placedOrders[0].Sz)
	}
}

func testLimits() risk.Limits {
	return risk.Limits{
		MaxLeverage: dec("5"), MaxPositionNotionalUSD: dec("1000"),
		MaxDailyDrawdownPct: dec("5"), MinLiquidationBufferPct: dec("1"),
	}
}

// TestStep_ObservationCarriesTokenIdentityAndAccountEquity covers the live/demo path's train-serve
// consistency (CLAUDE.md §15.1/§15.6): the global model conditions on a token-identity one-hot
// built from ActiveTokens and was trained on per-token equity, so cmd/trader must send the same
// roster and this token's own sub-budget — not an empty roster and total account equity, which is a
// distribution the model never saw in training.
func TestStep_ObservationCarriesTokenIdentityAndAccountEquity(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker:   domain.Ticker{Last: dec("50000")},
		balances: []domain.Balance{{Ccy: "USDT", Eq: dec("5000")}},
	}
	model := &fakeModelClient{action: domain.Action{TargetExposure: dec("0"), LeverageFrac: dec("0")}}
	trader := newTestTrader(exchange, model, risk.NewManager(testLimits(), dec("5000")))
	trader.ActiveTokens = []string{"BTC-USDT-SWAP", "XAU-USD-SWAP"}
	trader.AccountInitialUSD = dec("100")

	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step: %v", err)
	}

	if len(model.lastObs.ActiveTokens) != 2 {
		t.Errorf("expected the 2-token roster for the identity one-hot, got %v", model.lastObs.ActiveTokens)
	}
	// Demo/real equity is the exchange's reported balance (ground truth), reported alongside the
	// configured starting balance so drawdown is visible the same way it is in paper mode.
	if !model.lastObs.AccountEquityUSD.Equal(dec("5000")) {
		t.Errorf("expected the exchange's reported equity (5000), got %s", model.lastObs.AccountEquityUSD)
	}
	if !model.lastObs.AccountInitialUSD.Equal(dec("100")) {
		t.Errorf("expected the configured starting balance (100), got %s", model.lastObs.AccountInitialUSD)
	}
}

// With no configured starting balance, the exchange's current equity stands in for it — the
// documented fallback, so drawdown reads as zero rather than as a nonsense ratio against zero.
func TestStep_ObservationFallsBackToLiveEquityWithoutConfiguredInitial(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker:   domain.Ticker{Last: dec("50000")},
		balances: []domain.Balance{{Ccy: "USDT", Eq: dec("5000")}},
	}
	model := &fakeModelClient{action: domain.Action{TargetExposure: dec("0"), LeverageFrac: dec("0")}}
	trader := newTestTrader(exchange, model, risk.NewManager(testLimits(), dec("5000")))

	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step: %v", err)
	}

	if !model.lastObs.AccountInitialUSD.Equal(dec("5000")) {
		t.Errorf("expected the live-equity fallback (5000), got %s", model.lastObs.AccountInitialUSD)
	}
}
