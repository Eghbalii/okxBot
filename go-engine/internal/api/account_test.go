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

func TestHandleSetAccountCap_RejectsNonPositiveCap(t *testing.T) {
	for _, v := range []float64{0, -5} {
		repo := &accountStubRepo{}
		srv := newTestServer2(repo)
		rec := doSetAccountCap(srv, map[string]any{"newCapUsd": v})
		if rec.Code != 400 {
			t.Errorf("cap=%v: expected 400, got %d: %s", v, rec.Code, rec.Body.String())
		}
		if len(repo.setCapCalls) != 0 {
			t.Errorf("cap=%v: expected no repository call for a rejected request", v)
		}
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
func TestHandleSetAccountCap_AllowsRealMode(t *testing.T) {
	repo := &accountStubRepo{setCapResult: port.AccountEquity{Mode: "real", EquityUSD: dec("500")}}
	srv := newTestServer2(repo)

	rec := doSetAccountCap(srv, map[string]any{"mode": "real", "newCapUsd": 500})
	if rec.Code != 200 {
		t.Fatalf("expected 200 for mode=real, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.setCapCalls) != 1 || repo.setCapCalls[0].mode != "real" {
		t.Fatalf("expected one SetAccountCap call for mode=real, got %+v", repo.setCapCalls)
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
