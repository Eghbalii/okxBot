package api

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// traderHTTPClient mirrors paperTraderHTTPClient — short timeout since this is a same-Docker-
// network hop, not a call across the internet. A separate client/proxy pair rather than
// generalizing paper_trader_proxy.go's helpers to take a base URL parameter, same "duplication
// over coupling" reasoning that file's own comment already states.
var traderHTTPClient = &http.Client{Timeout: 5 * time.Second}

// proxyTraderRequest forwards method+body to cmd/trader's restart-only HTTP surface at path and
// copies its response back verbatim, same shape as proxyPaperTraderRequest.
func (s *Server) proxyTraderRequest(w http.ResponseWriter, r *http.Request, method, path string) {
	if s.TraderBaseURL == "" {
		writeError(w, http.StatusServiceUnavailable, "trader is not configured (TRADER_SERVICE_URL unset)")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), method, s.TraderBaseURL+path, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := traderHTTPClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("trader unreachable: %v", err))
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleRestartTrading routes to the mode-appropriate restart target — paper trading restarts
// cmd/paper-trader, real trading restarts cmd/trader (CLAUDE.md real-trading readiness plan,
// 2026-09-04). One handler rather than two separate routes so the panel's client.ts can call a
// single restartPaperTrader(mode) regardless of which tab it's on.
func (s *Server) handleRestartTrading(w http.ResponseWriter, r *http.Request) {
	mode, ok := statsMode(r.URL.Query().Get("mode"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid mode (want paper or real)")
		return
	}
	if mode == "real" {
		s.proxyTraderRequest(w, r, r.Method, "/restart")
		return
	}
	s.proxyPaperTraderRequest(w, r, r.Method, "/restart")
}
