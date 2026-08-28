package api

import (
	"net/http"
	"strconv"
	"time"
)

// defaultEquityHistoryLimit bounds an unqualified history read so a long-running paper account
// can't return an unbounded row count to the panel's chart. The panel can ask for more explicitly.
const defaultEquityHistoryLimit = 1000

// validModes are the trading modes an account balance is tracked for (CLAUDE.md §15.6). Validated
// here rather than passed through, so a typo'd mode is a 400 instead of silently seeding a new
// account row for a mode nothing ever trades against.
var validModes = map[string]bool{"paper": true, "demo": true, "real": true}

// queryMode resolves the ?mode= parameter, defaulting to "paper" (the only mode with a live writer
// today, CLAUDE.md §11.4). Reports false after writing a 400 if the mode isn't recognized.
func queryMode(w http.ResponseWriter, r *http.Request) (string, bool) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		return "paper", true
	}
	if !validModes[mode] {
		writeError(w, http.StatusBadRequest, "invalid mode (want paper, demo, or real): "+mode)
		return "", false
	}
	return mode, true
}

// handleGetAccount returns one mode's current shared-account balance — equity, its configured
// starting point, and how many times a drain has reset it (CLAUDE.md §15.6/§15.7). The reset count
// is the signal worth watching: an account resetting daily is a failing configuration, not free
// money, and surfacing it is the whole reason it's tracked.
func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	mode, ok := queryMode(w, r)
	if !ok {
		return
	}

	account, err := s.Repo.GetAccountEquity(r.Context(), mode, s.AccountInitialUSD)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, account)
}

// handleAccountHistory returns one mode's balance timeline, oldest-first, for the panel's chart
// (CLAUDE.md §15.7). Each point carries the reason it was written ("trade", "reset", "seed"), which
// is what lets the chart mark a drain-and-reset distinctly from ordinary trade PnL.
func (s *Server) handleAccountHistory(w http.ResponseWriter, r *http.Request) {
	mode, ok := queryMode(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()

	var since time.Time
	if v := q.Get("since"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid since (want RFC3339): "+err.Error())
			return
		}
		since = parsed
	}

	limit := defaultEquityHistoryLimit
	if v := q.Get("limit"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit (want a non-negative integer)")
			return
		}
		limit = parsed
	}

	points, err := s.Repo.ListEquityHistory(r.Context(), mode, since, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, points)
}
