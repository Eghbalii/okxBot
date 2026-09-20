package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// newTestServer2 mirrors adjust_position_test.go's newTestServer, distinctly named since it wires
// a different stub repository type.
func newTestServer2(repo *accountStubRepo) *Server {
	return &Server{Repo: repo, Logger: slog.Default()}
}

// accountStubRepo mirrors adjust_position_test.go's stubRepo pattern: embed a nil port.Repository
// so only the methods a test actually exercises need overriding.
type accountStubRepo struct {
	port.Repository
	setCapCalls []struct {
		mode string
		cap  decimal.Decimal
	}
	setCapResult port.AccountEquity
	setCapErr    error

	getAccountResult port.AccountEquity
	getAccountErr    error

	historyQueries []struct {
		mode  string
		since time.Time
		limit int
	}
	historyResult []port.EquityPoint
	// tradingCapCalls logs SetTradingCap separately from setCapCalls so a test can tell which of
	// the two operations the handler chose for a given mode.
	tradingCapCalls []struct {
		mode string
		cap  decimal.Decimal
	}
}

// SetTradingCap records into the SAME call log as SetAccountCap but tags which one ran, so a test
// can assert the handler routed real vs. paper to the right operation — the distinction that
// matters here (CLAUDE.md: overwriting AccountBalanceUSD in real mode corrupts the exchange
// reconciliation anchor).
func (r *accountStubRepo) SetTradingCap(ctx context.Context, mode string, capUSD decimal.Decimal) (port.AccountEquity, error) {
	r.tradingCapCalls = append(r.tradingCapCalls, struct {
		mode string
		cap  decimal.Decimal
	}{mode, capUSD})
	if r.setCapErr != nil {
		return port.AccountEquity{}, r.setCapErr
	}
	return r.setCapResult, nil
}

func (r *accountStubRepo) SetAccountCap(ctx context.Context, mode string, newCapUSD decimal.Decimal) (port.AccountEquity, error) {
	r.setCapCalls = append(r.setCapCalls, struct {
		mode string
		cap  decimal.Decimal
	}{mode, newCapUSD})
	if r.setCapErr != nil {
		return port.AccountEquity{}, r.setCapErr
	}
	return r.setCapResult, nil
}

func (r *accountStubRepo) GetAccountEquity(ctx context.Context, mode string, initialUSD decimal.Decimal) (port.AccountEquity, error) {
	if r.getAccountErr != nil {
		return port.AccountEquity{}, r.getAccountErr
	}
	return r.getAccountResult, nil
}

func (r *accountStubRepo) ListEquityHistory(ctx context.Context, mode string, since time.Time, limit int) ([]port.EquityPoint, error) {
	r.historyQueries = append(r.historyQueries, struct {
		mode  string
		since time.Time
		limit int
	}{mode, since, limit})
	return r.historyResult, nil
}

func doSetAccountCap(srv *Server, body map[string]any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/account/cap", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.handleSetAccountCap(rec, req)
	return rec
}

func TestHandleSetAccountCap_DefaultsToPaperMode(t *testing.T) {
	repo := &accountStubRepo{setCapResult: port.AccountEquity{Mode: "paper", EquityUSD: dec("40")}}
	srv := newTestServer2(repo)

	rec := doSetAccountCap(srv, map[string]any{"newCapUsd": 40})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.setCapCalls) != 1 {
		t.Fatalf("expected exactly 1 SetAccountCap call, got %d", len(repo.setCapCalls))
	}
	if repo.setCapCalls[0].mode != "paper" {
		t.Errorf("expected mode to default to paper, got %q", repo.setCapCalls[0].mode)
	}
	if !repo.setCapCalls[0].cap.Equal(dec("40")) {
		t.Errorf("expected newCapUsd 40 passed through, got %s", repo.setCapCalls[0].cap)
	}
}

// A cap of exactly 0 is a legitimate, explicit choice ("give this mode nothing of the shared
// balance"), not an error — was rejected until 2026-09-20, when an operator correctly pointed out
// that reducing a mode's cap back to 0 is a real action, not invalid input. Only a NEGATIVE
// request is rejected now.
func TestHandleSetAccountCap_AcceptsZeroCap(t *testing.T) {
	repo := &accountStubRepo{}
	srv := newTestServer2(repo)
	rec := doSetAccountCap(srv, map[string]any{"newCapUsd": 0})
	if rec.Code != 200 {
		t.Fatalf("cap=0: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.setCapCalls) != 1 {
		t.Fatalf("cap=0: expected exactly one repository call, got %d", len(repo.setCapCalls))
	}
}

func TestHandleSetAccountCap_RejectsNegativeCap(t *testing.T) {
	repo := &accountStubRepo{}
	srv := newTestServer2(repo)
	rec := doSetAccountCap(srv, map[string]any{"newCapUsd": -5})
	if rec.Code != 400 {
		t.Errorf("cap=-5: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.setCapCalls) != 0 {
		t.Errorf("cap=-5: expected no repository call for a rejected request")
	}
}

func TestHandleSetAccountCap_RejectsInvalidMode(t *testing.T) {
	repo := &accountStubRepo{}
	srv := newTestServer2(repo)
	rec := doSetAccountCap(srv, map[string]any{"mode": "bogus", "newCapUsd": 40})
	if rec.Code != 400 {
		t.Fatalf("expected 400 for an invalid mode, got %d: %s", rec.Code, rec.Body.String())
	}
}

// CLAUDE.md §31.2: real money is explicitly allowed here, unlike ApplyRealizedPnL's automatic
// reset — an operator choosing their own real trading cap is a human decision, not the kind of
// automatic top-up §15.7's real-mode carve-out exists to prevent.
//
// Real mode routes to SetTradingCap, NOT SetAccountCap (2026-09-08). This is the routing that
// keeps AccountBalanceUSD — RecordExchangeBalance's reconciliation anchor against the exchange's
// own reported balance — from being overwritten with an operator-chosen number, which would make
// the very next poll record the difference as realized PnL that never happened.
func TestHandleSetAccountCap_BotModeRoutesToSetTradingCap(t *testing.T) {
	repo := &accountStubRepo{setCapResult: port.AccountEquity{Mode: "bot", EquityUSD: dec("500")}}
	srv := newTestServer2(repo)

	rec := doSetAccountCap(srv, map[string]any{"mode": "bot", "newCapUsd": 500})
	if rec.Code != 200 {
		t.Fatalf("expected 200 for mode=bot, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.tradingCapCalls) != 1 || repo.tradingCapCalls[0].mode != "bot" {
		t.Fatalf("expected one SetTradingCap call for mode=bot, got %+v", repo.tradingCapCalls)
	}
	if len(repo.setCapCalls) != 0 {
		t.Fatalf("real mode must NOT call SetAccountCap (it would overwrite the exchange balance anchor), got %+v", repo.setCapCalls)
	}
}

// manual mode shares bot's routing: it too mirrors a real exchange balance (the SAME real balance
// bot draws from, Repository.SetTradingCap's own doc comment), so overwriting AccountBalanceUSD
// with a chosen number would be exactly as wrong here as it would for bot (2026-09-20, Account
// page). This is the regression this test guards: manual defaulting to SetAccountCap (the "else"
// branch before manual was added to realMoneyModes) would corrupt the reconciliation anchor the
// same way it would have for bot before that routing existed.
func TestHandleSetAccountCap_ManualModeRoutesToSetTradingCap(t *testing.T) {
	repo := &accountStubRepo{setCapResult: port.AccountEquity{Mode: "manual", EquityUSD: dec("20")}}
	srv := newTestServer2(repo)

	rec := doSetAccountCap(srv, map[string]any{"mode": "manual", "newCapUsd": 20})
	if rec.Code != 200 {
		t.Fatalf("expected 200 for mode=manual, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.tradingCapCalls) != 1 || repo.tradingCapCalls[0].mode != "manual" {
		t.Fatalf("expected one SetTradingCap call for mode=manual, got %+v", repo.tradingCapCalls)
	}
	if len(repo.setCapCalls) != 0 {
		t.Fatalf("manual mode must NOT call SetAccountCap (it would overwrite the exchange balance anchor), got %+v", repo.setCapCalls)
	}
}

// The paper side of the same routing: paper has no exchange, so AccountBalanceUSD is bookkeeping
// this system owns and a cap legitimately re-baselines the whole account (CLAUDE.md §32.3).
func TestHandleSetAccountCap_PaperModeRoutesToSetAccountCap(t *testing.T) {
	repo := &accountStubRepo{setCapResult: port.AccountEquity{Mode: "paper", EquityUSD: dec("40")}}
	srv := newTestServer2(repo)

	rec := doSetAccountCap(srv, map[string]any{"mode": "paper", "newCapUsd": 40})
	if rec.Code != 200 {
		t.Fatalf("expected 200 for mode=paper, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.setCapCalls) != 1 || repo.setCapCalls[0].mode != "paper" {
		t.Fatalf("expected one SetAccountCap call for mode=paper, got %+v", repo.setCapCalls)
	}
	if len(repo.tradingCapCalls) != 0 {
		t.Fatalf("paper mode must not call SetTradingCap, got %+v", repo.tradingCapCalls)
	}
}

// CLAUDE.md §31.2: with no explicit ?since=, the chart must show only the story since the account's
// last reset/cap choice — not its entire lifetime, which may span sizing regimes with nothing to do
// with the currently-chosen baseline.
func TestHandleAccountHistory_DefaultsSinceToLastResetAt(t *testing.T) {
	resetAt := time.Date(2026, 9, 1, 11, 40, 0, 0, time.UTC)
	repo := &accountStubRepo{
		getAccountResult: port.AccountEquity{Mode: "paper", LastResetAt: &resetAt},
	}
	srv := newTestServer2(repo)

	req := httptest.NewRequest("GET", "/api/account/history", nil)
	rec := httptest.NewRecorder()
	srv.handleAccountHistory(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.historyQueries) != 1 {
		t.Fatalf("expected exactly 1 ListEquityHistory call, got %d", len(repo.historyQueries))
	}
	if !repo.historyQueries[0].since.Equal(resetAt) {
		t.Errorf("expected since to default to LastResetAt (%s), got %s", resetAt, repo.historyQueries[0].since)
	}
}

// An explicit ?since= must still win over LastResetAt, for a caller that genuinely wants a wider
// (or narrower) window than "since the last reset".
func TestHandleAccountHistory_ExplicitSinceOverridesLastResetAt(t *testing.T) {
	resetAt := time.Date(2026, 9, 1, 11, 40, 0, 0, time.UTC)
	explicit := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo := &accountStubRepo{
		getAccountResult: port.AccountEquity{Mode: "paper", LastResetAt: &resetAt},
	}
	srv := newTestServer2(repo)

	req := httptest.NewRequest("GET", "/api/account/history?since="+explicit.Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	srv.handleAccountHistory(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !repo.historyQueries[0].since.Equal(explicit) {
		t.Errorf("expected the explicit since to win, got %s", repo.historyQueries[0].since)
	}
}

// A mode that has never been reset (LastResetAt nil) must not filter the chart down to nothing —
// the whole history is the correct "since the last reset" answer when there has never been one.
func TestHandleAccountHistory_NoResetYetShowsFullHistory(t *testing.T) {
	repo := &accountStubRepo{
		getAccountResult: port.AccountEquity{Mode: "paper", LastResetAt: nil},
	}
	srv := newTestServer2(repo)

	req := httptest.NewRequest("GET", "/api/account/history", nil)
	rec := httptest.NewRecorder()
	srv.handleAccountHistory(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !repo.historyQueries[0].since.IsZero() {
		t.Errorf("expected a zero since (no lower bound) when the account has never reset, got %s", repo.historyQueries[0].since)
	}
}
