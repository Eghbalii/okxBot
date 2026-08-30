package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/tester"
)

func (s *service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /versions", s.handleListVersions)
	mux.HandleFunc("GET /versions/{id}", s.handleGetVersion)
	mux.HandleFunc("POST /versions", s.handleCreateVersion)
	mux.HandleFunc("POST /versions/{id}/enable", s.handleEnableVersion)
	mux.HandleFunc("GET /config", s.handleGetConfig)
	mux.HandleFunc("PUT /config", s.handleSaveConfig)
	mux.HandleFunc("POST /restart", s.handleRestart)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// versionView is one version's data plus its stats, shaped for the panel's stats-only display
// (the operator explicitly asked for aggregate stats per version, not a positions list).
type versionView struct {
	ID              int64              `json:"id"`
	Kind            string             `json:"kind"`
	Version         int                `json:"version"`
	DisplayName     string             `json:"displayName"` // "{kind}_v{version}"
	Config          map[string]float64 `json:"config"`
	ParentVersionID *int64             `json:"parentVersionId,omitempty"`
	Enabled         bool               `json:"enabled"`
	Stats           versionStatsView   `json:"stats"`
}

type versionStatsView struct {
	SignalCount int64  `json:"signalCount"`
	Wins        int64  `json:"wins"`
	Losses      int64  `json:"losses"`
	TPCloses    int64  `json:"tpCloses"`
	SLCloses    int64  `json:"slCloses"`
	OpenCount   int64  `json:"openCount"`
	WinRatePct  string `json:"winRatePct"` // "—" when no decided trades yet
	RealizedPnL string `json:"realizedPnl"`
}

func toVersionStatsView(vs tester.VersionStats) versionStatsView {
	decided := vs.Wins + vs.Losses
	winRate := "—"
	if decided > 0 {
		winRate = fmt.Sprintf("%d%%", (vs.Wins*100)/decided)
	}
	return versionStatsView{
		SignalCount: vs.SignalCount,
		Wins:        vs.Wins,
		Losses:      vs.Losses,
		TPCloses:    vs.TPCloses,
		SLCloses:    vs.SLCloses,
		OpenCount:   vs.OpenCount,
		WinRatePct:  winRate,
		RealizedPnL: vs.RealizedPnL.StringFixed(2),
	}
}

func toVersionView(ctx context.Context, s *service, v tester.Version) (versionView, error) {
	var configMap map[string]float64
	_ = json.Unmarshal(v.Config, &configMap)
	stats, err := s.store.VersionStatsFor(ctx, v.ID)
	if err != nil {
		return versionView{}, err
	}
	return versionView{
		ID:              v.ID,
		Kind:            v.Kind,
		Version:         v.Version,
		DisplayName:     fmt.Sprintf("%s_v%d", v.Kind, v.Version),
		Config:          configMap,
		ParentVersionID: v.ParentVersionID,
		Enabled:         v.Enabled,
		Stats:           toVersionStatsView(stats),
	}, nil
}

// handleStats returns every version with its stats — this is the panel's main table (operator's
// explicit "I only want stats, not a positions list").
func (s *service) handleStats(w http.ResponseWriter, r *http.Request) {
	versions, err := s.store.ListVersions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]versionView, 0, len(versions))
	for _, v := range versions {
		vv, err := toVersionView(r.Context(), s, v)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, vv)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *service) handleListVersions(w http.ResponseWriter, r *http.Request) {
	s.handleStats(w, r) // same shape; kept as a distinct route in case the panel needs them to diverge later
}

// handleGetVersion backs the panel's "click a version to see its parent's params vs. this
// version's params" requirement — returns both this version and its parent (if any) in one call.
func (s *service) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.store.GetVersion(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	vv, err := toVersionView(r.Context(), s, v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := struct {
		Version versionView  `json:"version"`
		Parent  *versionView `json:"parent,omitempty"`
	}{Version: vv}

	if v.ParentVersionID != nil {
		parent, err := s.store.GetVersion(r.Context(), *v.ParentVersionID)
		if err == nil {
			pv, err := toVersionView(r.Context(), s, parent)
			if err == nil {
				resp.Parent = &pv
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type createVersionRequest struct {
	Kind   string             `json:"kind"`
	Config map[string]float64 `json:"config"`
}

// handleCreateVersion is how the panel applies a param edit: NEVER overwrites the enabled
// version's row, always inserts kind's next version number and switches trading over to it
// (operator's explicit instruction — "برای هر آپدیت جدید یه ورژن بزن"). Cloned from whichever
// version is currently enabled for kind, so ParentVersionID always points at what was live.
func (s *service) handleCreateVersion(w http.ResponseWriter, r *http.Request) {
	var req createVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Kind == "" {
		writeError(w, http.StatusBadRequest, "kind is required")
		return
	}
	if _, ok := strategy.Factories[req.Kind]; !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown strategy kind %q", req.Kind))
		return
	}

	enabled, err := s.store.EnabledVersions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var parentID int64
	for _, v := range enabled {
		if v.Kind == req.Kind {
			parentID = v.ID
			break
		}
	}
	if parentID == 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("no enabled version found for kind %q to clone from", req.Kind))
		return
	}

	// Validate against the kind's own ParamSpecs so a typo'd param name doesn't silently do
	// nothing (strategy.WithParams ignores unrecognized keys, per its own doc comment).
	live := strategy.Factories[req.Kind]()
	specs := make(map[string]bool, len(live.Params()))
	for _, spec := range live.Params() {
		specs[spec.Name] = true
	}
	for name := range req.Config {
		if !specs[name] {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown param %q for kind %q", name, req.Kind))
			return
		}
	}

	configJSON, err := json.Marshal(req.Config)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, err := s.store.CreateVersion(r.Context(), req.Kind, configJSON, parentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.reloadStrategies(r.Context()); err != nil {
		s.logger.Error("failed to reload strategies after creating version", "error", err)
	}

	v, err := s.store.GetVersion(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	vv, err := toVersionView(r.Context(), s, v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, vv)
}

// handleEnableVersion lets the operator revert to an older version manually.
func (s *service) handleEnableVersion(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SetEnabled(r.Context(), id, true); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.reloadStrategies(r.Context()); err != nil {
		s.logger.Error("failed to reload strategies after enabling version", "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type configView struct {
	Bar         string   `json:"bar"`
	InstIDs     []string `json:"instIds"`
	NotionalUSD string   `json:"notionalUsd"`
	Leverage    string   `json:"leverage"`
}

func (s *service) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, configView{
		Bar:         s.cfg.Tester.Bar,
		InstIDs:     s.cfg.Tester.InstIDs,
		NotionalUSD: s.cfg.Tester.NotionalUSD.String(),
		Leverage:    s.cfg.Tester.Leverage.String(),
	})
}

type saveConfigRequest struct {
	Bar         *string `json:"bar"`
	NotionalUSD *string `json:"notionalUsd"`
	Leverage    *string `json:"leverage"`
}

// handleSaveConfig persists the panel's config edits (bar/notional/leverage) to the tester_config
// row. It does NOT restart the service itself — a bar change needs a fresh Kafka subscription and
// candle-window reseed that only a real process restart gives cleanly, so the panel calls
// POST /restart separately once the operator is ready (operator's explicit "and reset the service
// if I want" — a distinct action from saving).
func (s *service) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var req saveConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	rc := tester.RuntimeConfig{Bar: req.Bar}
	if req.NotionalUSD != nil {
		v, err := decimal.NewFromString(*req.NotionalUSD)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid notionalUsd: "+err.Error())
			return
		}
		rc.NotionalUSD = &v
	}
	if req.Leverage != nil {
		v, err := decimal.NewFromString(*req.Leverage)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid leverage: "+err.Error())
			return
		}
		rc.Leverage = &v
	}
	if err := s.store.SaveRuntimeConfig(r.Context(), rc); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "restartRequired": true})
}

// handleRestart exits this process so Docker's restart policy brings it back reading the
// tester_config row just saved. Deliberately the ONLY mechanism: this binary has no Docker socket
// access (cmd/api's own dockerStatus/LogTail calls a `docker` binary that isn't even installed in
// its image, discovered while building this), and self-exit + a container-scoped restart policy
// is what guarantees every OTHER service is completely unaffected — there is no shared command
// path that could accidentally target them.
func (s *service) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("restart requested via panel; exiting for the container's restart policy to relaunch")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
	go func() {
		os.Exit(0)
	}()
}

func parseID(raw string) (int64, error) {
	var id int64
	_, err := fmt.Sscanf(raw, "%d", &id)
	if err != nil {
		return 0, fmt.Errorf("invalid id %q", raw)
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
