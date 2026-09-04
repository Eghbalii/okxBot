package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
)

// traderService is cmd/trader's minimal HTTP surface (CLAUDE.md real-trading readiness plan,
// 2026-09-04) — mirrors cmd/paper-trader's own POST /restart mechanism, but narrower: unlike
// paper-trader's control-box config (pause/stop/strategies/tokens), real trading's config lives
// entirely in Postgres via cmd/api's mode-aware /paper-trading/config endpoints, and cmd/trader
// itself only ever reads it once at its own startup — so there is no GET/PUT /config to mirror
// here, only the restart action the panel's Real-tab Save & Apply / Pause / Stop buttons need to
// actually take effect.
type traderService struct {
	logger *slog.Logger
}

func (s *traderService) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /restart", s.handleRestart)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// handleRestart exits this process so Docker's restart policy relaunches it reading whatever
// config (use_conductor_lifecycle, strategy_assignments mode='real', paper_trading_config
// mode='real') was just saved — identical mechanism to cmd/paper-trader's own handleRestart.
func (s *traderService) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("restart requested via panel; exiting for the container's restart policy to relaunch")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
	go func() {
		os.Exit(0)
	}()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
