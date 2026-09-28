package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/optimizer"
)

// handleRunNow triggers one immediate tick across every lineage, out of band from the scheduled
// interval — the panel's "Run now" button. Runs synchronously and returns once the pass
// completes, matching cmd/strategy-tester's own POST /restart precedent of a simple, blocking
// action rather than a background job with its own status endpoint.
func (s *scheduler) handleRunNow(w http.ResponseWriter, r *http.Request) {
	s.tickAll(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// candidateView is the panel-facing JSON shape for one strategy_candidates row — flattens the
// Go struct's pointer fields into either a present value or an absent one, and adds the computed
// DisplayName so the panel never has to reimplement the naming convention client-side.
type candidateView struct {
	ID          int64  `json:"id"`
	Kind        string `json:"kind"`
	InstID      string `json:"instId"`
	Bar         string `json:"bar"`
	Exchange    string `json:"exchange"`
	RiskProfile string `json:"riskProfile"`
	Leverage    int    `json:"leverage"`
	DisplayName string `json:"displayName"`
	Status      string `json:"status"`
	Source      string `json:"source"`

	Generation      int `json:"generation"`
	BacktestUpdates int `json:"backtestUpdates"`
	PaperUpdates    int `json:"paperUpdates"`

	BacktestTradeCount   *int             `json:"backtestTradeCount,omitempty"`
	BacktestWinRatePct   *decimal.Decimal `json:"backtestWinRatePct,omitempty"`
	BacktestRealizedPnL  *decimal.Decimal `json:"backtestRealizedPnl,omitempty"`
	BacktestResets       *int             `json:"backtestResets,omitempty"`
	BacktestSignificance *decimal.Decimal `json:"backtestSignificanceT,omitempty"`
	BacktestRejectReason *string          `json:"backtestRejectReason,omitempty"`
	// The real span of historical candles the backtest replayed, not when the backtest itself ran
	// (that's BacktestRanAt, not exposed here) — answers "over how many days was this PnL earned"
	// (operator's explicit request, 2026-09-28), since a $30 profit over 3 days reads completely
	// differently from the same $30 over 60.
	BacktestFrom *time.Time `json:"backtestFrom,omitempty"`
	BacktestTo   *time.Time `json:"backtestTo,omitempty"`

	StrategyID *int64 `json:"strategyId,omitempty"`
	// Only set for status=paper_replaced — a real, computed comparison against whichever candidate
	// actually replaced this one, built from the two candidates' own recorded stats (never a
	// hardcoded label): a paper_replaced candidate was never "rejected" by validation the way
	// BacktestRejectReason candidates were, it traded and lost to something that scored better.
	ReplacedReason *string `json:"replacedReason,omitempty"`
}

func toCandidateView(c optimizer.Candidate, leverage int) candidateView {
	return candidateView{
		ID: c.ID, Kind: c.Lineage.Kind, InstID: c.Lineage.InstID, Bar: c.Lineage.Bar,
		Exchange: c.Lineage.Exchange, RiskProfile: c.Lineage.RiskProfile, Leverage: leverage,
		DisplayName: c.DisplayName(leverage), Status: c.Status, Source: c.Source,
		Generation: c.Generation, BacktestUpdates: c.BacktestUpdates, PaperUpdates: c.PaperUpdates,
		BacktestTradeCount: c.BacktestTradeCount, BacktestWinRatePct: c.BacktestWinRatePct,
		BacktestRealizedPnL: c.BacktestRealizedPnL, BacktestResets: c.BacktestResets,
		BacktestSignificance: c.BacktestSignificanceT, BacktestRejectReason: c.BacktestRejectReason,
		BacktestFrom: c.BacktestFrom, BacktestTo: c.BacktestTo,
		StrategyID: c.StrategyID,
	}
}

// handleListCandidates returns every candidate for a lineage, given as query params
// (kind, instId, bar, exchange, riskProfile) — all five required, since a partial lineage query
// (e.g. just a kind) could span multiple risk profiles with different leverage/naming, which
// would make DisplayName's leverage argument ambiguous.
func (s *scheduler) handleListCandidates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lin := optimizer.Lineage{
		Kind: q.Get("kind"), InstID: q.Get("instId"), Bar: q.Get("bar"),
		Exchange: q.Get("exchange"), RiskProfile: q.Get("riskProfile"),
	}
	if lin.Kind == "" || lin.InstID == "" || lin.Bar == "" || lin.Exchange == "" || lin.RiskProfile == "" {
		jsonError(w, http.StatusBadRequest, fmt.Errorf("kind, instId, bar, exchange and riskProfile are all required"))
		return
	}

	candidates, err := s.store.ListCandidatesForLineage(r.Context(), lin)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}

	profile := s.cfg.StrategyOptimizer.RiskProfiles[lin.RiskProfile]
	leverage := profile.MaxLeverage.IntPart() // cosmetic display value only

	views := make([]candidateView, 0, len(candidates))
	for _, c := range candidates {
		views = append(views, toCandidateView(c, int(leverage)))
	}
	writeJSON(w, views)
}

// handleListByStatus returns up to `limit` candidates across every lineage sharing one status —
// the panel's Active/Backtested/Rejected browser. Unlike handleListCandidates this needs no
// lineage at all: it is answering "what has the pipeline found", not "what happened for this one
// (kind, token, bar)". leverage for DisplayName is ambiguous across risk profiles here (a
// backtest_passed list can span both "low" and "high"), so each row uses its OWN lineage's
// configured leverage rather than one leverage applied to every row.
func (s *scheduler) handleListByStatus(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		jsonError(w, http.StatusBadRequest, fmt.Errorf("status query param is required"))
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			jsonError(w, http.StatusBadRequest, fmt.Errorf("limit must be a positive integer"))
			return
		}
		limit = n
	}

	candidates, err := s.store.ListCandidatesByStatus(r.Context(), status, limit)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}

	views := make([]candidateView, 0, len(candidates))
	for _, c := range candidates {
		profile := s.cfg.StrategyOptimizer.RiskProfiles[c.Lineage.RiskProfile]
		v := toCandidateView(c, int(profile.MaxLeverage.IntPart()))
		if status == "paper_replaced" {
			if replacement, ok, err := s.store.ReplacedByFor(r.Context(), c.ID); err != nil {
				s.logger.Error("failed to look up replacement candidate", "candidateId", c.ID, "error", err)
			} else if ok {
				replacementLeverage := s.cfg.StrategyOptimizer.RiskProfiles[replacement.Lineage.RiskProfile].MaxLeverage.IntPart()
				reason := replacedReason(c, replacement, int(replacementLeverage))
				v.ReplacedReason = &reason
			}
		}
		views = append(views, v)
	}
	writeJSON(w, views)
}

// replacedReason builds a real, computed comparison sentence from two candidates' own recorded
// backtest stats — never a hardcoded label. old is the paper_replaced candidate; newer is whatever
// optimizer.Store.ReplacedByFor found took over its lineage.
func replacedReason(old, newer optimizer.Candidate, newerLeverage int) string {
	oldPnL, newPnL := "?", "?"
	if old.BacktestRealizedPnL != nil {
		oldPnL = old.BacktestRealizedPnL.StringFixed(2)
	}
	if newer.BacktestRealizedPnL != nil {
		newPnL = newer.BacktestRealizedPnL.StringFixed(2)
	}
	oldWin, newWin := "?", "?"
	if old.BacktestWinRatePct != nil {
		oldWin = old.BacktestWinRatePct.StringFixed(1)
	}
	if newer.BacktestWinRatePct != nil {
		newWin = newer.BacktestWinRatePct.StringFixed(1)
	}
	return fmt.Sprintf(
		"Replaced by %s ($%s PnL, %s%% win rate over %d trades) — this one scored $%s PnL, %s%% win rate over %d trades",
		newer.DisplayName(newerLeverage), newPnL, newWin, statTradeCount(newer.BacktestTradeCount),
		oldPnL, oldWin, statTradeCount(old.BacktestTradeCount),
	)
}

func statTradeCount(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// handleBacktestCapital reports the capital every backtest's PnL is actually measured against —
// the operator's explicit question "من از کجا بفهمم با چقدر سرمایه اینقدر سود بدست اومده؟". Every
// backtest in this process shares the same global account.initial_usd/max_position_pct (one
// PositionSlots=1 candidate at a time, per lineage, per scheduler.backtestParams), so this is a
// single config-derived number, not something that needs storing per candidate.
func (s *scheduler) handleBacktestCapital(w http.ResponseWriter, r *http.Request) {
	initial := s.cfg.Account.InitialUSD
	maxPct := s.cfg.Account.MaxPositionPct
	writeJSON(w, map[string]string{
		"initialUsd":         initial.StringFixed(2),
		"maxPositionPct":     maxPct.StringFixed(4),
		"positionCapitalUsd": initial.Mul(maxPct).StringFixed(2),
	})
}

// handlePromote promotes a backtest_passed candidate into production — the operator's explicit
// requirement that promotion be an auditable, deliberate action rather than something the loop
// does silently the instant a backtest passes.
func (s *scheduler) handlePromote(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.store.GetCandidate(r.Context(), id)
	if err != nil {
		jsonError(w, http.StatusNotFound, err)
		return
	}
	profile := s.cfg.StrategyOptimizer.RiskProfiles[c.Lineage.RiskProfile]
	leverage := profile.MaxLeverage.IntPart()

	strategyID, err := optimizer.Promote(r.Context(), s.store, s.repo, id, int(leverage))
	if err != nil {
		jsonError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]int64{"strategyId": strategyID})
}

// handleStatus reports what the scheduler currently knows: the configured lineages and, for
// each, whatever is in-flight.
func (s *scheduler) handleStatus(w http.ResponseWriter, r *http.Request) {
	lineages, err := s.lineages(r.Context())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	type lineageStatus struct {
		optimizer.Lineage
		InFlightCandidateID *int64 `json:"inFlightCandidateId,omitempty"`
	}
	out := make([]lineageStatus, 0, len(lineages))
	for _, lin := range lineages {
		inFlight, err := s.store.GetOptimizerState(r.Context(), lin)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, err)
			return
		}
		out = append(out, lineageStatus{Lineage: lin, InFlightCandidateID: inFlight})
	}
	writeJSON(w, out)
}

// handleGetConfig / handlePutConfig expose one risk profile's panel-editable validation
// thresholds (strategy_optimizer_config) — the operator's explicit "قابل تنظیمش کن که یوزر اگر
// خواست بعدا تغییرش بده". No restart is needed for these to take effect: the scheduler reads
// ValidationConfig fresh from the store on every tick (scheduler.go's tickAll), unlike the
// process-level RiskProfileConfig values (leverage, bars, kinds), which DO need a restart since
// they come from the YAML config file, not the database.
func (s *scheduler) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	riskProfile := r.URL.Query().Get("riskProfile")
	if riskProfile == "" {
		jsonError(w, http.StatusBadRequest, fmt.Errorf("riskProfile query param is required"))
		return
	}
	vc, err := s.store.GetValidationConfig(r.Context(), riskProfile)
	if err != nil {
		jsonError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, vc)
}

func (s *scheduler) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var vc optimizer.ValidationConfig
	if err := json.NewDecoder(r.Body).Decode(&vc); err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	if vc.RiskProfile == "" {
		jsonError(w, http.StatusBadRequest, fmt.Errorf("riskProfile is required"))
		return
	}
	if err := s.store.SaveValidationConfig(r.Context(), vc); err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func parseID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}
