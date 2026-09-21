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
	// UsedMarginUSD is the sum of every currently-OPEN position's margin (Size) in this mode —
	// AvailableMarginUSD is TotalEquityUSD minus that, floored at zero. Added 2026-09-20 for the
	// Trade page's order ticket, which needs to show how much of the trading cap is actually free
	// to size a new manual position against, not just the cap itself (which ignores what's already
	// committed to open positions).
	UsedMarginUSD      string `json:"usedMarginUsd"`
	AvailableMarginUSD string `json:"availableMarginUsd"`
	PnL24hUSD          string `json:"pnl24hUsd"`
	PnL24hPct          string `json:"pnl24hPct"`
	PnL7dUSD           string `json:"pnl7dUsd"`
	PnL7dPct           string `json:"pnl7dPct"`
	PnL30dUSD          string `json:"pnl30dUsd"`
	PnL30dPct          string `json:"pnl30dPct"`
}

// statsMode resolves the mode query param to "paper", "bot", or "manual" — CLAUDE.md real-trading
// readiness plan, 2026-09-04: stats/config now serve both tabs from one handler rather than a
// hardcoded "paper". "manual" added 2026-09-20 (Account page): manual trading's own trading-cap
// row needs the same open-count/PnL-history stats bot's does. Defaults to "paper" (every caller
// before this change implicitly meant paper trading).
func statsMode(raw string) (string, bool) {
	switch raw {
	case "":
		return "paper", true
	case "paper", "bot", "manual":
		return raw, true
	default:
		return "", false
	}
}

func (s *Server) handlePaperTradingStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mode, ok := statsMode(r.URL.Query().Get("mode"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid mode (want paper or bot)")
		return
	}

	open := true
	var openCount int
	var usedMarginUSD decimal.Decimal
	switch mode {
	case "bot":
		positions, err := s.Repo.ListBotPositions(ctx, port.PositionFilter{Open: &open})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		openCount = len(positions)
		for _, p := range positions {
			usedMarginUSD = usedMarginUSD.Add(p.Size)
		}
	case "manual":
		// manual_orders is its own table, not paper_orders (CLAUDE.md real-trading readiness plan)
		// — mirrors handleListPositions' own mode branch rather than reading a table this mode's
		// orders were never written to. ListManualOrders rather than the narrower
		// CountManualOrders: UsedMarginUSD (2026-09-20) needs each row's own Size, not just a count.
		positions, err := s.Repo.ListManualOrders(ctx, port.PositionFilter{Open: &open})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		openCount = len(positions)
		for _, p := range positions {
			usedMarginUSD = usedMarginUSD.Add(p.Size)
		}
	default:
		positions, err := s.Repo.ListPositions(ctx, port.PositionFilter{Mode: mode, Open: &open})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		openCount = len(positions)
		for _, p := range positions {
			usedMarginUSD = usedMarginUSD.Add(p.Size)
		}
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

	// Floored at zero rather than going negative: if open margin somehow exceeds the current cap
	// (e.g. the cap was just lowered below what's already committed), "available" reads as none
	// left, which is the honest answer, rather than a negative number a sizing UI would have to
	// special-case.
	availableMarginUSD := account.EquityUSD.Sub(usedMarginUSD)
	if availableMarginUSD.IsNegative() {
		availableMarginUSD = decimal.Zero
	}

	writeJSON(w, http.StatusOK, paperTradingStatsView{
		OpenCount:          openCount,
		TotalEquityUSD:     account.EquityUSD.String(),
		AccountBalanceUSD:  account.AccountBalanceUSD.String(),
		UsedMarginUSD:      usedMarginUSD.String(),
		AvailableMarginUSD: availableMarginUSD.String(),
		PnL24hUSD:          pnl24hUSD.String(),
		PnL24hPct:          pnl24hPct.String(),
		PnL7dUSD:           pnl7dUSD.String(),
		PnL7dPct:           pnl7dPct.String(),
		PnL30dUSD:          pnl30dUSD.String(),
		PnL30dPct:          pnl30dPct.String(),
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

// tokenStatsView is one row of the panel's "Manage tokens" all-time stats table (2026-09-04
// request, widened from a 24h window to all-time 2026-09-22 per operator instruction).
type tokenStatsView struct {
	InstID        string `json:"instId"`
	PositionCount int64  `json:"positionCount"`
	PnLUSD        string `json:"pnlUsd"`
	PnLPct        string `json:"pnlPct"`
}

func (s *Server) handleTokenStatsAllTime(w http.ResponseWriter, r *http.Request) {
	mode, ok := statsMode(r.URL.Query().Get("mode"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid mode (want paper or bot)")
		return
	}
	stats, err := s.Repo.TokenStatsAllTime(r.Context(), mode)
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
		writeError(w, http.StatusBadRequest, "invalid mode (want paper or bot)")
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
		writeError(w, http.StatusBadRequest, "invalid mode (want paper or bot)")
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
	// Real mode's Stop button (operator decision, 2026-09-04): close every open real position
	// IMMEDIATELY, from this always-running process, rather than waiting for cmd/trader to notice
	// trading_state='stopped' at its own next startup — a market-volatility emergency stop must not
	// depend on a restart completing. Paper mode keeps its existing behavior (cmd/paper-trader's own
	// startup sweep, RequestManualCloseAll) unchanged; this is additive, real-mode-only.
	if mode == "bot" && req.TradingState != nil && *req.TradingState == "stopped" {
		if _, err := s.Repo.RequestBotManualCloseAll(r.Context()); err != nil {
			s.Logger.Error("failed to request immediate close of all open real positions", "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "restartRequired": true})
}
