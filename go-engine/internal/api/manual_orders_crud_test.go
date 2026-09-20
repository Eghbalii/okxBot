package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

func decPtr(s string) *decimal.Decimal { d := dec(s); return &d }

// stubManualRepo embeds port.Repository (nil) so only the methods a given test actually exercises
// need overriding — matches stubRepo's own "unstubbed call panics" convention.
type stubManualRepo struct {
	port.Repository

	createdIntent     port.ManualOrderIntent
	createIntentErr   error
	createdIntentID   int64
	intent            port.ManualOrderIntent
	getIntentErr      error
	order             port.ManualOrder
	getOrderErr       error
	listOrders        []port.ManualOrder
	closeRequestedID  int64
	closeErr          error
	cancelRequestedID int64
	cancelErr         error
	protectionSet     []struct {
		id                  int64
		algoID              *string
		protectedByStrategy bool
	}
	adjustmentsRecorded []struct {
		id        int64
		field     string
		old, new_ *decimal.Decimal
	}
	listAdjustments []port.ManualOrderAdjustment
}

func (s *stubManualRepo) CreateManualOrderIntent(ctx context.Context, in port.ManualOrderIntent) (int64, error) {
	s.createdIntent = in
	return s.createdIntentID, s.createIntentErr
}

func (s *stubManualRepo) GetManualOrderIntent(ctx context.Context, id int64) (port.ManualOrderIntent, error) {
	return s.intent, s.getIntentErr
}

func (s *stubManualRepo) GetManualOrder(ctx context.Context, id int64) (port.ManualOrder, error) {
	return s.order, s.getOrderErr
}

func (s *stubManualRepo) ListManualOrders(ctx context.Context, f port.PositionFilter) ([]port.ManualOrder, error) {
	return s.listOrders, nil
}

func (s *stubManualRepo) RequestManualOrderClose(ctx context.Context, id int64) error {
	s.closeRequestedID = id
	return s.closeErr
}

func (s *stubManualRepo) CancelManualOrder(ctx context.Context, id int64) error {
	s.cancelRequestedID = id
	return s.cancelErr
}

func (s *stubManualRepo) SetManualOrderProtection(ctx context.Context, id int64, algoOrderID *string, protectedByStrategy bool) error {
	s.protectionSet = append(s.protectionSet, struct {
		id                  int64
		algoID              *string
		protectedByStrategy bool
	}{id, algoOrderID, protectedByStrategy})
	return nil
}

func (s *stubManualRepo) RecordManualOrderAdjustment(ctx context.Context, orderID int64, field string, oldValue, newValue *decimal.Decimal) error {
	s.adjustmentsRecorded = append(s.adjustmentsRecorded, struct {
		id        int64
		field     string
		old, new_ *decimal.Decimal
	}{orderID, field, oldValue, newValue})
	return nil
}

func (s *stubManualRepo) ListManualOrderAdjustments(ctx context.Context, orderID int64) ([]port.ManualOrderAdjustment, error) {
	return s.listAdjustments, nil
}

// stubManualExchange implements manualTradeClient for these tests — only GetInstrument/GetTicker/
// SetLeverage are ever exercised here, matching manual_orders_test.go's own convention of unused
// methods simply returning zero values.
type stubManualExchange struct {
	inst        domain.Instrument
	instErr     error
	ticker      domain.Ticker
	tickerErr   error
	leverageErr error
	leverageSet []domain.LeverageChange

	accountConfig    domain.AccountConfig
	accountConfigErr error
	positions        []domain.Position
	positionsErr     error
	setPosModeErr    error
	setPosModeCalls  []string
}

func (s *stubManualExchange) GetInstrument(instType, instID string) (domain.Instrument, error) {
	return s.inst, s.instErr
}
func (s *stubManualExchange) SetLeverage(req domain.LeverageChange) error {
	s.leverageSet = append(s.leverageSet, req)
	return s.leverageErr
}
func (s *stubManualExchange) PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error) {
	return nil, errors.New("not used in this test")
}
func (s *stubManualExchange) CancelOrder(instID, ordID string) error { return nil }
func (s *stubManualExchange) GetOrder(instID, ordID string) (domain.OrderStatus, error) {
	return domain.OrderStatus{}, nil
}
func (s *stubManualExchange) PlaceAlgoOrder(req domain.AlgoOrderRequest) (string, error) {
	return "", nil
}
func (s *stubManualExchange) CancelAlgoOrder(instID, algoID string) error { return nil }
func (s *stubManualExchange) GetTicker(instID string) (domain.Ticker, error) {
	return s.ticker, s.tickerErr
}
func (s *stubManualExchange) GetAccountConfig() (domain.AccountConfig, error) {
	return s.accountConfig, s.accountConfigErr
}
func (s *stubManualExchange) SetPositionMode(posMode string) error {
	s.setPosModeCalls = append(s.setPosModeCalls, posMode)
	return s.setPosModeErr
}
func (s *stubManualExchange) GetPositions(instType string) ([]domain.Position, error) {
	return s.positions, s.positionsErr
}

func newManualTestServer(repo *stubManualRepo, exchange *stubManualExchange) *Server {
	return &Server{
		Repo:        repo,
		ManualTrade: exchange,
		Protection:  &stubAmender{},
		Logger:      slog.Default(),
	}
}

func TestHandleCreateManualOrder_MarketOrderWithPercentLevels(t *testing.T) {
	repo := &stubManualRepo{createdIntentID: 42}
	exchange := &stubManualExchange{ticker: domain.Ticker{Last: dec("100")}}
	srv := newManualTestServer(repo, exchange)

	body := manualOrderRequest{
		Symbol: "BTC", Side: "buy", SizeUSD: dec("50"), Leverage: dec("10"),
		SLPct: floatPtr(-5), TPPct: floatPtr(10),
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/manual/orders", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.handleCreateManualOrder(rec, req)

	if rec.Code != 202 {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	// -5% margin loss at 10x leverage = 0.5% price move from entry (100), below for a long.
	if repo.createdIntent.SLPx == nil || !repo.createdIntent.SLPx.Equal(dec("99.5")) {
		t.Fatalf("expected slPx=99.5, got %v", repo.createdIntent.SLPx)
	}
	// +10% margin profit at 10x = 1% price move above.
	if repo.createdIntent.TPPx == nil || !repo.createdIntent.TPPx.Equal(dec("101")) {
		t.Fatalf("expected tpPx=101, got %v", repo.createdIntent.TPPx)
	}
	if repo.createdIntent.OrderType != "market" {
		t.Fatalf("expected orderType=market default, got %q", repo.createdIntent.OrderType)
	}
	var resp map[string]int64
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["intentId"] != 42 {
		t.Fatalf("expected intentId=42, got %v", resp)
	}
}

func TestHandleCreateManualOrder_LimitOrderUsesGivenPriceNotTicker(t *testing.T) {
	repo := &stubManualRepo{createdIntentID: 1}
	// A ticker error must not matter for a limit order — its own limit price is the reference,
	// never the live market price.
	exchange := &stubManualExchange{tickerErr: errors.New("should not be called")}
	srv := newManualTestServer(repo, exchange)

	body := manualOrderRequest{
		Symbol: "BTC", Side: "sell", OrderType: "limit", LimitPx: decPtr("200"),
		SizeUSD: dec("50"), Leverage: dec("10"), SLPct: floatPtr(-5),
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/manual/orders", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.handleCreateManualOrder(rec, req)

	if rec.Code != 202 {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	// A short's loss side is UP: -5%/10x = 0.5% above the 200 limit price = 201.
	if repo.createdIntent.SLPx == nil || !repo.createdIntent.SLPx.Equal(dec("201")) {
		t.Fatalf("expected slPx=201, got %v", repo.createdIntent.SLPx)
	}
}

func TestHandleCreateManualOrder_RejectsBothPriceAndPercent(t *testing.T) {
	repo := &stubManualRepo{}
	exchange := &stubManualExchange{ticker: domain.Ticker{Last: dec("100")}}
	srv := newManualTestServer(repo, exchange)

	body := manualOrderRequest{
		Symbol: "BTC", Side: "buy", SizeUSD: dec("50"), Leverage: dec("10"),
		SLPx: decPtr("90"), SLPct: floatPtr(-5),
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/manual/orders", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.handleCreateManualOrder(rec, req)

	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateManualOrder_RejectsLimitWithoutPrice(t *testing.T) {
	repo := &stubManualRepo{}
	exchange := &stubManualExchange{}
	srv := newManualTestServer(repo, exchange)

	body := manualOrderRequest{Symbol: "BTC", Side: "buy", OrderType: "limit", SizeUSD: dec("50"), Leverage: dec("10")}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/manual/orders", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.handleCreateManualOrder(rec, req)

	if rec.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleManualSetLeverage_NetModeNeedsNoSide(t *testing.T) {
	repo := &stubManualRepo{}
	exchange := &stubManualExchange{}
	srv := newManualTestServer(repo, exchange)
	srv.ManualTrading = ManualTradingConfig{TdMode: "isolated", PosMode: "net"}

	body := manualLeverageRequest{Symbol: "BTC", Leverage: dec("10")}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/manual/leverage", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.handleManualSetLeverage(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(exchange.leverageSet) != 1 || exchange.leverageSet[0].PosSide != "" {
		t.Fatalf("expected one leverage call with no PosSide in net mode, got %+v", exchange.leverageSet)
	}
}

func TestHandleManualSetLeverage_HedgeModeRequiresSide(t *testing.T) {
	repo := &stubManualRepo{}
	exchange := &stubManualExchange{}
	srv := newManualTestServer(repo, exchange)
	srv.ManualTrading = ManualTradingConfig{TdMode: "cross", PosMode: "long_short"}

	body := manualLeverageRequest{Symbol: "BTC", Leverage: dec("10")}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/manual/leverage", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.handleManualSetLeverage(rec, req)

	if rec.Code != 400 {
		t.Fatalf("expected 400 without side in hedge mode, got %d", rec.Code)
	}

	body.Side = "sell"
	raw, _ = json.Marshal(body)
	req = httptest.NewRequest("POST", "/api/manual/leverage", bytes.NewReader(raw))
	rec = httptest.NewRecorder()
	srv.handleManualSetLeverage(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(exchange.leverageSet) != 1 || exchange.leverageSet[0].PosSide != "short" {
		t.Fatalf("expected PosSide=short for a sell in hedge mode, got %+v", exchange.leverageSet)
	}
}

func TestHandleGetManualAccountMode_ReportsModeAndSwitchability(t *testing.T) {
	repo := &stubManualRepo{}
	exchange := &stubManualExchange{
		accountConfig: domain.AccountConfig{PosMode: "net_mode"},
		positions:     []domain.Position{{InstID: "BTC", Pos: dec("1")}},
	}
	srv := newManualTestServer(repo, exchange)

	req := httptest.NewRequest("GET", "/api/manual/account-mode", nil)
	rec := httptest.NewRecorder()
	srv.handleGetManualAccountMode(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got manualAccountModeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PosMode != "net" || got.OpenPositionCount != 1 || got.CanSwitchToHedge {
		t.Fatalf("unexpected response: %+v", got)
	}
}

func TestHandleSetManualAccountMode_RefusesHedgeSwitchWithOpenPositions(t *testing.T) {
	repo := &stubManualRepo{}
	exchange := &stubManualExchange{
		positions: []domain.Position{{InstID: "BTC", Pos: dec("1")}},
	}
	srv := newManualTestServer(repo, exchange)

	body, _ := json.Marshal(manualSetAccountModeRequest{PosMode: "hedge"})
	req := httptest.NewRequest("POST", "/api/manual/account-mode", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleSetManualAccountMode(rec, req)

	if rec.Code != 409 {
		t.Fatalf("expected 409 with an open position, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(exchange.setPosModeCalls) != 0 {
		t.Fatalf("must not call the exchange when the precondition fails, got %v", exchange.setPosModeCalls)
	}
}

func TestHandleSetManualAccountMode_SwitchesWhenFlat(t *testing.T) {
	repo := &stubManualRepo{}
	exchange := &stubManualExchange{}
	srv := newManualTestServer(repo, exchange)

	body, _ := json.Marshal(manualSetAccountModeRequest{PosMode: "hedge"})
	req := httptest.NewRequest("POST", "/api/manual/account-mode", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleSetManualAccountMode(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(exchange.setPosModeCalls) != 1 || exchange.setPosModeCalls[0] != "long_short_mode" {
		t.Fatalf("expected exactly one SetPositionMode(long_short_mode) call, got %v", exchange.setPosModeCalls)
	}
}

func TestHandleSetManualAccountMode_RejectsUnknownMode(t *testing.T) {
	repo := &stubManualRepo{}
	exchange := &stubManualExchange{}
	srv := newManualTestServer(repo, exchange)

	body, _ := json.Marshal(manualSetAccountModeRequest{PosMode: "dual"})
	req := httptest.NewRequest("POST", "/api/manual/account-mode", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleSetManualAccountMode(rec, req)

	if rec.Code != 400 {
		t.Fatalf("expected 400 for an unrecognized posMode, got %d", rec.Code)
	}
}

func TestHandleAdjustManualOrder_RefusesWhenProtectedByStrategy(t *testing.T) {
	repo := &stubManualRepo{order: port.ManualOrder{
		ID: 1, InstID: "BTC", Side: "buy", EntryPx: decPtr("100"), Leverage: dec("10"),
		ProtectedByStrategy: true,
	}}
	srv := newManualTestServer(repo, &stubManualExchange{})

	body := adjustPositionRequest{SLPct: floatPtr(-5)}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/manual/orders/1/adjust", bytes.NewReader(raw))
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	srv.handleAdjustManualOrder(rec, req)

	if rec.Code != 409 {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdjustManualOrder_AmendsExchangeAndRecordsAdjustment(t *testing.T) {
	repo := &stubManualRepo{order: port.ManualOrder{
		ID: 1, InstID: "BTC", ExecInstID: "BTC-USDT-SWAP", Side: "buy",
		EntryPx: decPtr("100"), Leverage: dec("10"), ExchangeAlgoOrderID: algoID(),
	}}
	srv := newManualTestServer(repo, &stubManualExchange{})

	body := adjustPositionRequest{SLPct: floatPtr(-5)}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/manual/orders/1/adjust", bytes.NewReader(raw))
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	srv.handleAdjustManualOrder(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	amender := srv.Protection.(*stubAmender)
	if len(amender.amends) != 1 {
		t.Fatalf("expected 1 amend call, got %d", len(amender.amends))
	}
	if len(repo.adjustmentsRecorded) != 1 || repo.adjustmentsRecorded[0].field != "sl" {
		t.Fatalf("expected 1 sl adjustment recorded, got %+v", repo.adjustmentsRecorded)
	}
}

func TestHandleCloseManualOrder_RequestsClose(t *testing.T) {
	repo := &stubManualRepo{}
	srv := newManualTestServer(repo, &stubManualExchange{})
	req := httptest.NewRequest("POST", "/api/manual/orders/7/close", nil)
	req.SetPathValue("id", "7")
	rec := httptest.NewRecorder()
	srv.handleCloseManualOrder(rec, req)

	if rec.Code != 202 {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if repo.closeRequestedID != 7 {
		t.Fatalf("expected close requested for id=7, got %d", repo.closeRequestedID)
	}
}

func TestHandleCancelManualOrder_RequestsCancel(t *testing.T) {
	repo := &stubManualRepo{}
	srv := newManualTestServer(repo, &stubManualExchange{})
	req := httptest.NewRequest("POST", "/api/manual/orders/9/cancel", nil)
	req.SetPathValue("id", "9")
	rec := httptest.NewRecorder()
	srv.handleCancelManualOrder(rec, req)

	if rec.Code != 202 {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if repo.cancelRequestedID != 9 {
		t.Fatalf("expected cancel requested for id=9, got %d", repo.cancelRequestedID)
	}
}

func TestHandleManualInstrumentLookup_NotConfiguredWithoutManualTrade(t *testing.T) {
	srv := &Server{Repo: &stubManualRepo{}, Logger: slog.Default()}
	req := httptest.NewRequest("GET", "/api/manual/instruments?symbol=BTC", nil)
	rec := httptest.NewRecorder()
	srv.handleManualInstrumentLookup(rec, req)
	if rec.Code != 503 {
		t.Fatalf("expected 503 without ManualTrade configured, got %d", rec.Code)
	}
}
