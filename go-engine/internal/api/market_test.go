package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// marketStubRepo follows this package's established stub pattern: embed a nil port.Repository so
// only the methods a test actually exercises need implementing.
type marketStubRepo struct {
	port.Repository
	tokens      []port.MarketToken
	instruments []port.Instrument
	// assignments backs ListAssignments, for the 2026-09-17 Active-column computation in
	// handleListInstruments: a symbol has an enabled row here IFF it currently trades, distinct
	// from EnabledPaper/EnabledReal which a roster row can carry without ever actually trading.
	assignments []port.StrategyAssignment
	// disabledInstIDs backs GetPaperTradingConfig, for the 2026-09-22 fix: a token can have a live
	// enabled assignment AND be per-token disabled (§22) — Active must be false in that case, or
	// the Manage Tokens "Active only" filter does nothing (found: every token read active=true).
	disabledInstIDs []string

	patches        map[int64]port.InstrumentPatch
	upserted       []port.Instrument
	deleted        []int64
	instrumentsErr error
}

func (r *marketStubRepo) ListMarketTokens(context.Context, string, int) ([]port.MarketToken, error) {
	return r.tokens, nil
}

func (r *marketStubRepo) GetPaperTradingConfig(context.Context, string, string) (port.PaperTradingConfig, error) {
	return port.PaperTradingConfig{DisabledInstIDs: r.disabledInstIDs}, nil
}

func (r *marketStubRepo) ListInstruments(_ context.Context, f port.InstrumentFilter) ([]port.Instrument, error) {
	if r.instrumentsErr != nil {
		return nil, r.instrumentsErr
	}
	if f.Enabled == "" {
		return r.instruments, nil
	}
	var out []port.Instrument
	for _, in := range r.instruments {
		switch f.Enabled {
		case "bot":
			if in.EnabledReal {
				out = append(out, in)
			}
		case "paper":
			if in.EnabledPaper {
				out = append(out, in)
			}
		}
	}
	return out, nil
}

func (r *marketStubRepo) UpsertInstrument(_ context.Context, in port.Instrument) (port.Instrument, bool, error) {
	in.ID = int64(len(r.upserted) + 1)
	in.UpdatedAt = time.Now()
	r.upserted = append(r.upserted, in)
	return in, true, nil
}

func (r *marketStubRepo) SetInstrumentFlags(_ context.Context, id int64, p port.InstrumentPatch) error {
	if r.patches == nil {
		r.patches = map[int64]port.InstrumentPatch{}
	}
	r.patches[id] = p
	return nil
}

func (r *marketStubRepo) DeleteInstrument(_ context.Context, id int64) error {
	r.deleted = append(r.deleted, id)
	return nil
}

func (r *marketStubRepo) ListAssignments(_ context.Context, instID string, enabledOnly bool, mode, exchange string) ([]port.StrategyAssignment, error) {
	var out []port.StrategyAssignment
	for _, a := range r.assignments {
		if instID != "" && a.InstID != instID {
			continue
		}
		if enabledOnly && !a.Enabled {
			continue
		}
		if a.Mode != mode {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func marketServer(repo *marketStubRepo) *Server {
	return &Server{Repo: repo, Logger: slog.Default()}
}

func do(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *httptest.ResponseRecorder = httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	srv.Routes().ServeHTTP(r, req)
	return r
}

// TestListMarketTokens_MarksRosterMembership is what lets the Home page show the whole market and
// the working set in one table rather than making the operator cross-reference two views.
func TestListMarketTokens_MarksRosterMembership(t *testing.T) {
	f := decimal.NewFromFloat
	repo := &marketStubRepo{
		tokens: []port.MarketToken{
			{Exchange: "okx", Symbol: "BTC", LastPx: f(77000), Vol24hUSD: f(69e6), Score: f(0.9)},
			{Exchange: "okx", Symbol: "NEW", LastPx: f(1.5), Vol24hUSD: f(5e6), Score: f(0.4)},
		},
		instruments: []port.Instrument{
			{ID: 1, Exchange: "okx", Symbol: "BTC", EnabledPaper: true, EnabledReal: true},
		},
	}

	rec := do(t, marketServer(repo), "GET", "/api/market/tokens", "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got []marketTokenView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tokens, want 2", len(got))
	}
	if !got[0].InRoster || !got[0].EnabledReal {
		t.Errorf("BTC: inRoster=%v enabledReal=%v, want both true", got[0].InRoster, got[0].EnabledReal)
	}
	// A scanned token that is not in the roster must read as not-in-roster rather than defaulting to
	// enabled — the Home page uses this to decide whether to offer an "add" action.
	if got[1].InRoster || got[1].EnabledPaper || got[1].EnabledReal {
		t.Errorf("NEW: %+v — a token outside the roster must not read as enabled", got[1])
	}
}

// A market token is stored per (exchange, symbol) and served that way, NOT merged across exchanges:
// §33.2 measured 29x-144x volume differences between venues for the same token, so merging would
// average away the one property that decides whether the bot can trade it.
func TestListMarketTokens_KeepsExchangesSeparate(t *testing.T) {
	f := decimal.NewFromFloat
	repo := &marketStubRepo{tokens: []port.MarketToken{
		{Exchange: "okx", Symbol: "BTC", LastPx: f(77000), Vol24hUSD: f(69e6), Score: f(0.9)},
		{Exchange: "mexc", Symbol: "BTC", LastPx: f(77037), Vol24hUSD: f(1.9e9), Score: f(0.95)},
	}}
	rec := do(t, marketServer(repo), "GET", "/api/market/tokens", "")
	var got []marketTokenView
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want one per exchange", len(got))
	}
	if got[0].Vol24hUSD == got[1].Vol24hUSD {
		t.Error("the two exchanges' volumes were merged — they must stay distinct")
	}
}

// Enabling a token for real money is its own explicit operator action (2026-09-13 instruction), and
// the patch must carry ONLY the field that was sent — a patch that also cleared ingest would stop
// the token's data collection as a side effect of enabling its trading.
// A token can carry EnabledPaper=true and still never trade — the exact MEXC situation this field
// was built for (2026-09-17 request): the discovery scan admits every token with enabled_paper set
// (CLAUDE.md §53.1), but paper-trader's roster load is hardcoded to the "okx" exchange (no MEXC
// execution wiring yet, §46.6), so a MEXC row is real, selectable, and enabled_paper — and never
// actually opens a position. HasAssignment must reflect that: it is computed from a live, enabled
// strategy_assignments row, never from the enable flags alone.
//
// Renamed from ...ActiveReflectsAssignments... on 2026-09-22, same day: the field this test
// checks stopped being called Active once the operator corrected Active's meaning to the
// per-token checkbox alone (see instrumentView.Active's doc comment) — this test now checks the
// field that kept the original, assignment-based meaning.
func TestHandleListInstruments_HasAssignmentReflectsAssignmentsNotEnabledFlag(t *testing.T) {
	repo := &marketStubRepo{
		instruments: []port.Instrument{
			{ID: 1, Symbol: "BTC", Exchange: "okx", EnabledPaper: true},
			{ID: 2, Symbol: "DOGE_MEXC", Exchange: "mexc", EnabledPaper: true}, // enabled, never assigned
		},
		assignments: []port.StrategyAssignment{
			{ID: 1, StrategyID: 1, InstID: "BTC", Bar: "5m", Enabled: true, Mode: "paper"},
		},
	}
	rec := do(t, marketServer(repo), "GET", "/api/instruments", "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp instrumentListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, in := range resp.Items {
		got[in.Symbol] = in.HasAssignment
	}
	if !got["BTC"] {
		t.Error("BTC has a live assignment and must report hasAssignment=true")
	}
	if got["DOGE_MEXC"] {
		t.Error("DOGE_MEXC is enabledPaper but has no assignment — must report hasAssignment=false, not derived from the flag")
	}
}

// A DISABLED assignment (the panel's per-token/per-timeframe toggle, CLAUDE.md §11.3) must not
// count toward hasAssignment — enabledOnly=true is threaded through to ListAssignments for
// exactly this.
func TestHandleListInstruments_DisabledAssignmentDoesNotCountAsHasAssignment(t *testing.T) {
	repo := &marketStubRepo{
		instruments: []port.Instrument{{ID: 1, Symbol: "ETH", Exchange: "okx", EnabledPaper: true}},
		assignments: []port.StrategyAssignment{
			{ID: 1, StrategyID: 1, InstID: "ETH", Bar: "5m", Enabled: false, Mode: "paper"},
		},
	}
	rec := do(t, marketServer(repo), "GET", "/api/instruments", "")
	var resp instrumentListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Items[0].HasAssignment {
		t.Errorf("ETH's only assignment is disabled — hasAssignment must be false, got %+v", resp.Items)
	}
}

// Regression for the 2026-09-22 bug and its own same-day correction. The FIRST fix made Active
// require BOTH a live assignment AND not-per-token-disabled — which broke bot/real mode entirely,
// since that mode currently has zero assignments at all (nothing has been assigned there yet,
// §34/§47): every bot-mode token read active=false regardless of the checkbox, a worse version of
// the original bug. Per explicit operator correction, Active must track ONLY the per-token
// checkbox (paper_trading_config.disabled_inst_ids) — whether a strategy happens to be assigned
// is the separate HasAssignment fact, checked above.
func TestHandleListInstruments_ActiveIsOnlyThePerTokenCheckbox(t *testing.T) {
	repo := &marketStubRepo{
		instruments: []port.Instrument{
			{ID: 1, Symbol: "ZAMA", Exchange: "okx", EnabledPaper: true}, // disabled, but has an assignment
			{ID: 2, Symbol: "BTC", Exchange: "okx", EnabledPaper: true},  // not disabled, has an assignment
			{ID: 3, Symbol: "SOL", Exchange: "okx", EnabledPaper: true},  // not disabled, NO assignment (bot-mode shape)
		},
		assignments: []port.StrategyAssignment{
			{ID: 1, StrategyID: 1, InstID: "ZAMA", Bar: "5m", Enabled: true, Mode: "paper"},
			{ID: 2, StrategyID: 2, InstID: "BTC", Bar: "5m", Enabled: true, Mode: "paper"},
		},
		disabledInstIDs: []string{"ZAMA"},
	}
	rec := do(t, marketServer(repo), "GET", "/api/instruments", "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp instrumentListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, in := range resp.Items {
		got[in.Symbol] = in.Active
	}
	if got["ZAMA"] {
		t.Error("ZAMA is per-token disabled — must report active=false regardless of its assignment")
	}
	if !got["BTC"] {
		t.Error("BTC is not disabled and has an assignment — must report active=true")
	}
	if !got["SOL"] {
		t.Error("SOL is not disabled but has NO assignment — must still report active=true; " +
			"Active tracks only the checkbox, not whether anything is assigned yet")
	}
}

func TestSetInstrumentFlags_PatchesOnlyWhatWasSent(t *testing.T) {
	repo := &marketStubRepo{}
	rec := do(t, marketServer(repo), "PATCH", "/api/instruments/7", `{"enabledReal":true}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	p, ok := repo.patches[7]
	if !ok {
		t.Fatal("no patch recorded for id 7")
	}
	if p.EnabledReal == nil || !*p.EnabledReal {
		t.Errorf("EnabledReal = %v, want true", p.EnabledReal)
	}
	if p.EnabledIngest != nil || p.EnabledPaper != nil || p.ExecInstID != nil {
		t.Errorf("unsent fields must stay nil, got %+v", p)
	}
}

// An empty patch would report success having changed nothing — a control that looks like it works
// and does not, which is the §36 failure shape this project has already shipped twice.
func TestSetInstrumentFlags_RejectsAnEmptyPatch(t *testing.T) {
	rec := do(t, marketServer(&marketStubRepo{}), "PATCH", "/api/instruments/7", `{}`)
	if rec.Code != 400 {
		t.Errorf("status %d, want 400 for a patch with no fields", rec.Code)
	}
}

// A hand-added token defaults real OFF when the field is omitted, matching the scan's own posture:
// discovery and real-capital exposure stay separate decisions.
func TestCreateInstrument_DefaultsRealOff(t *testing.T) {
	repo := &marketStubRepo{}
	rec := do(t, marketServer(repo), "POST", "/api/instruments",
		`{"symbol":"WIF","exchange":"okx","execInstId":"WIF-USD_UM_XPERP-310404"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(repo.upserted) != 1 {
		t.Fatalf("upserted %d rows", len(repo.upserted))
	}
	got := repo.upserted[0]
	if got.EnabledReal {
		t.Error("enabled_real defaulted true — real-money exposure must be opt-in")
	}
	if !got.EnabledIngest || !got.EnabledPaper {
		t.Error("a manually added token should collect data and paper-trade by default")
	}
	if got.Source != "manual" {
		t.Errorf("source = %q, want manual so a reader can tell it from a scan's work", got.Source)
	}
}

// An explicit true IS honored on the manual route: this endpoint is the operator acting directly,
// the same trust boundary as the manual SL/TP edit (§27.7 commit 6).
func TestCreateInstrument_HonorsAnExplicitRealEnable(t *testing.T) {
	repo := &marketStubRepo{}
	do(t, marketServer(repo), "POST", "/api/instruments",
		`{"symbol":"WIF","exchange":"okx","execInstId":"WIF-X","enabledReal":true}`)
	if len(repo.upserted) != 1 || !repo.upserted[0].EnabledReal {
		t.Error("an explicit enabledReal:true from the operator must be honored")
	}
}

func TestCreateInstrument_RequiresIdentifyingFields(t *testing.T) {
	for _, body := range []string{`{}`, `{"symbol":"WIF"}`, `{"symbol":"WIF","exchange":"okx"}`} {
		if rec := do(t, marketServer(&marketStubRepo{}), "POST", "/api/instruments", body); rec.Code != 400 {
			t.Errorf("body %s: status %d, want 400", body, rec.Code)
		}
	}
}

type stubBalance struct {
	bal []domain.Balance
	err error
}

func (s stubBalance) GetBalance(string) ([]domain.Balance, error) { return s.bal, s.err }

// An exchange with no credentials must read as NOT CONFIGURED, never as a zero balance: a missing
// key and an empty account mean very different things, and collapsing them hides the former behind a
// real-looking number. MEXC's authenticated half has never run against a real account (§46.6), so
// this is the live case, not a hypothetical.
func TestExchangeBalances_DistinguishesUnconfiguredFromZero(t *testing.T) {
	srv := marketServer(&marketStubRepo{})
	srv.ExchangeBalances = []ExchangeBalanceSource{
		{Name: "okx", Ccy: "USDT", Client: stubBalance{bal: []domain.Balance{
			{Ccy: "USDT", Eq: decimal.NewFromFloat(41.25), AvailEq: decimal.NewFromFloat(40)},
		}}},
		{Name: "mexc", Ccy: "USDT"}, // no client — credentials absent
	}

	rec := do(t, srv, "GET", "/api/exchange-balances", "")
	var got []exchangeBalanceView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if !got[0].Configured || got[0].EquityUSD != "41.25" {
		t.Errorf("okx: %+v", got[0])
	}
	if got[1].Configured {
		t.Error("mexc has no client but reported as configured")
	}
}

// One unreachable exchange must report its own error and leave the others intact — the same
// partial-failure posture as the scan and §17's per-instrument backfill results.
func TestExchangeBalances_OneFailureDoesNotBlankTheRow(t *testing.T) {
	srv := marketServer(&marketStubRepo{})
	srv.ExchangeBalances = []ExchangeBalanceSource{
		{Name: "okx", Ccy: "USDT", Client: stubBalance{err: errors.New("gateway down")}},
		{Name: "mexc", Ccy: "USDT", Client: stubBalance{bal: []domain.Balance{
			{Ccy: "USDT", Eq: decimal.NewFromInt(10)},
		}}},
	}
	rec := do(t, srv, "GET", "/api/exchange-balances", "")
	if rec.Code != 200 {
		t.Fatalf("status %d — a single exchange failure must not fail the request", rec.Code)
	}
	var got []exchangeBalanceView
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got[0].Err == "" {
		t.Error("the failing exchange must report its own error")
	}
	if got[1].EquityUSD != "10" {
		t.Errorf("the healthy exchange's balance was lost: %+v", got[1])
	}
}

type stubScanner struct{ results []ScanResultView }

func (s stubScanner) Scan(context.Context) []ScanResultView { return s.results }

// A scan where one exchange failed still returns 200 with per-exchange detail: partial failure is an
// expected outcome reported per exchange, not a request-level error (§17's precedent).
func TestRunScan_ReportsPerExchangeFailure(t *testing.T) {
	srv := marketServer(&marketStubRepo{})
	srv.Scanner = stubScanner{results: []ScanResultView{
		{Exchange: "okx", Scanned: 207, Candidates: 179, Admitted: 20},
		{Exchange: "mexc", Err: errors.New("timeout")},
	}}
	rec := do(t, srv, "POST", "/api/market/scan", "")
	if rec.Code != 200 {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var got []struct {
		Exchange string `json:"exchange"`
		Admitted int    `json:"admitted"`
		Err      string `json:"err"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got) != 2 || got[0].Admitted != 20 || got[1].Err == "" {
		t.Errorf("unexpected results: %+v", got)
	}
}

// With no scanner wired, the route must say so rather than panic — cmd/api has to run anywhere the
// exchange clients are not configured.
func TestRunScan_WithoutAScannerFailsCleanly(t *testing.T) {
	rec := do(t, marketServer(&marketStubRepo{}), "POST", "/api/market/scan", "")
	if rec.Code != 503 {
		t.Errorf("status %d, want 503", rec.Code)
	}
}
