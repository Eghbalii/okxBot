package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	// realOrder backs GetRealOrder, which handleClosePosition consults when a close is refused so
	// it can say WHY (already closed, and by what) instead of only "not open".
	realOrder    port.RealOrder
	realOrderErr error
}

func (r *positionsStubRepo) GetRealOrder(ctx context.Context, id int64) (port.RealOrder, error) {
	return r.realOrder, r.realOrderErr
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

// A Close click that lands just after the exchange's own stop-loss fired must say so (2026-09-09).
// The operator hit exactly this on real orders 39 and 40: OKX's stop closed the position, the panel
// had not refreshed yet, and the click came back with the bare "real order 39 is not open" — which
// reads like a fault and leaves them unsure whether the position is still open.
func TestHandleClosePosition_ExplainsThatThePositionAlreadyClosed(t *testing.T) {
	closedAt := time.Date(2026, 9, 9, 19, 50, 23, 0, time.UTC)
	reason := "sl"
	repo := &positionsStubRepo{
		realManualCloseErr: errors.New("real order 39 is not open"),
		realOrder:          port.RealOrder{ID: 39, ClosedAt: &closedAt, CloseReason: &reason},
	}
	srv := &Server{Repo: repo, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	req := httptest.NewRequest("POST", "/api/positions/39/close?mode=real", nil)
	req.SetPathValue("id", "39")
	rec := httptest.NewRecorder()
	srv.handleClosePosition(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "already closed") || !strings.Contains(body, "sl") {
		t.Errorf("the message must say the position is already closed and how; got %s", body)
	}
}

// When the order genuinely cannot be read back, the original error still surfaces rather than being
// replaced by a friendlier message that would be a guess.
func TestHandleClosePosition_KeepsTheRawErrorWhenTheOrderCannotBeRead(t *testing.T) {
	repo := &positionsStubRepo{
		realManualCloseErr: errors.New("real order 99 is not open"),
		realOrderErr:       errors.New("no such order"),
	}
	srv := &Server{Repo: repo, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	req := httptest.NewRequest("POST", "/api/positions/99/close?mode=real", nil)
	req.SetPathValue("id", "99")
	rec := httptest.NewRecorder()
	srv.handleClosePosition(rec, req)

	if !strings.Contains(rec.Body.String(), "is not open") {
		t.Errorf("want the underlying error, got %s", rec.Body.String())
	}
}
