package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// tokenStatsStubRepo mirrors stubRepo's nil-embed pattern (adjust_position_test.go).
type tokenStatsStubRepo struct {
	port.Repository
	stats []port.TokenStats
	err   error
}

func (s *tokenStatsStubRepo) TokenStats24h(ctx context.Context, mode string) ([]port.TokenStats, error) {
	return s.stats, s.err
}

func TestHandleTokenStats24h_ReturnsPerTokenRows(t *testing.T) {
	repo := &tokenStatsStubRepo{stats: []port.TokenStats{
		{InstID: "BTC", PositionCount: 5, PnLUSD: decimal.RequireFromString("1.5"), PnLPct: decimal.RequireFromString("3.75")},
		{InstID: "ETH", PositionCount: 2, PnLUSD: decimal.RequireFromString("-0.4"), PnLPct: decimal.RequireFromString("-2.1")},
	}}
	srv := &Server{Repo: repo, Logger: slog.Default()}

	req := httptest.NewRequest("GET", "/api/paper-trading/token-stats", nil)
	rec := httptest.NewRecorder()
	srv.handleTokenStats24h(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []tokenStatsView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	if got[0].InstID != "BTC" || got[0].PositionCount != 5 {
		t.Errorf("unexpected first row: %+v", got[0])
	}
	if got[0].PnLUSD != "1.5" || got[0].PnLPct != "3.75" {
		t.Errorf("unexpected PnL fields: %+v", got[0])
	}
}

func TestHandleTokenStats24h_EmptyReturnsEmptyArray(t *testing.T) {
	repo := &tokenStatsStubRepo{stats: nil}
	srv := &Server{Repo: repo, Logger: slog.Default()}

	req := httptest.NewRequest("GET", "/api/paper-trading/token-stats", nil)
	rec := httptest.NewRecorder()
	srv.handleTokenStats24h(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if body != "[]\n" && body != "[]" {
		t.Errorf("expected an empty JSON array, got %q", body)
	}
}

func TestHandleTokenStats24h_RepoErrorReturns500(t *testing.T) {
	repo := &tokenStatsStubRepo{err: context.DeadlineExceeded}
	srv := &Server{Repo: repo, Logger: slog.Default()}

	req := httptest.NewRequest("GET", "/api/paper-trading/token-stats", nil)
	rec := httptest.NewRecorder()
	srv.handleTokenStats24h(rec, req)

	if rec.Code != 500 {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}
