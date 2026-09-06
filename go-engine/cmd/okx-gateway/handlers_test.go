package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/gateway"
)

// fakeExchange is a hand-rolled exchangeClient recording calls, matching this repo's existing
// fake-over-mock-library convention (e.g. internal/usecase/trade_test.go).
type fakeExchange struct {
	ticker      domain.Ticker
	positions   []domain.Position
	balances    []domain.Balance
	candles     []domain.Candle
	orderResult *domain.OrderResult
	orderStatus domain.OrderStatus
	instrument  domain.Instrument
	fundingRates []domain.FundingRate

	placeOrderCalls    int
	cancelOrderCalls   int
	getOrderCalls      int
	getInstrumentCalls int
	setLeverageCalls   int

	errOnCall int // if > 0, the Nth call across ALL methods fails once, then succeeds — for retry tests
	callCount int
	failErr   error
}

func (f *fakeExchange) maybeFail() error {
	f.callCount++
	if f.errOnCall > 0 && f.callCount == f.errOnCall {
		return f.failErr
	}
	return nil
}

func (f *fakeExchange) GetTicker(instID string) (domain.Ticker, error) {
	if err := f.maybeFail(); err != nil {
		return domain.Ticker{}, err
	}
	return f.ticker, nil
}
func (f *fakeExchange) GetPositions(instType string) ([]domain.Position, error) {
	if err := f.maybeFail(); err != nil {
		return nil, err
	}
	return f.positions, nil
}
func (f *fakeExchange) GetBalance(ccy string) ([]domain.Balance, error) {
	if err := f.maybeFail(); err != nil {
		return nil, err
	}
	return f.balances, nil
}
func (f *fakeExchange) GetCandles(instID, bar string, limit int) ([]domain.Candle, error) {
	if err := f.maybeFail(); err != nil {
		return nil, err
	}
	return f.candles, nil
}
func (f *fakeExchange) PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error) {
	f.placeOrderCalls++
	if err := f.maybeFail(); err != nil {
		return nil, err
	}
	return f.orderResult, nil
}
func (f *fakeExchange) SetLeverage(req domain.LeverageChange) error {
	f.setLeverageCalls++
	return f.maybeFail()
}
func (f *fakeExchange) CancelOrder(instID, ordID string) error {
	f.cancelOrderCalls++
	return f.maybeFail()
}
func (f *fakeExchange) GetOrder(instID, ordID string) (domain.OrderStatus, error) {
	f.getOrderCalls++
	if err := f.maybeFail(); err != nil {
		return domain.OrderStatus{}, err
	}
	return f.orderStatus, nil
}
func (f *fakeExchange) GetInstrument(instType, instID string) (domain.Instrument, error) {
	f.getInstrumentCalls++
	if err := f.maybeFail(); err != nil {
		return domain.Instrument{}, err
	}
	return f.instrument, nil
}

func (f *fakeExchange) GetFundingRateHistory(instID string, limit int) ([]domain.FundingRate, error) {
	if err := f.maybeFail(); err != nil {
		return nil, err
	}
	return f.fundingRates, nil
}

func newTestService(fx *fakeExchange) *service {
	return &service{
		logger:  slog.New(slog.NewTextHandler(os.Stdout, nil)),
		client:  fx,
		limiter: gateway.NewLimiter(gateway.DefaultLimits()),
		retry:   gateway.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond},
	}
}

func TestHandleGetTicker_ReturnsExchangeResult(t *testing.T) {
	fx := &fakeExchange{ticker: domain.Ticker{InstID: "BTC-USDT-SWAP", Last: decimal.NewFromInt(50000)}}
	svc := newTestService(fx)

	req := httptest.NewRequest(http.MethodGet, "/ticker?instId=BTC-USDT-SWAP", nil)
	req.Header.Set("X-Gateway-Consumer", "trader")
	rec := httptest.NewRecorder()
	svc.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got domain.Ticker
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.InstID != "BTC-USDT-SWAP" || !got.Last.Equal(decimal.NewFromInt(50000)) {
		t.Fatalf("unexpected ticker: %+v", got)
	}
}

func TestHandleGetTicker_MissingInstIDReturns400(t *testing.T) {
	svc := newTestService(&fakeExchange{})
	req := httptest.NewRequest(http.MethodGet, "/ticker", nil)
	rec := httptest.NewRecorder()
	svc.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandlePlaceOrder_ForwardsRequestAndReturnsResult(t *testing.T) {
	fx := &fakeExchange{orderResult: &domain.OrderResult{OrdID: "12345", SCode: "0"}}
	svc := newTestService(fx)

	body, _ := json.Marshal(domain.OrderRequest{InstID: "BTC-USDT-SWAP", TdMode: "isolated", Side: "buy", OrdType: "market", Sz: decimal.NewFromInt(1)})
	req := httptest.NewRequest(http.MethodPost, "/order", bytes.NewReader(body))
	req.Header.Set("X-Gateway-Consumer", "trader")
	rec := httptest.NewRecorder()
	svc.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if fx.placeOrderCalls != 1 {
		t.Fatalf("expected exactly 1 PlaceOrder call, got %d", fx.placeOrderCalls)
	}
	var got domain.OrderResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.OrdID != "12345" {
		t.Fatalf("unexpected order result: %+v", got)
	}
}

func TestHandleCancelOrder_CallsExchange(t *testing.T) {
	fx := &fakeExchange{}
	svc := newTestService(fx)

	body, _ := json.Marshal(map[string]string{"instId": "BTC-USDT-SWAP", "ordId": "999"})
	req := httptest.NewRequest(http.MethodPost, "/order/cancel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	svc.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if fx.cancelOrderCalls != 1 {
		t.Fatalf("expected exactly 1 CancelOrder call, got %d", fx.cancelOrderCalls)
	}
}

func TestHandleGetOrder_ReturnsStatus(t *testing.T) {
	fx := &fakeExchange{orderStatus: domain.OrderStatus{InstID: "BTC-USDT-SWAP", OrdID: "999", State: "filled"}}
	svc := newTestService(fx)

	req := httptest.NewRequest(http.MethodGet, "/order?instId=BTC-USDT-SWAP&ordId=999", nil)
	rec := httptest.NewRecorder()
	svc.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got domain.OrderStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.State != "filled" {
		t.Fatalf("unexpected order status: %+v", got)
	}
}

func TestHandleGetInstrument_ReturnsInstrument(t *testing.T) {
	fx := &fakeExchange{instrument: domain.Instrument{
		InstID: "BTC-USD_UM_XPERP-310404", CtVal: decimal.RequireFromString("0.0001"),
		LotSz: decimal.NewFromInt(1), MinSz: decimal.NewFromInt(1), CtValCcy: "BTC",
	}}
	svc := newTestService(fx)

	req := httptest.NewRequest(http.MethodGet, "/instrument?instType=FUTURES&instId=BTC-USD_UM_XPERP-310404", nil)
	rec := httptest.NewRecorder()
	svc.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got domain.Instrument
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.CtVal.Equal(decimal.RequireFromString("0.0001")) {
		t.Fatalf("unexpected instrument: %+v", got)
	}
}

func TestHandleGetInstrument_MissingParamsRejected(t *testing.T) {
	fx := &fakeExchange{}
	svc := newTestService(fx)

	req := httptest.NewRequest(http.MethodGet, "/instrument?instType=FUTURES", nil)
	rec := httptest.NewRecorder()
	svc.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if fx.getInstrumentCalls != 0 {
		t.Errorf("expected no exchange call for a rejected request, got %d", fx.getInstrumentCalls)
	}
}

func TestHandleSetLeverage_CallsExchange(t *testing.T) {
	fx := &fakeExchange{}
	svc := newTestService(fx)

	body, _ := json.Marshal(domain.LeverageChange{InstID: "BTC-USDT-SWAP", Lever: decimal.NewFromInt(10), MgnMode: "isolated"})
	req := httptest.NewRequest(http.MethodPost, "/leverage", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	svc.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if fx.setLeverageCalls != 1 {
		t.Fatalf("expected exactly 1 SetLeverage call, got %d", fx.setLeverageCalls)
	}
}

// TestCall_RetriesRateLimitErrorThenSucceeds confirms the gateway's own retry policy (not just
// the exchange call) is actually wired into the request path — a transient OKX rate-limit error
// on the first attempt should not surface to the caller if a retry succeeds.
func TestCall_RetriesRateLimitErrorThenSucceeds(t *testing.T) {
	fx := &fakeExchange{
		ticker:    domain.Ticker{InstID: "BTC-USDT-SWAP"},
		errOnCall: 1,
		failErr:   errors.New("okx api error GET /api/v5/market/ticker: code=50011 msg=rate limit reached"),
	}
	svc := newTestService(fx)

	req := httptest.NewRequest(http.MethodGet, "/ticker?instId=BTC-USDT-SWAP", nil)
	rec := httptest.NewRecorder()
	svc.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after retry, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestCall_DoesNotRetryNonRateLimitError confirms a non-retryable OKX error (e.g. bad params) is
// surfaced immediately rather than retried and delaying the caller for no reason.
func TestCall_DoesNotRetryNonRateLimitError(t *testing.T) {
	fx := &fakeExchange{
		errOnCall: 1,
		failErr:   errors.New("okx api error POST /api/v5/trade/order: code=51008 msg=insufficient balance"),
	}
	svc := newTestService(fx)

	body, _ := json.Marshal(domain.OrderRequest{InstID: "BTC-USDT-SWAP"})
	req := httptest.NewRequest(http.MethodPost, "/order", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	svc.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 (error surfaced, not retried away), got %d", rec.Code)
	}
	if fx.placeOrderCalls != 1 {
		t.Fatalf("expected exactly 1 call (no retry on non-retryable error), got %d", fx.placeOrderCalls)
	}
}

func TestConsumerAndPriority_OnlyLiteralTraderGetsPriority(t *testing.T) {
	cases := []struct {
		header   string
		wantPrio gateway.ConsumerPriority
	}{
		{"trader", gateway.PriorityTrader},
		{"paper-trader", gateway.PriorityNormal},
		{"strategy-tester", gateway.PriorityNormal},
		{"", gateway.PriorityNormal},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/ticker", nil)
		if c.header != "" {
			req.Header.Set("X-Gateway-Consumer", c.header)
		}
		_, prio := consumerAndPriority(req)
		if prio != c.wantPrio {
			t.Errorf("header=%q: got priority %v, want %v", c.header, prio, c.wantPrio)
		}
	}
}

func TestIsRetryableOKXError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("okx api error: code=50011 msg=rate limit reached"), true},
		{errors.New("okx api error: code=50061 msg=sub-account rate limit"), true},
		{errors.New("okx api error: code=51008 msg=insufficient balance"), false},
		{errors.New("request failed: context deadline exceeded"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isRetryableOKXError(c.err); got != c.want {
			t.Errorf("isRetryableOKXError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
