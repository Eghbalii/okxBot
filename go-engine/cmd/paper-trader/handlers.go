package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// paperTraderService is the minimal HTTP surface for the panel's control-box (CLAUDE.md):
// cmd/paper-trader is otherwise a headless consumer-loop process (only /metrics is bound), so this
// mirrors cmd/strategy-tester's own GET/PUT /config + POST /restart pattern faithfully rather than
// inventing a new one — including the same GET-reads-memory / PUT-writes-DB / restart-required
// asymmetry, and the same self-exit-and-rely-on-the-container's-restart-policy mechanism, since
// this binary has no Docker socket access either.
type paperTraderService struct {
	repo       port.Repository
	logger     *slog.Logger
	current    port.PaperTradingConfig // snapshot read at startup; GET /config reflects THIS, not a live DB round-trip
	allInstIDs []string
}

func (s *paperTraderService) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", s.handleGetConfig)
	mux.HandleFunc("PUT /config", s.handleSaveConfig)
	mux.HandleFunc("POST /restart", s.handleRestart)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

type paperTradingConfigView struct {
	TradingState    string   `json:"tradingState"`
	DisableLong     bool     `json:"disableLong"`
	DisableShort    bool     `json:"disableShort"`
	ActiveKinds     []string `json:"activeKinds"`
	DisabledInstIDs []string `json:"disabledInstIds"`
	ActiveBars      []string `json:"activeBars"`
	// AllInstIDs is the full configured trading.inst_ids roster (not itself part of the saved
	// config) — included so the panel's token-manage modal knows what tokens exist to toggle,
	// without a second endpoint.
	AllInstIDs []string `json:"allInstIds"`
}

func (s *paperTraderService) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, paperTradingConfigView{
		TradingState:    s.current.TradingState,
		DisableLong:     s.current.DisableLong,
		DisableShort:    s.current.DisableShort,
		ActiveKinds:     s.current.ActiveKinds,
		DisabledInstIDs: s.current.DisabledInstIDs,
		ActiveBars:      s.current.ActiveBars,
		AllInstIDs:      s.allInstIDs,
	})
}

type savePaperTradingConfigRequest struct {
	TradingState    *string   `json:"tradingState"`
	DisableLong     *bool     `json:"disableLong"`
	DisableShort    *bool     `json:"disableShort"`
	ActiveKinds     *[]string `json:"activeKinds"`
	DisabledInstIDs *[]string `json:"disabledInstIds"`
	ActiveBars      *[]string `json:"activeBars"`
}

// handleSaveConfig persists the panel's control-box edits to the paper_trading_config row. Does
// NOT apply live and does NOT restart itself — same "save, then an explicit separate restart"
// split as cmd/strategy-tester's handleSaveConfig, for the same reason: several of these fields
// (active_bars especially) need a fresh candle-window/assignment reload that only a real process
// restart gives cleanly.
func (s *paperTraderService) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var req savePaperTradingConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.TradingState != nil {
		switch *req.TradingState {
		case "running", "paused", "stopped":
		default:
			writeError(w, http.StatusBadRequest, "invalid tradingState (want running, paused, or stopped): "+*req.TradingState)
			return
		}
	}
	patch := port.PaperTradingConfigPatch{
		TradingState:    req.TradingState,
		DisableLong:     req.DisableLong,
		DisableShort:    req.DisableShort,
		ActiveKinds:     req.ActiveKinds,
		DisabledInstIDs: req.DisabledInstIDs,
		ActiveBars:      req.ActiveBars,
	}
	if _, err := s.repo.SavePaperTradingConfig(r.Context(), patch); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "restartRequired": true})
}

// handleRestart exits this process so Docker's restart policy (paper-trader already runs
// restart: unless-stopped) brings it back reading the paper_trading_config row just saved —
// identical mechanism and identical reasoning to cmd/strategy-tester's handleRestart.
func (s *paperTraderService) handleRestart(w http.ResponseWriter, r *http.Request) {
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

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
