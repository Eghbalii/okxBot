package usecase

import (
	"context"
	"encoding/json"
	"fmt"
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

	// getOrderRawErr, when set, makes the optional raw-record fetch fail — the capture path must
	// degrade to storing nothing rather than affecting the trade.
	getOrderRawErr error
	// ordSeq gives each placed order its own id (see PlaceOrder).
	ordSeq int

	placedOrders   []domain.OrderRequest
	leverageCalls  []domain.LeverageChange
	placeOrderErr  error
	setLeverageErr error

	// orderStatusQueue lets a test script a sequence of GetOrder responses (e.g. "live" then
	// "filled", to exercise a fill-timeout poll loop) — each call pops the next entry; once
	// exhausted, GetOrder falls back to orderStatus (or a default "filled" if that's also unset).
	// getOrderErr, if set, is returned on every call regardless of the queue.
	orderStatusQueue []domain.OrderStatus
	orderStatus      *domain.OrderStatus
	getOrderErr      error
	getOrderCalls    int

	cancelOrderCalls []string // ordIDs passed to CancelOrder
	cancelOrderErr   error

	// instrument, if unset, defaults to CtVal=1/LotSz=1/MinSz=0 — a no-op multiplier matching
	// this codebase's pre-2026-09-04 implicit assumption, so existing tests that never set this
	// field keep computing the exact same order sizes as before GetInstrument existed.
	instrument       *domain.Instrument
	getInstrumentErr error

	// The resting SL/TP (algo) order calls — the exchange-side protection every real position
	// carries since 2026-09-09. Recorded rather than merely counted so a test can assert the
	// TRIGGER PRICES that actually reached the exchange, which is the entire property at stake:
	// a stop stored locally but never pushed is the bug this whole mechanism exists to remove.
	placedAlgoOrders  []domain.AlgoOrderRequest
	amendedAlgoOrders []domain.AlgoOrderAmend
	canceledAlgoIDs   []string
	placeAlgoErr      error
	amendAlgoErr      error
	cancelAlgoErr     error
	// algoSeq gives each placed algo order its own id, so a test can tell one from another.
	algoSeq int
	// algoStatus, when set, is what GetAlgoOrder reports; unset means a live order carrying the
	// most recently placed triggers.
	algoStatus      *domain.AlgoOrderStatus
	algoStatusQueue []domain.AlgoOrderStatus
	getAlgoErr      error
	getAlgoCalls    int
}

func (f *fakeExchangeClient) PlaceAlgoOrder(req domain.AlgoOrderRequest) (string, error) {
	if f.placeAlgoErr != nil {
		return "", f.placeAlgoErr
	}
	f.placedAlgoOrders = append(f.placedAlgoOrders, req)
	f.algoSeq++
	return fmt.Sprintf("algo-%d", f.algoSeq), nil
}

func (f *fakeExchangeClient) AmendAlgoOrder(req domain.AlgoOrderAmend) error {
	if f.amendAlgoErr != nil {
		return f.amendAlgoErr
	}
	f.amendedAlgoOrders = append(f.amendedAlgoOrders, req)
	return nil
}

func (f *fakeExchangeClient) CancelAlgoOrder(instID, algoID string) error {
	if f.cancelAlgoErr != nil {
		return f.cancelAlgoErr
	}
	f.canceledAlgoIDs = append(f.canceledAlgoIDs, algoID)
	return nil
}

func (f *fakeExchangeClient) GetAlgoOrder(instID, algoID string) (domain.AlgoOrderStatus, error) {
	f.getAlgoCalls++
	// algoStatusQueue lets a test script a SEQUENCE of responses — needed to reproduce OKX
	// populating actualSide a moment after the trigger, which no single fixed response can model.
	if len(f.algoStatusQueue) > 0 {
		next := f.algoStatusQueue[0]
		f.algoStatusQueue = f.algoStatusQueue[1:]
		return next, nil
	}
	if f.getAlgoErr != nil {
		return domain.AlgoOrderStatus{}, f.getAlgoErr
	}
	if f.algoStatus != nil {
		return *f.algoStatus, nil
	}
	status := domain.AlgoOrderStatus{AlgoID: algoID, InstID: instID, State: "live"}
	if n := len(f.placedAlgoOrders); n > 0 {
		status.SLTriggerPx = f.placedAlgoOrders[n-1].SLTriggerPx
		status.TPTriggerPx = f.placedAlgoOrders[n-1].TPTriggerPx
	}
	return status, nil
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
	// A distinct id per order, as the real exchange returns — a constant would make an open and
	// its own flatten indistinguishable, which hides any bug that confuses the two legs.
	f.ordSeq++
	return &domain.OrderResult{SCode: "0", OrdID: fmt.Sprintf("fake-ord-id-%d", f.ordSeq)}, nil
}
func (f *fakeExchangeClient) SetLeverage(req domain.LeverageChange) error {
	f.leverageCalls = append(f.leverageCalls, req)
	return f.setLeverageErr
}
func (f *fakeExchangeClient) CancelOrder(instID, ordID string) error {
	f.cancelOrderCalls = append(f.cancelOrderCalls, ordID)
	return f.cancelOrderErr
}

// GetOrderRaw makes the fake satisfy the optional rawOrderFetcher capability, so tests can
// exercise the exchange-record capture path. Returns a payload identifiably keyed by the ids it
// was asked for, so a test can prove the RIGHT leg's record was stored.
func (f *fakeExchangeClient) GetOrderRaw(instID, ordID string) (json.RawMessage, error) {
	if f.getOrderRawErr != nil {
		return nil, f.getOrderRawErr
	}
	return json.RawMessage(`{"instId":"` + instID + `","ordId":"` + ordID + `","captured":true}`), nil
}

func (f *fakeExchangeClient) GetOrder(instID, ordID string) (domain.OrderStatus, error) {
	f.getOrderCalls++
	if f.getOrderErr != nil {
		return domain.OrderStatus{}, f.getOrderErr
	}
	if len(f.orderStatusQueue) > 0 {
		next := f.orderStatusQueue[0]
		f.orderStatusQueue = f.orderStatusQueue[1:]
		return next, nil
	}
	if f.orderStatus != nil {
		return *f.orderStatus, nil
	}
	// A default fill reports a real filled SIZE, not just the state. A filled order with
	// AccFillSz=0 does not exist on the exchange, and a fake that models one silently weakens every
	// test that depends on fill-derived values — order.Contracts (what a flatten closes and what
	// the exchange-side protective order is sized to) is derived from exactly this field, so the
	// old zero made every test position unprotectable in a way production never is.
	sz := decimal.NewFromInt(1)
	if n := len(f.placedOrders); n > 0 && f.placedOrders[n-1].Sz.IsPositive() {
		sz = f.placedOrders[n-1].Sz
	}
	return domain.OrderStatus{InstID: instID, OrdID: ordID, State: "filled", AccFillSz: sz, Sz: sz}, nil
}
func (f *fakeExchangeClient) GetInstrument(instType, instID string) (domain.Instrument, error) {
	if f.getInstrumentErr != nil {
		return domain.Instrument{}, f.getInstrumentErr
	}
	if f.instrument != nil {
		return *f.instrument, nil
	}
	// CtVal=1, LotSz=0 (no lot-size rounding) is the TRUE no-op default: sizeToContracts skips
	// LotSz rounding entirely when LotSz is not positive, so notional/price passes through exactly
	// as it did before GetInstrument existed — a LotSz=1 default would silently floor any
	// fractional-contract test scenario to zero, which is real X-Perp behavior but not what most
	// existing tests here are set up to exercise.
	return domain.Instrument{InstID: instID, CtVal: decimal.NewFromInt(1)}, nil
}

func (f *fakeExchangeClient) GetFundingRateHistory(instID string, limit int) ([]domain.FundingRate, error) {
	return nil, nil
}

// fakeModelClient is a hand-rolled port.ModelClient returning a configured Action.
type fakeModelClient struct {
	action  domain.Action
	lastObs domain.Observation // captured so tests can assert what the model was actually asked
	// predictCalls counts how many decisions were actually asked for. Some paths are worth
	// asserting were never REACHED, not merely that they produced no effect — an unusable answer
	// and a question never asked look identical downstream but are not the same behavior.
	predictCalls int
}

func (f *fakeModelClient) Predict(ctx context.Context, obs domain.Observation) (*domain.Action, error) {
	f.predictCalls++
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
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("1"), LeverageFrac: dec("0.5")}}

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
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("1")}}

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
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("1")}}

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
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.001"), LeverageFrac: dec("0")}}

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
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, Side: "sell", SizePct: dec("0.5"), LeverageFrac: dec("0"),
	}}

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
		t.Errorf("expected posSide=short when the action asks to sell, got %q", exchange.placedOrders[0].PosSide)
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
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("1"), LeverageFrac: dec("0")}}

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
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0"), LeverageFrac: dec("0")}}
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
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0"), LeverageFrac: dec("0")}}
	trader := newTestTrader(exchange, model, risk.NewManager(testLimits(), dec("5000")))

	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step: %v", err)
	}

	if !model.lastObs.AccountInitialUSD.Equal(dec("5000")) {
		t.Errorf("expected the live-equity fallback (5000), got %s", model.lastObs.AccountInitialUSD)
	}
}

// TestStep_MarginModeMismatchHaltsTrading covers CLAUDE.md §27.2's read-back verification: a
// configured TdMode that OKX's own reported position.MgnMode disagrees with must halt trading, not
// be silently traded through.
func TestStep_MarginModeMismatchHaltsTrading(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker: domain.Ticker{Last: dec("50000")},
		// TdMode is "cross" (newTestTrader's default) but OKX reports isolated on the open position.
		positions: []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("0.1"), Lever: dec("5"), MgnMode: "isolated"}},
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5")}}
	riskManager := risk.NewManager(testLimits(), dec("1000"))
	trader := newTestTrader(exchange, model, riskManager)

	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step returned error: %v", err)
	}

	if halted, reason := riskManager.Halted(); !halted {
		t.Fatal("expected risk manager halted after a margin mode mismatch")
	} else if reason == "" {
		t.Error("expected a non-empty halt reason")
	}
	if len(exchange.placedOrders) != 0 || len(exchange.leverageCalls) != 0 {
		t.Error("expected no orders/leverage calls on the step that detects the mismatch")
	}

	// A subsequent step must also be a no-op — the halt must stick, not just skip one step.
	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("second step returned error: %v", err)
	}
	if len(exchange.placedOrders) != 0 {
		t.Error("expected the halt to persist across steps")
	}
}

// TestStep_MarginModeMatchDoesNotHalt confirms the check is not a false positive on the normal
// case (configured mode matches what OKX reports).
func TestStep_MarginModeMatchDoesNotHalt(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker:    domain.Ticker{Last: dec("50000")},
		positions: []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("0.1"), Lever: dec("5"), MgnMode: "cross"}},
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0"), LeverageFrac: dec("0.5")}}
	riskManager := risk.NewManager(testLimits(), dec("1000"))
	trader := newTestTrader(exchange, model, riskManager)

	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step returned error: %v", err)
	}
	if halted, reason := riskManager.Halted(); halted {
		t.Fatalf("expected no halt when margin mode matches, got halted with reason %q", reason)
	}
}

// TestStep_FlatPositionSkipsMarginModeCheck confirms a flat (no open position) response never
// trips the check — MgnMode on an empty position carries no information to compare against, and
// treating it as a mismatch would halt trading before any position even exists.
func TestStep_FlatPositionSkipsMarginModeCheck(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker:    domain.Ticker{Last: dec("50000")},
		positions: []domain.Position{{InstID: "BTC-USDT-SWAP", Pos: dec("0"), Lever: dec("0"), MgnMode: ""}},
		balances:  []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5")}}
	riskManager := risk.NewManager(testLimits(), dec("1000"))
	trader := newTestTrader(exchange, model, riskManager)

	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step returned error: %v", err)
	}
	if halted, reason := riskManager.Halted(); halted {
		t.Fatalf("expected no halt on a flat position, got halted with reason %q", reason)
	}
}

// TestStep_LiquidationBufferCrossCheckTightensEstimate covers §27.2's cross-check: when OKX's own
// reported LiqPx/MarkPx implies a narrower buffer than the rough 100/leverage estimate, Approve
// must see the tighter (real) number — an oversized position that the rough estimate alone would
// have approved must now be rejected.
func TestStep_LiquidationBufferCrossCheckTightensEstimate(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker: domain.Ticker{Last: dec("50000")},
		// leverageFrac=1, MaxLeverage=5 => targetLeverage=5 => rough estimate = 100/5 = 20%, which
		// alone would clear MinLiquidationBufferPct=15. But OKX reports MarkPx=50000, LiqPx=48000:
		// real buffer = (50000-48000)/50000*100 = 4%, well below the 15% floor.
		positions: []domain.Position{{
			InstID: "BTC-USDT-SWAP", Pos: dec("0.1"), Lever: dec("5"), MgnMode: "cross",
			MarkPx: dec("50000"), LiqPx: dec("48000"),
		}},
		balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("1")}}
	limits := risk.Limits{
		MaxLeverage: dec("5"), MaxPositionNotionalUSD: dec("1000"),
		MaxDailyDrawdownPct: dec("50"), MinLiquidationBufferPct: dec("15"),
	}
	riskManager := risk.NewManager(limits, dec("1000"))
	trader := newTestTrader(exchange, model, riskManager)

	if err := trader.step(context.Background(), testLogger()); err != nil {
		t.Fatalf("step returned error: %v", err)
	}

	if len(exchange.leverageCalls) != 0 {
		t.Error("expected the real (tighter) liquidation buffer to reject the action, but SetLeverage was called")
	}
	if len(exchange.placedOrders) != 0 {
		t.Error("expected no order placed when the real liquidation buffer rejects the action")
	}
}

// TestStep_LiquidationBufferCrossCheckNeverLoosens confirms the cross-check only ever tightens —
// a real buffer WIDER than the rough estimate must not be used to approve an action the rough
// estimate alone would have rejected.
func TestStep_LiquidationBufferCrossCheckNeverLoosens(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker: domain.Ticker{Last: dec("50000")},
		// Rough estimate at targetLeverage=20 (MaxLeverage=20, leverageFrac=1) = 100/20 = 5%, below
		// MinLiquidationBufferPct=15 and would be rejected on the rough estimate alone. OKX reports
		// a real buffer of 30% here (MarkPx=50000, LiqPx=35000) — wider than the estimate. The
		// mismatch itself is unrealistic in practice, but the point is this must NOT flip the
		// action to approved; the tighter number always wins.
		positions: []domain.Position{{
			InstID: "BTC-USDT-SWAP", Pos: dec("0.1"), Lever: dec("20"), MgnMode: "cross",
			MarkPx: dec("50000"), LiqPx: dec("35000"),
		}},
		balances: []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}},
	}
	model := &fakeModelClient{action: domain.Action{Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("1")}}
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
		t.Error("expected the rough estimate's rejection to hold even though the real buffer is wider")
	}
}
