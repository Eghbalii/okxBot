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
// mode='real') was just saved — identical mechanism to cmd/paper-trader's own handleRestart, with
// one deliberate difference: this exits with status 1, not 0.
//
// cmd/trader's compose service uses restart: on-failure:5 (not paper-trader's unless-stopped) —
// on purpose, so a genuinely missing/misconfigured okx-gateway at startup doesn't crash-loop
// forever (CLAUDE.md §27.1's docker-compose.yml comment). on-failure only relaunches on a
// NONZERO exit code, so the os.Exit(0) this handler originally used (copied verbatim from
// paper-trader, whose unless-stopped policy makes exit code irrelevant) meant a panel Resume
// click here would save trading_state='running' to Postgres, self-exit cleanly, and then just...
// stay dead — Docker correctly saw a successful exit and did nothing. Found live 2026-09-05: the
// container sat Exited(0) for 12 minutes after a Resume click, with trading_state='running' in
// the DB the whole time and no process left to read it. Exit(1) makes this restart count as a
// failure for on-failure's purposes without touching the cap (this is one restart, not five) or
// reopening the missing-gateway crash-loop concern the comment above is actually about.
func (s *traderService) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("restart requested via panel; exiting for the container's restart policy to relaunch")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
	go func() {
		os.Exit(1)
	}()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
