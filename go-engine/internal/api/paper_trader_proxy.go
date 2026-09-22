package api

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// paperTraderHTTPClient mirrors testerHTTPClient — short timeout since this is a same-Docker-
// network hop, not a call across the internet. Kept as its own client/proxy pair rather than
// generalizing tester_proxy.go's helpers to take a base URL parameter: each proxy is small, and
// duplicating it keeps cmd/paper-trader's proxy path from silently changing if strategy-tester's
// ever does (same "duplication over coupling" pattern this codebase already uses elsewhere, e.g.
// internal/tester.RealizedPnL's own small duplicated helper).
var paperTraderHTTPClient = &http.Client{Timeout: 5 * time.Second}

// proxyPaperTraderRequest forwards method+body to cmd/paper-trader's control-box HTTP surface at
// path and copies its response back verbatim, same shape as proxyTesterRequest.
func (s *Server) proxyPaperTraderRequest(w http.ResponseWriter, r *http.Request, method, path string) {
	s.proxyPaperTraderRequestTo(w, r, s.PaperTraderBaseURL, method, path)
}

// proxyPaperTraderRequestTo is proxyPaperTraderRequest widened to take an explicit base URL
// (2026-09-22, multi-exchange paper trading) — a second, independent cmd/paper-trader instance
// (e.g. paper-trader-mexc) runs its own control-box HTTP surface on its own port, so
// handleRestartTrading needs to pick which one to reach rather than always the OKX default.
func (s *Server) proxyPaperTraderRequestTo(w http.ResponseWriter, r *http.Request, baseURL, method, path string) {
	if baseURL == "" {
		writeError(w, http.StatusServiceUnavailable, "paper-trader is not configured (PAPER_TRADER_SERVICE_URL unset)")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), method, baseURL+path, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := paperTraderHTTPClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("paper-trader unreachable: %v", err))
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) proxyPaperTrader(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.proxyPaperTraderRequest(w, r, r.Method, path)
	}
}
