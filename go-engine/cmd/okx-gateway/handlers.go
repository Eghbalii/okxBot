package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/gateway"
)

// exchangeClient is the narrow slice of rest.Client this service calls — kept as an interface so
// handler tests can substitute a fake without a real OKX connection.
type exchangeClient interface {
	GetTicker(instID string) (domain.Ticker, error)
	GetPositions(instType string) ([]domain.Position, error)
	GetBalance(ccy string) ([]domain.Balance, error)
	GetCandles(instID, bar string, limit int) ([]domain.Candle, error)
	PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error)
	SetLeverage(req domain.LeverageChange) error
	CancelOrder(instID, ordID string) error
	GetOrder(instID, ordID string) (domain.OrderStatus, error)
}

type service struct {
	logger  *slog.Logger
	client  exchangeClient
	limiter *gateway.Limiter
	retry   gateway.RetryPolicy
	// simulated is this gateway's own cfg.OKX.Simulated — the ONE place real vs. demo credentials
	// are known, now that cmd/trader no longer holds OKX credentials itself (CLAUDE.md §27.1's
	// migration). GET /health exposes it so cmd/trader can derive its real-vs-demo mode (and the
	// allow_real_money gate, §15.6/§15.7) from the same source the credentials actually came from,
	// rather than from its own now-absent OKX_SIMULATED_TRADING — keeping the "mode can never
	// disagree with the credentials in use" invariant intact across the process boundary.
	simulated bool
}

func (s *service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ticker", s.handleGetTicker)
	mux.HandleFunc("GET /positions", s.handleGetPositions)
	mux.HandleFunc("GET /balance", s.handleGetBalance)
	mux.HandleFunc("GET /candles", s.handleGetCandles)
	mux.HandleFunc("POST /order", s.handlePlaceOrder)
	mux.HandleFunc("POST /order/cancel", s.handleCancelOrder)
	mux.HandleFunc("GET /order", s.handleGetOrder)
	mux.HandleFunc("POST /leverage", s.handleSetLeverage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /health", s.handleHealth)
	return mux
}

func (s *service) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "simulated": s.simulated})
}

// consumerAndPriority reads the calling service's identity off the request. X-Gateway-Consumer
// defaults to "unknown" (still rate-limited, just under a shared/unnamed bucket) rather than
// rejecting the request outright — a missing header should degrade to "treated as low priority",
// not take down whatever forgot to set it. Only the literal consumer name "trader" is ever
// treated as gateway.PriorityTrader (CLAUDE.md §27.1) — every other consumer, named or not, is
// PriorityNormal, so a consumer cannot grant itself priority by claiming to be the trader unless
// it actually is cmd/trader (identity here is by convention/internal-network trust, not a signed
// token — this gateway is reachable only from the docker-compose internal network, matching
// every other internal service-to-service call in this codebase, e.g. optimizer-service).
func consumerAndPriority(r *http.Request) (string, gateway.ConsumerPriority) {
	consumer := r.Header.Get("X-Gateway-Consumer")
	if consumer == "" {
		consumer = "unknown"
	}
	if consumer == "trader" {
		return consumer, gateway.PriorityTrader
	}
	return consumer, gateway.PriorityNormal
}

// isRetryableOKXError treats OKX's rate-limit error code (50011) and its sub-account aggregate
// cap (50061) as retryable; every other error (bad params, insufficient balance, instrument not
// found, ...) is not retrying-shaped and is returned to the caller immediately.
func isRetryableOKXError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "code=50011") || strings.Contains(msg, "code=50061")
}

func (s *service) call(ctx context.Context, class gateway.EndpointClass, r *http.Request, fn func() error) error {
	consumer, priority := consumerAndPriority(r)
	if err := s.limiter.Acquire(ctx, class, consumer, priority); err != nil {
		return err
	}
	return s.retry.Do(ctx, isRetryableOKXError, fn)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *service) handleGetTicker(w http.ResponseWriter, r *http.Request) {
	instID := r.URL.Query().Get("instId")
	if instID == "" {
		writeError(w, http.StatusBadRequest, errors.New("instId is required"))
		return
	}
	var result domain.Ticker
	err := s.call(r.Context(), gateway.ClassMarket, r, func() error {
		var innerErr error
		result, innerErr = s.client.GetTicker(instID)
		return innerErr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *service) handleGetPositions(w http.ResponseWriter, r *http.Request) {
	instType := r.URL.Query().Get("instType")
	var result []domain.Position
	err := s.call(r.Context(), gateway.ClassAccount, r, func() error {
		var innerErr error
		result, innerErr = s.client.GetPositions(instType)
		return innerErr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *service) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	ccy := r.URL.Query().Get("ccy")
	var result []domain.Balance
	err := s.call(r.Context(), gateway.ClassAccount, r, func() error {
		var innerErr error
		result, innerErr = s.client.GetBalance(ccy)
		return innerErr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *service) handleGetCandles(w http.ResponseWriter, r *http.Request) {
	instID := r.URL.Query().Get("instId")
	bar := r.URL.Query().Get("bar")
	if instID == "" || bar == "" {
		writeError(w, http.StatusBadRequest, errors.New("instId and bar are required"))
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	var result []domain.Candle
	err := s.call(r.Context(), gateway.ClassMarket, r, func() error {
		var innerErr error
		result, innerErr = s.client.GetCandles(instID, bar, limit)
		return innerErr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *service) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req domain.OrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var result *domain.OrderResult
	err := s.call(r.Context(), gateway.ClassTrade, r, func() error {
		var innerErr error
		result, innerErr = s.client.PlaceOrder(req)
		return innerErr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *service) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstID string `json:"instId"`
		OrdID  string `json:"ordId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := s.call(r.Context(), gateway.ClassTrade, r, func() error {
		return s.client.CancelOrder(req.InstID, req.OrdID)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *service) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	instID := r.URL.Query().Get("instId")
	ordID := r.URL.Query().Get("ordId")
	if instID == "" || ordID == "" {
		writeError(w, http.StatusBadRequest, errors.New("instId and ordId are required"))
		return
	}
	var result domain.OrderStatus
	// Order status is trade-critical (fill-timeout decisions depend on it, CLAUDE.md §27.5) —
	// deliberately in ClassAccount, not ClassTrade, since it's a read, not a mutating trade
	// action, and OKX rate-limits reads independently of order placement/cancel/amend.
	err := s.call(r.Context(), gateway.ClassAccount, r, func() error {
		var innerErr error
		result, innerErr = s.client.GetOrder(instID, ordID)
		return innerErr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *service) handleSetLeverage(w http.ResponseWriter, r *http.Request) {
	var req domain.LeverageChange
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := s.call(r.Context(), gateway.ClassLeverage, r, func() error {
		return s.client.SetLeverage(req)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
