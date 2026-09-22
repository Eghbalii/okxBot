package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// This file backs the Home page (2026-09-13 request): per-exchange balances at the top, then the
// ranked token market the operator can sort and click through to a chart, plus the instrument-roster
// controls that decide which of those tokens the bot actually trades.

// marketScanner is the discovery job, narrowed to what this handler needs. An interface rather than
// the concrete *usecase.MarketScanner so cmd/api's handlers stay testable without a scanner (and so
// a nil one disables the manual-scan route with a clear error rather than a panic).
type marketScanner interface {
	Scan(ctx context.Context) []ScanResultView
}

// ScanResultView mirrors usecase.ScanResult's fields. Declared here rather than imported so this
// package does not depend on the scanner's concrete type just to describe its output; cmd/api's
// wiring converts between the two.
type ScanResultView struct {
	Exchange   string
	Scanned    int
	Candidates int
	Admitted   int
	Err        error
}

// balanceReader is the one exchange capability the Home page's balance row needs. Narrow
// deliberately, the same reasoning as positionLister (§48): a page that displays balances must not
// be able to place an order, and the type system is a better guarantee of that than care.
type balanceReader interface {
	GetBalance(ccy string) ([]domain.Balance, error)
}

// ExchangeBalanceSource is one exchange whose balance the Home page shows. Configured with a nil
// Client for an exchange whose credentials this deployment does not hold — MEXC's authenticated half
// has never run against a real account (§46.6) — so the row renders as "not configured" rather than
// as an error or, worse, a plausible-looking zero.
type ExchangeBalanceSource struct {
	Name   string
	Client balanceReader
	// Ccy is the settlement currency to report ("USDT" on OKX, "USDT" on MEXC).
	Ccy string
}

type exchangeBalanceView struct {
	Exchange string `json:"exchange"`
	// Configured is false when this deployment holds no credentials for the exchange. The panel
	// shows that state explicitly: an unconfigured exchange and one reporting a zero balance mean
	// very different things, and collapsing them would hide a missing key behind a real-looking
	// number.
	Configured bool   `json:"configured"`
	Ccy        string `json:"ccy"`
	EquityUSD  string `json:"equityUsd"`
	AvailUSD   string `json:"availUsd"`
	// Err is the exchange's own failure, reported per-exchange so one unreachable exchange does not
	// blank the whole row (the same partial-failure posture as the scan itself).
	Err string `json:"err,omitempty"`
}

// handleExchangeBalances reports every configured exchange's account balance.
func (s *Server) handleExchangeBalances(w http.ResponseWriter, _ *http.Request) {
	out := make([]exchangeBalanceView, 0, len(s.ExchangeBalances))
	for _, ex := range s.ExchangeBalances {
		v := exchangeBalanceView{Exchange: ex.Name, Ccy: ex.Ccy, EquityUSD: "0", AvailUSD: "0"}
		if ex.Client == nil {
			out = append(out, v)
			continue
		}
		v.Configured = true
		balances, err := ex.Client.GetBalance(ex.Ccy)
		if err != nil {
			v.Err = err.Error()
			out = append(out, v)
			continue
		}
		for _, b := range balances {
			v.EquityUSD = b.Eq.String()
			v.AvailUSD = b.AvailEq.String()
			break
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

type marketTokenView struct {
	Exchange     string `json:"exchange"`
	Symbol       string `json:"symbol"`
	ExecInstID   string `json:"execInstId"`
	LastPx       string `json:"lastPx"`
	High24h      string `json:"high24h"`
	Low24h       string `json:"low24h"`
	Vol24hUSD    string `json:"vol24hUsd"`
	Change24hPct string `json:"change24hPct"`
	Range24hPct  string `json:"range24hPct"`
	Score        string `json:"score"`
	ScannedAt    string `json:"scannedAt"`
	// InRoster/EnabledPaper/EnabledReal say whether this token is actually being traded, so the
	// Home page can show the market and the working set in one table rather than making the operator
	// cross-reference two.
	InRoster     bool `json:"inRoster"`
	EnabledPaper bool `json:"enabledPaper"`
	EnabledReal  bool `json:"enabledReal"`
}

// handleListMarketTokens serves the ranked market snapshot the Home page sorts.
//
// One row per (exchange, symbol) as stored, NOT merged across exchanges: the same token's volume,
// price and tick size genuinely differ per venue (§33.2 measured 29x-144x volume differences between
// OKX's SWAP and X-Perp markets for the same tokens), so merging would average away the one property
// that decides whether the bot can trade it. The panel groups by symbol for display and shows which
// exchanges carry it.
func (s *Server) handleListMarketTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}

	toks, err := s.Repo.ListMarketTokens(ctx, r.URL.Query().Get("exchange"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// One roster read for the whole response rather than a lookup per token — the roster is tens of
	// rows and the market is over a thousand.
	roster, err := s.Repo.ListInstruments(ctx, port.InstrumentFilter{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type key struct{ exchange, symbol string }
	inRoster := make(map[key]port.Instrument, len(roster))
	for _, in := range roster {
		inRoster[key{in.Exchange, in.Symbol}] = in
	}

	out := make([]marketTokenView, 0, len(toks))
	for _, t := range toks {
		v := marketTokenView{
			Exchange: t.Exchange, Symbol: t.Symbol, ExecInstID: t.ExecInstID,
			LastPx: t.LastPx.String(), High24h: t.High24h.String(), Low24h: t.Low24h.String(),
			Vol24hUSD: t.Vol24hUSD.String(), Change24hPct: t.Change24hPct.String(),
			Range24hPct: t.Range24hPct.String(), Score: t.Score.String(),
			ScannedAt: t.ScannedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		if in, ok := inRoster[key{t.Exchange, t.Symbol}]; ok {
			v.InRoster, v.EnabledPaper, v.EnabledReal = true, in.EnabledPaper, in.EnabledReal
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

type instrumentView struct {
	ID            int64  `json:"id"`
	Symbol        string `json:"symbol"`
	Exchange      string `json:"exchange"`
	ExecInstID    string `json:"execInstId"`
	InstType      string `json:"instType"`
	EnabledIngest bool   `json:"enabledIngest"`
	EnabledPaper  bool   `json:"enabledPaper"`
	EnabledReal   bool   `json:"enabledReal"`
	Source        string `json:"source"`
	Vol24hUSD     string `json:"vol24hUsd"`
	Change24hPct  string `json:"change24hPct"`
	ScanScore     string `json:"scanScore"`
	UpdatedAt     string `json:"updatedAt"`
	// Active is the per-token enable/disable checkbox state (paper_trading_config.
	// disabled_inst_ids, §22's Manage Tokens toggle), and ONLY that — corrected 2026-09-22 per
	// explicit operator instruction, after a same-day first version additionally required a live
	// enabled strategy_assignments row and broke bot/real mode entirely (that mode currently has
	// zero assignments at all, so every token there read active=false regardless of the toggle —
	// worse than the original bug, which at least tracked the checkbox correctly in paper mode
	// where assignments already exist). Active answers "did the operator turn this token on",
	// nothing about whether a strategy happens to be assigned to it yet — that is a separate,
	// real fact HasAssignment answers instead.
	Active bool `json:"active"`
	// HasAssignment is true when this symbol has at least one ENABLED strategy_assignments row for
	// activeMode (2026-09-17 request, originally named Active before the 2026-09-22 split above) —
	// deliberately NOT the same thing as EnabledPaper/EnabledReal. A token can be enabled_paper=true
	// and still never trade: MEXC tokens are enabled_paper on admission (the scan's own §53.1 rule)
	// but paper-trader's roster load is hardcoded to the "okx" exchange only (no MEXC execution
	// wiring yet, §46.6) — those rows are real and selectable, they just have no assignment. This
	// is what the panel's "Trading" badge reads.
	HasAssignment bool `json:"hasAssignment"`
}

// instrumentListResponse pages the roster (2026-09-17): the discovery scan keeps growing it on its
// own, and returning every row unpaginated stopped being reasonable once it passed the ~10-token
// config-file era this endpoint was first written for. Total is the filtered count BEFORE paging,
// so the panel can render "page X of Y" rather than only "here are up to Limit rows".
type instrumentListResponse struct {
	Items []instrumentView `json:"items"`
	Total int              `json:"total"`
}

// handleListInstruments serves the roster — the full set of tokens SELECTABLE for trading, not
// only the ones currently active (see instrumentView.Active's own doc comment for why those two
// differ). limit/offset are optional query params; omitting both returns every matching row
// unpaginated, which is what every trading service's own startup load needs (they want the full
// working set, never a page of it) — only the panel passes them.
func (s *Server) handleListInstruments(w http.ResponseWriter, r *http.Request) {
	f := port.InstrumentFilter{
		Exchange: r.URL.Query().Get("exchange"),
		Enabled:  r.URL.Query().Get("enabled"),
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		f.Limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		f.Offset = n
	}
	// activeMode decides which assignment set Active is computed against — defaults to "paper"
	// since that's this endpoint's only caller today (Manage Tokens), matching every other
	// paper-vs-real-scoped endpoint's own default in this file.
	activeMode := r.URL.Query().Get("mode")
	if activeMode == "" {
		activeMode = "paper"
	}

	roster, err := s.Repo.ListInstruments(r.Context(), f)
	if err != nil {
		// An unknown enabled filter is the caller's mistake, not a server fault — ListInstruments
		// rejects it rather than silently returning everything, which would look like success.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Active is the per-token enable/disable checkbox state ALONE (paper_trading_config.
	// disabled_inst_ids, §22's Manage Tokens toggle) — corrected 2026-09-22, same day as the
	// first version of this fix. That first version additionally required a live enabled
	// strategy_assignments row, reasoning that a disabled-but-still-assigned token wasn't
	// "really" active — true for paper (which has assignments), but wrong for bot/real mode,
	// which currently has ZERO assignments at all (nothing has been assigned there yet, §34/§47).
	// Under that AND, every bot-mode token read active=0 regardless of the toggle, which is a
	// worse version of the exact bug being fixed: the filter still didn't reflect the checkbox.
	// The operator's own instruction: Active must track the checkbox, full stop — whether a
	// strategy happens to be assigned is a separate question the "Trading" column already answers
	// (instrumentView has no equivalent field for that split yet, see below).
	ptCfg, err := s.Repo.GetPaperTradingConfig(r.Context(), activeMode, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	disabledSymbols := make(map[string]bool, len(ptCfg.DisabledInstIDs))
	for _, id := range ptCfg.DisabledInstIDs {
		disabledSymbols[id] = true
	}
	// hasAssignment still backs the "Trading" column (instrumentView doc comment above) — whether
	// a strategy is actually assigned is a real, separate fact the panel also displays, just not
	// the same fact as Active any more.
	assignments, err := s.Repo.ListAssignments(r.Context(), "", true, activeMode, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hasAssignment := make(map[string]bool, len(assignments))
	for _, a := range assignments {
		hasAssignment[a.InstID] = true
	}

	out := make([]instrumentView, 0, len(roster))
	for _, in := range roster {
		out = append(out, instrumentView{
			ID: in.ID, Symbol: in.Symbol, Exchange: in.Exchange, ExecInstID: in.ExecInstID,
			InstType: in.InstType, EnabledIngest: in.EnabledIngest, EnabledPaper: in.EnabledPaper,
			EnabledReal: in.EnabledReal, Source: in.Source,
			Vol24hUSD: in.Vol24hUSD.String(), Change24hPct: in.Change24hPct.String(),
			ScanScore:     in.ScanScore.String(),
			UpdatedAt:     in.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			Active:        !disabledSymbols[in.Symbol],
			HasAssignment: hasAssignment[in.Symbol],
		})
	}

	// Total is only meaningful (and only worth a second query) when the caller actually paginated —
	// an unpaginated request already returns every matching row, so len(out) already is the total.
	total := len(out)
	if f.Limit > 0 {
		total, err = s.Repo.CountInstruments(r.Context(), f)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, instrumentListResponse{Items: out, Total: total})
}

// handleCreateInstrument adds a token to the roster by hand — the operator's own path for a token
// the scan has not surfaced, or has ranked below its TopN cut.
//
// Real mode defaults OFF regardless of what the caller sends only when the field is omitted; an
// explicit true is honored, because this route IS the operator acting deliberately. That is the
// same trust boundary as the manual SL/TP edit (§27.7 commit 6): a person acting directly is
// trusted, a scan is not.
func (s *Server) handleCreateInstrument(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Symbol        string `json:"symbol"`
		Exchange      string `json:"exchange"`
		ExecInstID    string `json:"execInstId"`
		InstType      string `json:"instType"`
		EnabledIngest *bool  `json:"enabledIngest"`
		EnabledPaper  *bool  `json:"enabledPaper"`
		EnabledReal   *bool  `json:"enabledReal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if body.Symbol == "" || body.Exchange == "" || body.ExecInstID == "" {
		writeError(w, http.StatusBadRequest, "symbol, exchange and execInstId are required")
		return
	}

	in := port.Instrument{
		Symbol: body.Symbol, Exchange: body.Exchange, ExecInstID: body.ExecInstID,
		InstType: body.InstType, Source: "manual",
		EnabledIngest: boolOr(body.EnabledIngest, true),
		EnabledPaper:  boolOr(body.EnabledPaper, true),
		EnabledReal:   boolOr(body.EnabledReal, false),
	}
	out, wasNew, err := s.Repo.UpsertInstrument(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Same top-up a scan-discovered token gets (usecase.MarketScanner.topUpForNewTokens) — one
	// operator decision to add a token should have one consistent consequence regardless of which of
	// the two paths added it. Only for a genuinely new row and only when paper trading is enabled on
	// it: re-adding an already-known token, or adding one with paper trading off, must not grow the
	// account. Best-effort, matching the scanner's own posture — a failed top-up must not make an
	// otherwise-successful instrument creation look like it failed.
	if wasNew && out.EnabledPaper && s.PerTokenCapUSD.IsPositive() {
		if _, err := s.Repo.AdjustAccountCap(r.Context(), "paper", s.PerTokenCapUSD); err != nil {
			s.Logger.Warn("manual instrument add: could not top up paper account",
				"symbol", out.Symbol, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, instrumentView{
		ID: out.ID, Symbol: out.Symbol, Exchange: out.Exchange, ExecInstID: out.ExecInstID,
		InstType: out.InstType, EnabledIngest: out.EnabledIngest, EnabledPaper: out.EnabledPaper,
		EnabledReal: out.EnabledReal, Source: out.Source,
		Vol24hUSD: out.Vol24hUSD.String(), Change24hPct: out.Change24hPct.String(),
		ScanScore: out.ScanScore.String(),
		UpdatedAt: out.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// handleSetInstrumentFlags toggles one roster row's per-mode flags. This is where a person enables a
// discovered token for real money — deliberately its own explicit action, never a side effect of a
// scan (2026-09-13 instruction).
func (s *Server) handleSetInstrumentFlags(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid instrument id")
		return
	}
	var body struct {
		EnabledIngest *bool   `json:"enabledIngest"`
		EnabledPaper  *bool   `json:"enabledPaper"`
		EnabledReal   *bool   `json:"enabledReal"`
		ExecInstID    *string `json:"execInstId"`
		InstType      *string `json:"instType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if body.EnabledIngest == nil && body.EnabledPaper == nil && body.EnabledReal == nil &&
		body.ExecInstID == nil && body.InstType == nil {
		// An empty patch would report success having changed nothing, which reads as a working
		// control that does not work — the §36 failure shape.
		writeError(w, http.StatusBadRequest, "no fields to update")
		return
	}

	patch := port.InstrumentPatch{
		EnabledIngest: body.EnabledIngest, EnabledPaper: body.EnabledPaper,
		EnabledReal: body.EnabledReal, ExecInstID: body.ExecInstID, InstType: body.InstType,
	}
	if err := s.Repo.SetInstrumentFlags(r.Context(), id, patch); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleDeleteInstrument(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid instrument id")
		return
	}
	if err := s.Repo.DeleteInstrument(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleRunScan triggers a discovery scan on demand, alongside the scheduled one. Synchronous: a
// scan is two REST calls and finishes in seconds, and a fire-and-forget job would need its own
// status endpoint to answer the only question anyone asks afterwards ("did it work?") — the same
// call §17 made for the candle backfill.
func (s *Server) handleRunScan(w http.ResponseWriter, r *http.Request) {
	if s.Scanner == nil {
		writeError(w, http.StatusServiceUnavailable, "no market scanner configured on this service")
		return
	}
	results := s.Scanner.Scan(r.Context())

	type view struct {
		Exchange   string `json:"exchange"`
		Scanned    int    `json:"scanned"`
		Candidates int    `json:"candidates"`
		Admitted   int    `json:"admitted"`
		Err        string `json:"err,omitempty"`
	}
	out := make([]view, 0, len(results))
	for _, res := range results {
		v := view{Exchange: res.Exchange, Scanned: res.Scanned, Candidates: res.Candidates, Admitted: res.Admitted}
		if res.Err != nil {
			v.Err = res.Err.Error()
		}
		out = append(out, v)
	}
	// 200 even when an individual exchange failed: partial failure is an expected outcome reported
	// per exchange, not a request-level error (§17's BackfillResult precedent). The panel reads each
	// row's own err.
	writeJSON(w, http.StatusOK, out)
}
