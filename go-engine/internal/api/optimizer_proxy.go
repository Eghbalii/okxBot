package api

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// optimizerHTTPClient is shared across every proxy call — short timeout since this is a
// same-Docker-network hop, not a call across the internet.
var optimizerHTTPClient = &http.Client{Timeout: 30 * time.Second}

// proxyOptimizerRequest forwards method+query+body to cmd/strategy-optimizer at path and copies
// its response back verbatim — mirrors this codebase's established proxy pattern (§18's
// tester_proxy.go, §22's paper_trader_proxy.go): the panel never talks to an internal service
// directly, always through cmd/api (CLAUDE.md §11). Query string is forwarded so
// GET /candidates?kind=...&instId=... reaches the optimizer with its filters intact.
func (s *Server) proxyOptimizerRequest(w http.ResponseWriter, r *http.Request, path string) {
	if s.OptimizerBaseURL == "" {
		writeError(w, http.StatusServiceUnavailable, "strategy-optimizer is not configured (OPTIMIZER_ADDR/STRATEGY_OPTIMIZER_URL unset)")
		return
	}
	url := s.OptimizerBaseURL + path
	if q := r.URL.RawQuery; q != "" {
		url += "?" + q
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := optimizerHTTPClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("strategy-optimizer unreachable: %v", err))
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) proxyOptimizer(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.proxyOptimizerRequest(w, r, path)
	}
}

func (s *Server) proxyOptimizerWithID(pathFmt string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.proxyOptimizerRequest(w, r, fmt.Sprintf(pathFmt, r.PathValue("id")))
	}
}
