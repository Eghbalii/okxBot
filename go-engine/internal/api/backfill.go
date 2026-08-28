package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

// backfillRequest is the POST /api/candles/backfill body. Every field is optional: an empty body
// backfills the server's configured instruments and bars at the default depth, which is the common
// case ("load history for what I actually trade").
type backfillRequest struct {
	InstIDs []string `json:"instIds"`
	Bars    []string `json:"bars"`
	// TargetCandles per (instrument, bar). Sized in CANDLES rather than days because ReplayEnv
	// advances one candle per training step — "30 days" is ~30 rows on a 1D bar and ~8,600 on 5m,
	// so a day-based depth starves exactly the timeframes that need the most history.
	TargetCandles int `json:"targetCandles"`
}

type backfillResponse struct {
	Results  []usecase.BackfillResult `json:"results"`
	Fetched  int                      `json:"fetched"`
	Stored   int                      `json:"stored"`
	Duration string                   `json:"duration"`
}

// handleBackfillCandles loads historical candles from the exchange into Postgres, so warm-start
// training has real market history without waiting days for the live ingestor to accumulate it
// (CLAUDE.md §15.8).
//
// Runs SYNCHRONOUSLY and can take minutes: it is paced to stay inside OKX's rate limit, and the
// caller wants to know what actually landed. A fire-and-forget job would need its own status
// endpoint and progress store to answer the only question anyone asks afterward ("did it work?"),
// which is not worth the machinery for an operation run a handful of times.
//
// Safe to re-run: candle writes upsert on (inst_id, bar, ts), so an interrupted run is resumed by
// simply issuing the request again.
func (s *Server) handleBackfillCandles(w http.ResponseWriter, r *http.Request) {
	if s.Backfill == nil {
		// cmd/api can run without exchange credentials (it is primarily a read-only panel
		// backend), so this is a configuration state rather than a bug.
		writeError(w, http.StatusServiceUnavailable, "backfill is not configured: cmd/api has no exchange client")
		return
	}

	var req backfillRequest
	if r.Body != nil {
		// An empty body is valid and means "use the configured defaults", so a decode failure on
		// EOF is not an error.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}

	instIDs := req.InstIDs
	if len(instIDs) == 0 {
		instIDs = s.BackfillInstIDs
	}
	bars := req.Bars
	if len(bars) == 0 {
		bars = s.BackfillBars
	}
	if len(instIDs) == 0 || len(bars) == 0 {
		writeError(w, http.StatusBadRequest,
			"no instruments or bars: pass them in the request body, or configure trading.inst_ids and paper_trading.bars")
		return
	}

	started := time.Now()
	results, err := s.Backfill.Run(r.Context(), usecase.BackfillRequest{
		InstIDs:       instIDs,
		Bars:          bars,
		TargetCandles: req.TargetCandles,
	})
	// A cancelled or partially-failed run still reports what landed — those rows are durably
	// stored, and knowing which pairs succeeded is what tells the operator whether to re-run.
	if err != nil && len(results) == 0 {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := backfillResponse{Results: results, Duration: time.Since(started).Round(time.Second).String()}
	for _, res := range results {
		resp.Fetched += res.Fetched
		resp.Stored += res.Stored
	}
	writeJSON(w, http.StatusOK, resp)
}
