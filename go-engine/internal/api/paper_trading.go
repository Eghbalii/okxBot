package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// paperTradingStatsView backs the panel's stats box above the positions table (CLAUDE.md):
// open-position count, total account equity, and 24h/1w/1month realized PnL in both USD and
// percent — computed from the SAME account_equity/account_equity_history rows the panel's
// (not-yet-built) balance chart would read, so there is no second source of truth for equity.
type paperTradingStatsView struct {
	OpenCount int `json:"openCount"`
	// TotalEquityUSD ("Total Equity") is the balance SINCE the operator last chose a baseline via
	// POST /api/account/cap, or since the last automatic drain-to-zero reset — account_equity.
	// equity_usd. AccountBalanceUSD ("Account Balance") is the real, continuous running total that
	// baseline changes never touch (CLAUDE.md §31.2) — the two are equal until the first reset ever
	// happens for this mode, then diverge.
	TotalEquityUSD    string `json:"totalEquityUsd"`
	AccountBalanceUSD string `json:"accountBalanceUsd"`
	PnL24hUSD         string `json:"pnl24hUsd"`
	PnL24hPct         string `json:"pnl24hPct"`
	PnL7dUSD          string `json:"pnl7dUsd"`
	PnL7dPct          string `json:"pnl7dPct"`
	PnL30dUSD         string `json:"pnl30dUsd"`
	PnL30dPct         string `json:"pnl30dPct"`
}

// statsMode resolves the mode query param to "paper" or "real" — CLAUDE.md real-trading readiness
// plan, 2026-09-04: stats/config now serve both tabs from one handler rather than a hardcoded
// "paper". Defaults to "paper" (every caller before this change implicitly meant paper trading).
func statsMode(raw string) (string, bool) {
	switch raw {
	case "":
		return "paper", true
	case "paper", "real":
		return raw, true
	default:
		return "", false
	}
}

func (s *Server) handlePaperTradingStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mode, ok := statsMode(r.URL.Query().Get("mode"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid mode (want paper or real)")
		return
	}

	open := true
	var openCount int
	if mode == "real" {
		positions, err := s.Repo.ListRealPositions(ctx, port.PositionFilter{Open: &open})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		openCount = len(positions)
	} else {
		positions, err := s.Repo.ListPositions(ctx, port.PositionFilter{Mode: mode, Open: &open})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		openCount = len(positions)
	}

	account, err := s.Repo.GetAccountEquity(ctx, mode, s.AccountInitialUSD)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// One history read covering the longest window (30d) is enough for all three sub-windows —
	// each is a fold over the same rows, not a separate query.
	now := time.Now().UTC()
	history, err := s.Repo.ListEquityHistory(ctx, mode, now.Add(-30*24*time.Hour), 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	pnl24hUSD, pnl24hPct := realizedPnLOverWindow(history, now.Add(-24*time.Hour))
	pnl7dUSD, pnl7dPct := realizedPnLOverWindow(history, now.Add(-7*24*time.Hour))
	pnl30dUSD, pnl30dPct := realizedPnLOverWindow(history, now.Add(-30*24*time.Hour))

	writeJSON(w, http.StatusOK, paperTradingStatsView{
		OpenCount:         openCount,
		TotalEquityUSD:    account.EquityUSD.String(),
		AccountBalanceUSD: account.AccountBalanceUSD.String(),
		PnL24hUSD:         pnl24hUSD.String(),
		PnL24hPct:         pnl24hPct.String(),
		PnL7dUSD:          pnl7dUSD.String(),
		PnL7dPct:          pnl7dPct.String(),
		PnL30dUSD:         pnl30dUSD.String(),
		PnL30dPct:         pnl30dPct.String(),
	})
}

// realizedPnLOverWindow sums Reason="trade" deltas in history at or after since, and expresses
// that sum as a percent of the equity the window STARTED at (current equity minus the window's
// own summed delta — every delta in the window, trade and reset alike, since a reset genuinely
// changed the balance the window measures from). history is oldest-first (ListEquityHistory's
// contract).
func realizedPnLOverWindow(history []port.EquityPoint, since time.Time) (usd, pct decimal.Decimal) {
	if len(history) == 0 {
		return decimal.Zero, decimal.Zero
	}
	tradeSum := decimal.Zero
	windowSum := decimal.Zero
	for _, p := range history {
		if p.CreatedAt.Before(since) {
			continue
		}
		windowSum = windowSum.Add(p.DeltaUSD)
		if p.Reason == "trade" {
			tradeSum = tradeSum.Add(p.DeltaUSD)
		}
	}
	latestEquity := history[len(history)-1].EquityUSD
	startEquity := latestEquity.Sub(windowSum)
	if startEquity.IsZero() {
		return tradeSum, decimal.Zero
	}
	pct = tradeSum.Div(startEquity).Mul(decimal.NewFromInt(100))
	return tradeSum, pct
}

// tokenStatsView is one row of the panel's "Manage tokens" 24h stats table (2026-09-04 request).
type tokenStatsView struct {
	InstID        string `json:"instId"`
	PositionCount int64  `json:"positionCount"`
	PnLUSD        string `json:"pnlUsd"`
	PnLPct        string `json:"pnlPct"`
}

func (s *Server) handleTokenStats24h(w http.ResponseWriter, r *http.Request) {
	stats, err := s.Repo.TokenStats24h(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := make([]tokenStatsView, len(stats))
	for i, s := range stats {
		views[i] = tokenStatsView{
			InstID:        s.InstID,
			PositionCount: s.PositionCount,
			PnLUSD:        s.PnLUSD.String(),
			PnLPct:        s.PnLPct.String(),
		}
	}
	writeJSON(w, http.StatusOK, views)
}

// paperTradingConfigView mirrors cmd/paper-trader's own paperTradingConfigView shape (camelCase,
// same field names) — this handler talks to Postgres directly rather than proxying, but the panel
// should see identical JSON regardless of which config endpoint it happens to call.
type paperTradingConfigView struct {
	TradingState    string   `json:"tradingState"`
	DisableLong     bool     `json:"disableLong"`
	DisableShort    bool     `json:"disableShort"`
	ActiveKinds     []string `json:"activeKinds"`
	DisabledInstIDs []string `json:"disabledInstIds"`
	ActiveBars      []string `json:"activeBars"`
	// AllInstIDs is the full configured trading.inst_ids roster (not itself part of the saved
	// config; reuses Server.AllInstIDs, which already carries the same list) — lets the panel's
	// token-manage modal know what tokens exist to toggle, without a second endpoint.
	AllInstIDs []string `json:"allInstIds"`
}

// handleGetPaperTradingConfig returns the SAVED config from Postgres — note this may differ from
// what cmd/paper-trader is currently RUNNING with if a save hasn't been followed by a restart yet
// (that process only re-reads this at its own startup). The panel is responsible for surfacing
// "restart required" state, same as the strategy-tester tab already does for its own config.
func (s *Server) handleGetPaperTradingConfig(w http.ResponseWriter, r *http.Request) {
	mode, ok := statsMode(r.URL.Query().Get("mode"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid mode (want paper or real)")
		return
	}
	c, err := s.Repo.GetPaperTradingConfig(r.Context(), mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, paperTradingConfigView{
		TradingState:    c.TradingState,
		DisableLong:     c.DisableLong,
		DisableShort:    c.DisableShort,
		ActiveKinds:     c.ActiveKinds,
		DisabledInstIDs: c.DisabledInstIDs,
		ActiveBars:      c.ActiveBars,
		AllInstIDs:      s.AllInstIDs,
	})
}

type savePaperTradingConfigRequest struct {
	Mode            string    `json:"mode"`
	TradingState    *string   `json:"tradingState"`
	DisableLong     *bool     `json:"disableLong"`
	DisableShort    *bool     `json:"disableShort"`
	ActiveKinds     *[]string `json:"activeKinds"`
	DisabledInstIDs *[]string `json:"disabledInstIds"`
	ActiveBars      *[]string `json:"activeBars"`
}

func (s *Server) handleSavePaperTradingConfig(w http.ResponseWriter, r *http.Request) {
	var req savePaperTradingConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	mode, ok := statsMode(req.Mode)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid mode (want paper or real)")
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
	if _, err := s.Repo.SavePaperTradingConfig(r.Context(), mode, patch); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "restartRequired": true})
}
