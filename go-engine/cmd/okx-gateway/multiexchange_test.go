package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/gateway"
	mexcrest "github.com/eghbalii/okxBot/go-engine/internal/mexc/rest"
	"github.com/shopspring/decimal"
)

// This file proves the claim that justified generalizing this service rather than forking it into a
// second binary: the gateway's routes, limiter, retry and metrics are exchange-agnostic, and only
// the client behind it and its retry predicate differ.
//
// It matters because the alternative reading is plausible — the package is literally named
// okx-gateway, its struct field is cfg.OKX, and its default predicate matches OKX codes. Asserting
// the generality rather than claiming it is what turns "should work" into "does".

// mexcShapedClient stands in for a real MEXC client. It is NOT a hand-rolled fake of the port:
// mexcrest.Client already satisfies port.ExchangeClient (proved in internal/mexc/adapter.go), and
// this only replaces the network so the test needs no credentials or connectivity.
type mexcShapedClient struct {
	exchangeClient // embedded so unimplemented methods panic loudly if a test reaches them
	ticker         domain.Ticker
	err            error
	calls          int
}

func (m *mexcShapedClient) GetAllTickers(string) ([]domain.MarketTicker, error) {
	m.calls++
	return nil, m.err
}

func (m *mexcShapedClient) GetTicker(instID string) (domain.Ticker, error) {
	m.calls++
	if m.err != nil {
		return domain.Ticker{}, m.err
	}
	t := m.ticker
	t.InstID = instID
	return t, nil
}

// TestGateway_ServesANonOKXClient runs the REAL service — real routes, real limiter, real metrics
// path — with a MEXC-shaped client behind it, and asserts a request is served end to end.
func TestGateway_ServesANonOKXClient(t *testing.T) {
	client := &mexcShapedClient{ticker: domain.Ticker{Last: decimal.NewFromFloat(77135.2)}}
	svc := &service{
		logger:      slog.New(slog.NewTextHandler(discard{}, nil)),
		client:      client,
		limiter:     gateway.NewLimiter(gateway.DefaultLimits()),
		retry:       gateway.RetryPolicy{MaxAttempts: 1},
		isRetryable: mexcrest.IsRetryableError,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ticker?instId=BTC_USDT", nil)
	req.Header.Set("X-Gateway-Consumer", "trader")
	svc.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got domain.Ticker
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// MEXC's symbol format, served through a route that knows nothing about either exchange.
	if got.InstID != "BTC_USDT" {
		t.Errorf("InstID = %q, want BTC_USDT", got.InstID)
	}
	if !got.Last.Equal(decimal.NewFromFloat(77135.2)) {
		t.Errorf("Last = %s, want 77135.2", got.Last)
	}
	if client.calls != 1 {
		t.Errorf("client called %d times, want 1", client.calls)
	}
}

// TestGateway_UsesTheConfiguredRetryPredicate is the part that would silently regress. If the
// service ignored its predicate and kept calling isRetryableOKXError, a MEXC rate-limit (code 510)
// would not be retried — the gateway would look fine and simply give up on exactly the errors
// retrying exists for.
func TestGateway_UsesTheConfiguredRetryPredicate(t *testing.T) {
	rateLimited := &mexcrest.APIError{Code: 510, Message: "rate limited"}
	client := &mexcShapedClient{err: rateLimited}
	svc := &service{
		logger:      slog.New(slog.NewTextHandler(discard{}, nil)),
		client:      client,
		limiter:     gateway.NewLimiter(gateway.DefaultLimits()),
		retry:       gateway.RetryPolicy{MaxAttempts: 3},
		isRetryable: mexcrest.IsRetryableError,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ticker?instId=BTC_USDT", nil)
	svc.routes().ServeHTTP(rec, req)

	// Three attempts means the MEXC predicate was consulted and said "retry". With OKX's predicate
	// (which matches code=50011 in the message text) this error is unrecognised and tried once.
	if client.calls != 3 {
		t.Errorf("client called %d times, want 3 — the configured MEXC predicate was not used", client.calls)
	}
}

// TestGateway_DefaultsToOKXPredicate — existing OKX wiring passes no predicate, and must keep its
// exact previous behaviour rather than silently losing retries.
func TestGateway_DefaultsToOKXPredicate(t *testing.T) {
	svc := &service{}
	if svc.retryablePredicate() == nil {
		t.Fatal("nil predicate must fall back to OKX's, not to nothing")
	}
	okxRateLimit := errors.New("request failed: code=50011 msg=Too Many Requests")
	if !svc.retryablePredicate()(okxRateLimit) {
		t.Error("the fallback predicate does not recognise OKX's rate-limit code — existing " +
			"behaviour regressed")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
