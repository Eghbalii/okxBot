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
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
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
	// GetOrderRaw returns OKX's payload untouched, for the panel's exchange-report view — the
	// typed GetOrder above stays the trading loop's interface.
	GetOrderRaw(instID, ordID string) (json.RawMessage, error)
	GetInstrument(instType, instID string) (domain.Instrument, error)
	GetFundingRateHistory(instID string, limit int) ([]domain.FundingRate, error)
	// The resting SL/TP (conditional "algo") order calls — the exchange-side protection every real
	// position now carries (2026-09-09). Routed through the gateway like every other trade action
	// so they share the trader's priority rate-limit budget rather than competing outside it.
	PlaceAlgoOrder(req domain.AlgoOrderRequest) (string, error)
	AmendAlgoOrder(req domain.AlgoOrderAmend) error
	CancelAlgoOrder(instID, algoID string) error
	GetAlgoOrder(instID, algoID string) (domain.AlgoOrderStatus, error)
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
	mux.HandleFunc("GET /order/raw", s.handleGetOrderRaw)
	mux.HandleFunc("GET /instrument", s.handleGetInstrument)
	mux.HandleFunc("GET /funding-rate-history", s.handleGetFundingRateHistory)
	mux.HandleFunc("POST /leverage", s.handleSetLeverage)
	mux.HandleFunc("POST /order/algo", s.handlePlaceAlgoOrder)
	mux.HandleFunc("POST /order/algo/amend", s.handleAmendAlgoOrder)
	mux.HandleFunc("POST /order/algo/cancel", s.handleCancelAlgoOrder)
	mux.HandleFunc("GET /order/algo", s.handleGetAlgoOrder)
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

// call is the one choke point every proxied OKX request passes through, which is why the request
// counter lives here rather than in each handler — a handler added later is counted without anyone
// remembering to instrument it.
func (s *service) call(ctx context.Context, class gateway.EndpointClass, r *http.Request, fn func() error) error {
	consumer, priority := consumerAndPriority(r)
	if err := s.limiter.Acquire(ctx, class, consumer, priority); err != nil {
		// Counted separately from a plain error: being held back by our OWN limiter is a capacity
		// signal, while an OKX error is a request that actually went out and failed. Conflating
		// them would hide exactly the case this counter exists to expose.
		metrics.GatewayRequestsTotal.WithLabelValues(consumer, string(class), "rate_limited").Inc()
		return err
	}
	err := s.retry.Do(ctx, isRetryableOKXError, fn)
	status := "ok"
	if err != nil {
		status = "error"
	}
	metrics.GatewayRequestsTotal.WithLabelValues(consumer, string(class), status).Inc()
	return err
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

// handleGetOrderRaw proxies OKX's order-status payload through untouched, for the panel's
// exchange-report view (2026-09-09). Same rate-limit class as the typed GetOrder — it is the same
// upstream call, so it must draw from the same budget rather than opening a second, unaccounted
// path to the same endpoint.
func (s *service) handleGetOrderRaw(w http.ResponseWriter, r *http.Request) {
	instID := r.URL.Query().Get("instId")
	ordID := r.URL.Query().Get("ordId")
	var raw json.RawMessage
	err := s.call(r.Context(), gateway.ClassAccount, r, func() error {
		var innerErr error
		raw, innerErr = s.client.GetOrderRaw(instID, ordID)
		return innerErr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
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

func (s *service) handleGetFundingRateHistory(w http.ResponseWriter, r *http.Request) {
	instID := r.URL.Query().Get("instId")
	if instID == "" {
		writeError(w, http.StatusBadRequest, errors.New("instId is required"))
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	var result []domain.FundingRate
	// ClassMarket: /public/funding-rate-history is unauthenticated market data, same rate-limit
	// family as GetTicker/GetCandles/GetInstrument.
	err := s.call(r.Context(), gateway.ClassMarket, r, func() error {
		var innerErr error
		result, innerErr = s.client.GetFundingRateHistory(instID, limit)
		return innerErr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *service) handleGetInstrument(w http.ResponseWriter, r *http.Request) {
	instType := r.URL.Query().Get("instType")
	instID := r.URL.Query().Get("instId")
	if instType == "" || instID == "" {
		writeError(w, http.StatusBadRequest, errors.New("instType and instId are required"))
		return
	}
	var result domain.Instrument
	// ClassMarket, not ClassAccount: /public/instruments is unauthenticated market metadata, same
	// rate-limit family as GetTicker/GetCandles.
	err := s.call(r.Context(), gateway.ClassMarket, r, func() error {
		var innerErr error
		result, innerErr = s.client.GetInstrument(instType, instID)
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

// handlePlaceAlgoOrder places a resting SL/TP order. ClassTrade, not ClassAccount: it is a
// mutating trade action competing for the same OKX-side budget as order placement, and it is on
// the critical path of opening a position — a real position is not considered protected until this
// call has succeeded, so it must not queue behind bulk market-data traffic.
func (s *service) handlePlaceAlgoOrder(w http.ResponseWriter, r *http.Request) {
	var req domain.AlgoOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var algoID string
	err := s.call(r.Context(), gateway.ClassTrade, r, func() error {
		var innerErr error
		algoID, innerErr = s.client.PlaceAlgoOrder(req)
		return innerErr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"algoId": algoID})
}

// handleAmendAlgoOrder moves a resting SL/TP order's trigger price(s).
func (s *service) handleAmendAlgoOrder(w http.ResponseWriter, r *http.Request) {
	var req domain.AlgoOrderAmend
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := s.call(r.Context(), gateway.ClassTrade, r, func() error {
		return s.client.AmendAlgoOrder(req)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCancelAlgoOrder removes a resting SL/TP order.
func (s *service) handleCancelAlgoOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InstID string `json:"instId"`
		AlgoID string `json:"algoId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := s.call(r.Context(), gateway.ClassTrade, r, func() error {
		return s.client.CancelAlgoOrder(req.InstID, req.AlgoID)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleGetAlgoOrder reports a resting SL/TP order's state. ClassAccount — a read, like order
// status, not a mutating trade action.
func (s *service) handleGetAlgoOrder(w http.ResponseWriter, r *http.Request) {
	instID := r.URL.Query().Get("instId")
	algoID := r.URL.Query().Get("algoId")
	if algoID == "" {
		writeError(w, http.StatusBadRequest, errors.New("algoId is required"))
		return
	}
	var status domain.AlgoOrderStatus
	err := s.call(r.Context(), gateway.ClassAccount, r, func() error {
		var innerErr error
		status, innerErr = s.client.GetAlgoOrder(instID, algoID)
		return innerErr
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
