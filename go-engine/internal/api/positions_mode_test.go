package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// positionsStubRepo is a second, narrower stub than stubRepo (adjust_position_test.go) — scoped to
// exactly what handleListPositions/handleClosePosition exercise, keeping each test file's fake
// minimal and focused on the handlers it actually tests.
type positionsStubRepo struct {
	port.Repository
	paperPositions         []port.PaperOrder
	realPositions          []port.RealOrder
	requestManualClose     []int64
	requestRealManualClose []int64
	realManualCloseErr     error
}

func (r *positionsStubRepo) ListPositions(ctx context.Context, f port.PositionFilter) ([]port.PaperOrder, error) {
	return r.paperPositions, nil
}
func (r *positionsStubRepo) CountPositions(ctx context.Context, f port.PositionFilter) (int, error) {
	return len(r.paperPositions), nil
}
func (r *positionsStubRepo) ListRealPositions(ctx context.Context, f port.PositionFilter) ([]port.RealOrder, error) {
	return r.realPositions, nil
}
func (r *positionsStubRepo) CountRealPositions(ctx context.Context, f port.PositionFilter) (int, error) {
	return len(r.realPositions), nil
}
func (r *positionsStubRepo) RequestManualClose(ctx context.Context, id int64) error {
	r.requestManualClose = append(r.requestManualClose, id)
	return nil
}
func (r *positionsStubRepo) RequestRealManualClose(ctx context.Context, id int64) error {
	r.requestRealManualClose = append(r.requestRealManualClose, id)
	return r.realManualCloseErr
}

// TestHandleListPositions_RealModeRoutesToRealTable confirms mode=real hits ListRealPositions, not
// ListPositions, and the response carries the RealOrder's Status field — CLAUDE.md real-trading
// readiness plan, 2026-09-04's core "one unified endpoint, routes at the repository layer" design.
func TestHandleListPositions_RealModeRoutesToRealTable(t *testing.T) {
	repo := &positionsStubRepo{
		paperPositions: []port.PaperOrder{{ID: 1, InstID: "paper-should-not-appear"}},
		realPositions:  []port.RealOrder{{ID: 2, InstID: "BTC", Status: "pending", Side: "buy"}},
	}
	srv := &Server{Repo: repo}

	req := httptest.NewRequest("GET", "/api/positions?mode=real", nil)
	rec := httptest.NewRecorder()
	srv.handleListPositions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ID":2`) {
		t.Errorf("expected the real order (id=2) in the response, got %s", body)
	}
	if strings.Contains(body, `"ID":1`) {
		t.Errorf("expected the paper order to NOT appear for mode=real, got %s", body)
	}
	if !strings.Contains(body, `"Status":"pending"`) {
		t.Errorf("expected Status=pending to be present in the mapped response, got %s", body)
	}
}

// TestHandleListPositions_PaperModeRoutesToPaperTable confirms the reverse: mode=paper (or the
// default) still hits the original ListPositions path, unaffected by the real_orders addition.
func TestHandleListPositions_PaperModeRoutesToPaperTable(t *testing.T) {
	repo := &positionsStubRepo{
		paperPositions: []port.PaperOrder{{ID: 1, InstID: "BTC"}},
		realPositions:  []port.RealOrder{{ID: 2, InstID: "real-should-not-appear"}},
	}
	srv := &Server{Repo: repo}

	req := httptest.NewRequest("GET", "/api/positions?mode=paper", nil)
	rec := httptest.NewRecorder()
	srv.handleListPositions(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ID":1`) {
		t.Errorf("expected the paper order (id=1) in the response, got %s", body)
	}
	if strings.Contains(body, `"ID":2`) {
		t.Errorf("expected the real order to NOT appear for mode=paper, got %s", body)
	}
}

// TestHandleListPositions_InvalidModeRejected confirms a typo'd mode 400s instead of silently
// returning zero rows.
func TestHandleListPositions_InvalidModeRejected(t *testing.T) {
	srv := &Server{Repo: &positionsStubRepo{}}
	req := httptest.NewRequest("GET", "/api/positions?mode=bogus", nil)
	rec := httptest.NewRecorder()
	srv.handleListPositions(rec, req)
	if rec.Code != 400 {
		t.Fatalf("expected 400 for an invalid mode, got %d", rec.Code)
	}
}

// TestHandleClosePosition_RealModeCallsRequestRealManualClose confirms ?mode=real routes to the
// real-order close-intent path, not the paper one.
func TestHandleClosePosition_RealModeCallsRequestRealManualClose(t *testing.T) {
	repo := &positionsStubRepo{}
	srv := &Server{Repo: repo}

	req := httptest.NewRequest("POST", "/api/positions/5/close?mode=real", nil)
	req.SetPathValue("id", "5")
	rec := httptest.NewRecorder()
	srv.handleClosePosition(rec, req)

	if rec.Code != 202 {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.requestRealManualClose) != 1 || repo.requestRealManualClose[0] != 5 {
		t.Errorf("expected RequestRealManualClose(5), got %v", repo.requestRealManualClose)
	}
	if len(repo.requestManualClose) != 0 {
		t.Errorf("expected no call to the paper-mode RequestManualClose, got %v", repo.requestManualClose)
	}
}

// TestHandleClosePosition_MissingModeRejected confirms mode is required, not defaulted — an
// omitted mode must never silently target the wrong table.
func TestHandleClosePosition_MissingModeRejected(t *testing.T) {
	srv := &Server{Repo: &positionsStubRepo{}}
	req := httptest.NewRequest("POST", "/api/positions/5/close", nil)
	req.SetPathValue("id", "5")
	rec := httptest.NewRecorder()
	srv.handleClosePosition(rec, req)
	if rec.Code != 400 {
		t.Fatalf("expected 400 for a missing mode, got %d", rec.Code)
	}
}
