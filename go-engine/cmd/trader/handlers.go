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
//
// Exits 0, and the compose service uses restart: unless-stopped (changed 2026-09-09).
//
// The history here is worth keeping, because the previous two attempts each fixed one failure by
// creating another:
//
//  1. Originally exit(0) under on-failure:5 — copied from paper-trader, whose unless-stopped policy
//     makes the exit code irrelevant. on-failure only relaunches on a NONZERO exit, so a Resume
//     click saved trading_state='running' and then sat Exited(0) forever with no process to read
//     it (found live 2026-09-05, 12 minutes dead).
//  2. Then exit(1) under on-failure:5, on the reasoning that a nonzero exit would relaunch without
//     touching the retry cap. That reasoning was simply wrong: Docker's on-failure:N counts EVERY
//     nonzero exit toward N and only resets after a sufficiently long successful run. Six panel
//     restarts exhausted the five retries and the trader stayed dead — found 2026-09-09 with two
//     real positions open on the exchange, both flagged for manual close, both still losing (PEPE
//     -9.5%, BTC -5.3%) because nothing was alive to close them.
//
// The lesson is that exit code and restart policy have to be chosen together, not one at a time.
// on-failure existed to stop a misconfigured startup (a missing gateway, real money without
// allow_real_money) from crash-looping forever — but those are exactly the cases that DO exit
// nonzero and SHOULD stop retrying, while an operator-requested restart is a clean, successful
// exit that should always relaunch. unless-stopped + exit(0) gives both: cmd/trader's startup
// guards still exit(1), and Docker's default backoff already spaces a genuine crash-loop out
// rather than spinning, so the cap was never the thing protecting against it.
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
