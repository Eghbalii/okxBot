// Package api implements cmd/api, the dashboard/reporting backend (CLAUDE.md §11): RL model
// status, strategy CRUD + assignments + stats, and the positions panel. No auth in v1 — access
// control is the OpenVPN tunnel in front of this service, not this package.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// Server holds the dependencies cmd/api's handlers need.
type Server struct {
	Repo       port.Repository
	RLBaseURL  string
	GrafanaURL string
	// TesterBaseURL is cmd/strategy-tester's base URL — proxied through so the panel never talks
	// to it directly (matching every other backend, all reachable only via cmd/api, CLAUDE.md §11).
	// Empty disables the tester routes with a clear error rather than a confusing connection-reset.
	TesterBaseURL string
	// PaperTraderBaseURL is cmd/paper-trader's control-box HTTP surface — used only to proxy the
	// restart button (CLAUDE.md): GET/PUT /api/paper-trading/config talk to Postgres directly via
	// Repo instead, same as every other cmd/api handler, since that data must stay readable/
	// writable even when cmd/paper-trader itself happens to be down or mid-restart.
	PaperTraderBaseURL string
	ProcessMgr         string
	Units              []string
	Logger             *slog.Logger

	// AccountInitialUSD seeds a mode's account row on first read, matching what the trading
	// services are configured with (CLAUDE.md §15.6) — cmd/api must not invent a different starting
	// balance than the engine actually trades against.
	AccountInitialUSD decimal.Decimal

	// AllInstIDs is the full configured instrument roster, used as GET /api/paper-trading/config's
	// AllInstIDs field. Was named BackfillInstIDs (and paired with a now-removed BackfillBars)
	// before the candle backfill feature itself (POST /api/candles/backfill, usecase.Backfill) was
	// removed entirely (2026-09-01, explicit operator instruction: that OKX endpoint must never be
	// called) — renamed since it now has nothing to do with backfill.
	AllInstIDs []string

	// hub fans out real-time paper-order open/close events to connected panel WebSocket clients
	// (CLAUDE.md §11.4). Lazily initialized by Routes/Hub so callers never need to construct it
	// themselves.
	hub *wsHub
}

// Hub returns the Server's WebSocket broadcast hub, initializing it on first call — cmd/api's
// main.go uses this to get a handle for feeding in Kafka-consumed events (Broadcast), and Routes
// uses it to register the GET /api/ws endpoint. Both must share the same hub instance.
func (s *Server) Hub() *wsHub {
	if s.hub == nil {
		s.hub = newWSHub(s.Logger)
	}
	return s.hub
}

// Broadcast pushes msg (typically a JSON-marshaled usecase.PaperOrderEvent) to every connected
// panel WebSocket client.
func (s *Server) Broadcast(msg []byte) {
	s.Hub().broadcast(msg)
}

// CloseWS disconnects every connected WebSocket client — called on server shutdown.
func (s *Server) CloseWS() {
	if s.hub != nil {
		s.hub.closeAll()
	}
}

// Routes builds the HTTP handler for all panel endpoints.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/resources", s.handleResources)
	mux.HandleFunc("GET /api/ws", s.Hub().handleWS)

	mux.HandleFunc("GET /api/model/status", s.handleModelStatus)
	mux.HandleFunc("GET /api/model/logs", s.handleModelLogs)

	mux.HandleFunc("GET /api/strategies", s.handleListStrategies)
	mux.HandleFunc("POST /api/strategies", s.handleCreateStrategy)
	mux.HandleFunc("GET /api/strategies/{id}", s.handleGetStrategy)
	mux.HandleFunc("PUT /api/strategies/{id}", s.handleUpdateStrategy)
	mux.HandleFunc("DELETE /api/strategies/{id}", s.handleDeleteStrategy)
	mux.HandleFunc("POST /api/strategies/{id}/reset", s.handleResetStrategy)
	mux.HandleFunc("GET /api/strategies/{id}/stats", s.handleStrategyStats)

	mux.HandleFunc("GET /api/assignments", s.handleListAssignments)
	mux.HandleFunc("POST /api/assignments", s.handleCreateAssignment)
	mux.HandleFunc("PATCH /api/assignments/{id}", s.handleSetAssignmentEnabled)
	mux.HandleFunc("DELETE /api/assignments/{id}", s.handleDeleteAssignment)

	mux.HandleFunc("GET /api/positions", s.handleListPositions)
	mux.HandleFunc("POST /api/positions/{id}/close", s.handleClosePosition)

	// CLAUDE.md §15.6/§15.7: the shared account's current balance and its timeline, backing the
	// panel's balance chart — the point of which is that a drain-and-reset that happened overnight
	// is reviewable after the fact, not only visible in logs nobody was watching.
	mux.HandleFunc("GET /api/account", s.handleGetAccount)
	mux.HandleFunc("GET /api/account/history", s.handleAccountHistory)

	// CLAUDE.md §15.4/§15.12 revision, 2026-09-02: backs the order-detail modal's adjustment
	// history table — replaces the old baseline-vs-rl_adjusted A/B comparison, which had no data
	// once the SL/TP-adjust mechanic stopped forking orders.
	mux.HandleFunc("GET /api/positions/{id}/adjustments", s.handleListPaperOrderAdjustments)

	// CLAUDE.md §16 point 6: backs the Strategies page's price-line + parameter-change-marker
	// chart — candles for the price line, param-changes for the vertical markers.
	mux.HandleFunc("GET /api/candles", s.handleListCandles)
	mux.HandleFunc("GET /api/strategies/{id}/param-changes", s.handleListParamChanges)

	// Independent strategy-tester tab (2026-08-30 request): proxied through, same access-control
	// posture as every other panel data source (CLAUDE.md §11) — the panel never talks to
	// cmd/strategy-tester directly.
	mux.HandleFunc("GET /api/tester/stats", s.proxyTester("/stats"))
	mux.HandleFunc("GET /api/tester/versions/{id}", s.proxyTesterWithID("/versions/%s"))
	mux.HandleFunc("POST /api/tester/versions", s.proxyTesterBody("/versions"))
	mux.HandleFunc("POST /api/tester/versions/{id}/enable", s.proxyTesterWithIDBody("/versions/%s/enable"))
	mux.HandleFunc("DELETE /api/tester/versions/{id}", s.proxyTesterWithID("/versions/%s"))
	mux.HandleFunc("GET /api/tester/config", s.proxyTester("/config"))
	mux.HandleFunc("PUT /api/tester/config", s.proxyTesterBody("/config"))
	mux.HandleFunc("POST /api/tester/restart", s.proxyTesterBody("/restart"))

	// Panel control-box for paper trading (CLAUDE.md): pause/stop, long/short toggle, active
	// strategies/tokens/timeframes. Config reads/writes hit Postgres directly (Repo) rather than
	// proxying to cmd/paper-trader, so they stay available even if that process is down or
	// mid-restart; only the restart action itself needs to reach the running process.
	mux.HandleFunc("GET /api/paper-trading/stats", s.handlePaperTradingStats)
	mux.HandleFunc("GET /api/paper-trading/config", s.handleGetPaperTradingConfig)
	mux.HandleFunc("PUT /api/paper-trading/config", s.handleSavePaperTradingConfig)
	mux.HandleFunc("POST /api/paper-trading/restart", s.proxyPaperTrader("/restart"))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

// handleResources reports where the Grafana dashboard lives (CLAUDE.md §11.1) — cmd/api does not
// duplicate CPU/RAM/GPU charting itself, Prometheus+Grafana already own that.
func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"grafanaUrl": s.GrafanaURL})
}

// handleModelStatus reports RL service liveness (CLAUDE.md §11.2): the /health proxy answers
// "is a model loaded," and per-unit process-manager status answers "is the process itself up, and
// for how long, and has it been crash-restarting."
func (s *Server) handleModelStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	health := FetchRLHealth(ctx, s.RLBaseURL)
	units := make([]UnitStatus, 0, len(s.Units))
	for _, u := range s.Units {
		units = append(units, ProcessStatus(ctx, s.ProcessMgr, u))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"health": health,
		"units":  units,
	})
}

// handleModelLogs tails one unit's logs, optionally filtered to error-level lines
// (?unit=...&lines=200&errors=true).
func (s *Server) handleModelLogs(w http.ResponseWriter, r *http.Request) {
	unit := r.URL.Query().Get("unit")
	if unit == "" {
		writeError(w, http.StatusBadRequest, "missing required query param: unit")
		return
	}
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			lines = n
		}
	}
	errorsOnly := r.URL.Query().Get("errors") == "true"

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	out, err := LogTail(ctx, s.ProcessMgr, unit, lines, errorsOnly)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"unit": unit, "logs": out})
}

func (s *Server) handleListStrategies(w http.ResponseWriter, r *http.Request) {
	instID := r.URL.Query().Get("instId")
	enabledOnly := r.URL.Query().Get("enabledOnly") == "true"
	list, err := s.Repo.ListStrategies(r.Context(), instID, enabledOnly)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetStrategy(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := s.Repo.GetStrategy(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// createStrategyRequest clones an existing strategy (origin or another sub-strategy) into a new
// sub-strategy row — the only way to create a strategy row via the API. Origin rows themselves
// are seeded by cmd/paper-trader from strategy.Factories, not created here (CLAUDE.md §11.3).
type createStrategyRequest struct {
	Name       string          `json:"name"`
	ClonedFrom int64           `json:"clonedFrom"`
	InstIDs    []string        `json:"instIds"`
	Config     json.RawMessage `json:"config"`
}

func (s *Server) handleCreateStrategy(w http.ResponseWriter, r *http.Request) {
	var req createStrategyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.ClonedFrom == 0 {
		writeError(w, http.StatusBadRequest, "clonedFrom is required — every strategy must descend from an origin row")
		return
	}
	source, err := s.Repo.GetStrategy(r.Context(), req.ClonedFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "clonedFrom strategy not found: "+err.Error())
		return
	}

	config := req.Config
	if len(config) == 0 {
		config = source.Config
	}
	originID := req.ClonedFrom
	id, err := s.Repo.CreateStrategy(r.Context(), port.StrategyConfig{
		Name:       req.Name,
		Kind:       source.Kind,
		InstIDs:    req.InstIDs,
		Config:     config,
		Enabled:    true,
		IsOrigin:   false,
		ClonedFrom: &originID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

type updateStrategyRequest struct {
	Config  json.RawMessage `json:"config"`
	Enabled bool            `json:"enabled"`
}

func (s *Server) handleUpdateStrategy(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	existing, err := s.Repo.GetStrategy(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if existing.IsOrigin {
		writeError(w, http.StatusForbidden, "origin strategies are read-only — clone it into a sub-strategy to customize (CLAUDE.md §11.3)")
		return
	}

	var req updateStrategyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.Repo.UpdateStrategyConfig(r.Context(), id, req.Config, req.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// CLAUDE.md §16, §16.3 step 5: manual panel edits go on the same parameter-change timeline the
	// optimizer writes to (source="manual"), so the Strategies page's marker overlay shows every
	// change regardless of who/what made it. Best-effort — the config update itself already
	// succeeded above, so a logging failure here must not turn into a client-visible error.
	for _, instID := range existing.InstIDs {
		if _, err := s.Repo.RecordParamChange(r.Context(), port.ParamChange{
			StrategyID: id,
			InstID:     instID,
			OldConfig:  existing.Config,
			NewConfig:  req.Config,
			Source:     "manual",
		}); err != nil && s.Logger != nil {
			s.Logger.Warn("failed to record manual param change", "strategyId", id, "instId", instID, "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteStrategy(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	existing, err := s.Repo.GetStrategy(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if existing.IsOrigin {
		writeError(w, http.StatusForbidden, "origin strategies cannot be deleted (CLAUDE.md §11.3: \"keep the origin always\")")
		return
	}
	if err := s.Repo.DeleteStrategy(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResetStrategy(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Repo.ResetStrategyToOrigin(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStrategyStats(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stats, err := s.Repo.StrategyStatsFor(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleListAssignments(w http.ResponseWriter, r *http.Request) {
	instID := r.URL.Query().Get("instId")
	enabledOnly := r.URL.Query().Get("enabledOnly") == "true"
	list, err := s.Repo.ListAssignments(r.Context(), instID, enabledOnly)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateAssignment(w http.ResponseWriter, r *http.Request) {
	var req port.StrategyAssignment
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	req.Enabled = true
	id, err := s.Repo.CreateAssignment(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *Server) handleSetAssignmentEnabled(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.Repo.SetAssignmentEnabled(r.Context(), id, req.Enabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteAssignment(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Repo.DeleteAssignment(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// defaultPositionsPageSize/maxPositionsPageSize bound handleListPositions' pageSize query param —
// added 2026-09-02 alongside server-side pagination (CLAUDE.md §11.4): with closed positions
// numbering in the hundreds, fetching every row on every 5s poll had become a genuinely slow query
// and a multi-MB payload, so the panel now asks for one page at a time. maxPositionsPageSize is a
// hard ceiling so a malformed/oversized pageSize can't silently reintroduce the unbounded fetch.
const (
	defaultPositionsPageSize = 50
	maxPositionsPageSize     = 200
)

// positionsListResponse wraps the page plus the total row count the filter matches, so the panel's
// pagination control knows how many pages exist without a second request.
type positionsListResponse struct {
	Items []port.PaperOrder `json:"items"`
	Total int               `json:"total"`
}

// handleListPositions serves the positions panel (CLAUDE.md §11.4): filterable by mode
// (paper/demo/real), instrument, open/closed, sortable by opened_at/closed_at/pnl/inst_id, and
// paged via page/pageSize (page is 0-indexed; pageSize defaults to defaultPositionsPageSize,
// capped at maxPositionsPageSize).
func (s *Server) handleListPositions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := port.PositionFilter{
		Mode:     q.Get("mode"),
		InstID:   q.Get("instId"),
		SortBy:   q.Get("sortBy"),
		SortDesc: q.Get("sortDesc") == "true",
	}
	if v := q.Get("open"); v != "" {
		open := v == "true"
		filter.Open = &open
	}

	pageSize := defaultPositionsPageSize
	if v := q.Get("pageSize"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	}
	if pageSize > maxPositionsPageSize {
		pageSize = maxPositionsPageSize
	}
	page := 0
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			page = n
		}
	}
	filter.Limit = pageSize
	filter.Offset = page * pageSize

	list, err := s.Repo.ListPositions(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, err := s.Repo.CountPositions(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, positionsListResponse{Items: list, Total: total})
}

// handleClosePosition lets the panel close an open paper position by hand (2026-08-31 request).
// cmd/api runs in a separate process from the PaperTrader that actually owns this order's
// instrument's tick stream, so it cannot close the order itself — it only flags intent via
// RequestManualClose; PaperTrader closes it at the live price on its next tick, close_reason=
// 'manual', reported to the model as closed_early (conductor.TerminalCategory). Only meaningful
// for paper-mode positions, since that is the only mode PaperTrader's loop monitors — demo/real
// positions have no equivalent close-my-own-order path yet.
func (s *Server) handleClosePosition(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Repo.RequestManualClose(r.Context(), id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// handleListPaperOrderAdjustments serves an order's in-trade SL/TP adjustment history (CLAUDE.md
// §15.4/§15.12 revision, 2026-09-02) — the audit trail the order-detail modal shows on click,
// replacing the old baseline-vs-rl_adjusted A/B comparison.
func (s *Server) handleListPaperOrderAdjustments(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	adjustments, err := s.Repo.ListPaperOrderAdjustments(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, adjustments)
}

// handleListCandles serves a plain recent-history read of the durable candles hypertable
// (?instId=&bar=&limit=, CLAUDE.md §16 point 6) — the price line the Strategies page's chart
// draws param-change markers on top of.
func (s *Server) handleListCandles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	instID := q.Get("instId")
	bar := q.Get("bar")
	if instID == "" || bar == "" {
		writeError(w, http.StatusBadRequest, "instId and bar are required query params")
		return
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	candles, err := s.Repo.ListCandles(r.Context(), instID, bar, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, candles)
}

// handleListParamChanges serves a strategy's parameter-change timeline for one instrument
// (?instId=&since=, CLAUDE.md §16 point 6) — the panel's chart marker data. instId is required:
// a strategy row's own InstIDs can list several tokens, but the chart is always for one at a
// time, so the caller (the panel) picks which.
func (s *Server) handleListParamChanges(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	instID := r.URL.Query().Get("instId")
	if instID == "" {
		writeError(w, http.StatusBadRequest, "instId is a required query param")
		return
	}
	since := time.Time{}
	if v := r.URL.Query().Get("since"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid since (want RFC3339): "+err.Error())
			return
		}
		since = parsed
	}
	changes, err := s.Repo.ListParamChanges(r.Context(), instID, since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Filter to this strategy id (ListParamChanges is instID-scoped only — one instrument can have
	// several strategies' history in principle, though today each inst_id+bar has one active
	// assignment; filtering here keeps the endpoint correct if that ever changes).
	out := make([]port.ParamChange, 0, len(changes))
	for _, c := range changes {
		if c.StrategyID == id {
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func pathInt64(r *http.Request, key string) (int64, error) {
	return strconv.ParseInt(r.PathValue(key), 10, 64)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
