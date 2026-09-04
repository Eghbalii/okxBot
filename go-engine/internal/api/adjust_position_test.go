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

// stubRepo embeds port.Repository (nil) so only the methods a given test actually exercises need
// overriding — any unstubbed call panics, which is the right failure mode for a test-only fake:
// it surfaces immediately as "this test needs a stub for X" rather than silently doing nothing.
type stubRepo struct {
	port.Repository
	order           port.RealOrder
	getErr          error
	updateSLTPCalls []struct {
		id     int64
		sl, tp *decimal.Decimal
	}
	adjustmentsRecorded []struct {
		id        int64
		field     string
		old, new_ *decimal.Decimal
		source    string
	}
}

func (s *stubRepo) GetRealOrder(ctx context.Context, id int64) (port.RealOrder, error) {
	if s.getErr != nil {
		return port.RealOrder{}, s.getErr
	}
	return s.order, nil
}

func (s *stubRepo) UpdateRealOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal) error {
	s.updateSLTPCalls = append(s.updateSLTPCalls, struct {
		id     int64
		sl, tp *decimal.Decimal
	}{id, slPx, tpPx})
	return nil
}

func (s *stubRepo) RecordRealOrderAdjustment(ctx context.Context, orderID int64, field string, oldValue, newValue *decimal.Decimal, source string) error {
	s.adjustmentsRecorded = append(s.adjustmentsRecorded, struct {
		id        int64
		field     string
		old, new_ *decimal.Decimal
		source    string
	}{orderID, field, oldValue, newValue, source})
	return nil
}

func newTestServer(repo *stubRepo) *Server {
	return &Server{
		Repo:   repo,
		Logger: slog.Default(),
	}
}

// doAdjust defaults to ?mode=real (every pre-existing test in this file exercises the real-mode
// path); doAdjustMode lets a test override it (e.g. to exercise the paper-mode-rejected case).
func doAdjust(srv *Server, id string, body adjustPositionRequest) *httptest.ResponseRecorder {
	return doAdjustMode(srv, id, "real", body)
}

func doAdjustMode(srv *Server, id, mode string, body adjustPositionRequest) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/positions/"+id+"/adjust?mode="+mode, bytes.NewReader(raw))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	srv.handleAdjustPosition(rec, req)
	return rec
}

func floatPtr(f float64) *float64 { return &f }

func TestHandleAdjustPosition_MovesSLIntoLoss(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{
		ID: 1, InstID: "BTC-USDT-SWAP", Side: "buy",
		EntryPx: dec("100"), Leverage: dec("10"),
	}}
	srv := newTestServer(repo)

	rec := doAdjust(srv, "1", adjustPositionRequest{SLPct: floatPtr(-5)})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.updateSLTPCalls) != 1 {
		t.Fatalf("expected 1 UpdateRealOrderSLTP call, got %d", len(repo.updateSLTPCalls))
	}
	// -5% margin loss at 10x leverage = 0.5% price move from entry, below (loss side) for a long.
	got := repo.updateSLTPCalls[0].sl
	if got == nil || !got.Equal(dec("99.5")) {
		t.Fatalf("expected SL=99.5, got %v", got)
	}
	if len(repo.adjustmentsRecorded) != 1 || repo.adjustmentsRecorded[0].source != "manual" {
		t.Fatalf("expected exactly 1 adjustment row with source=manual, got %+v", repo.adjustmentsRecorded)
	}
}

func TestHandleAdjustPosition_BringsSLIntoProfit(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{
		ID: 1, InstID: "BTC-USDT-SWAP", Side: "buy",
		EntryPx: dec("100"), Leverage: dec("10"),
	}}
	srv := newTestServer(repo)

	// Positive SLPct: "bring the stop into profit" (operator's own phrase).
	rec := doAdjust(srv, "1", adjustPositionRequest{SLPct: floatPtr(2)})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := repo.updateSLTPCalls[0].sl
	// +2% margin profit at 10x = 0.2% price move ABOVE entry for a long.
	if got == nil || !got.Equal(dec("100.2")) {
		t.Fatalf("expected SL=100.2 (profit side, above entry), got %v", got)
	}
}

func TestHandleAdjustPosition_ShortSideDirectionIsMirrored(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{
		ID: 1, InstID: "BTC-USDT-SWAP", Side: "sell",
		EntryPx: dec("100"), Leverage: dec("10"),
	}}
	srv := newTestServer(repo)

	rec := doAdjust(srv, "1", adjustPositionRequest{SLPct: floatPtr(-5)})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := repo.updateSLTPCalls[0].sl
	// A short's loss side is ABOVE entry: -5%/10x = 0.5% price move up.
	if got == nil || !got.Equal(dec("100.5")) {
		t.Fatalf("expected SL=100.5 (loss side for a short is above entry), got %v", got)
	}
}

// TestHandleAdjustPosition_IsDeliberatelyUnclamped confirms the manual endpoint applies NO clamp
// at all (explicit operator decision, 2026-09-03): a manual edit converts straight from percentage
// to price with no bound, unlike the model's own automated paths (Clamps.Apply at open,
// RatchetSLTP in-trade) — neither of which fits an operator action taken directly and knowingly.
// A -50% "loss" at 10x leverage (a 5% price move, ten times CLAUDE.md §19.2's normal 15%-of-margin/
// leverage cap) must still be written exactly as entered.
func TestHandleAdjustPosition_IsDeliberatelyUnclamped(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{
		ID: 1, InstID: "BTC-USDT-SWAP", Side: "buy",
		EntryPx: dec("100"), Leverage: dec("10"),
	}}
	srv := newTestServer(repo)

	rec := doAdjust(srv, "1", adjustPositionRequest{SLPct: floatPtr(-50)})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := repo.updateSLTPCalls[0].sl
	// -50%/10x = 5% price move, unclamped: 100 - 5 = 95.
	if got == nil || !got.Equal(dec("95")) {
		t.Fatalf("expected an unclamped SL=95, got %v", got)
	}
}

// TestHandleAdjustPosition_BringsSLPastEntryIntoProfit confirms the exact scenario that motivated
// removing the clamp: moving a long's SL from below entry to ABOVE it (locking in profit) — which
// conductor.Clamps.Apply would reject outright, since it treats any long stop above entry as an
// incoherent/"wrong side" level. The unclamped manual endpoint must allow it.
func TestHandleAdjustPosition_BringsSLPastEntryIntoProfit(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{
		ID: 1, InstID: "BTC-USDT-SWAP", Side: "buy",
		EntryPx: dec("100"), Leverage: dec("20"),
	}}
	srv := newTestServer(repo)

	// The operator's own example: +10 on a 20x position = 0.5% above entry.
	rec := doAdjust(srv, "1", adjustPositionRequest{SLPct: floatPtr(10)})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	got := repo.updateSLTPCalls[0].sl
	if got == nil || !got.Equal(dec("100.5")) {
		t.Fatalf("expected SL=100.5 (0.5%% above entry), got %v", got)
	}
}

func TestHandleAdjustPosition_RejectsClosedOrder(t *testing.T) {
	now := time.Now()
	repo := &stubRepo{order: port.RealOrder{
		ID: 1, Side: "buy", EntryPx: dec("100"), Leverage: dec("10"),
		ClosedAt: &now,
	}}
	srv := newTestServer(repo)

	rec := doAdjust(srv, "1", adjustPositionRequest{SLPct: floatPtr(-5)})
	if rec.Code != 409 {
		t.Fatalf("expected 409 for a closed order, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.updateSLTPCalls) != 0 {
		t.Error("expected no write for a closed order")
	}
}

// TestHandleAdjustPosition_RejectsPaperMode confirms ?mode=paper is rejected outright, before any
// repository call — the table split (CLAUDE.md real-trading readiness plan, 2026-09-04) makes
// mode="real" the only table this endpoint can ever address.
func TestHandleAdjustPosition_RejectsPaperMode(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{ID: 1, Side: "buy", EntryPx: dec("100"), Leverage: dec("10")}}
	srv := newTestServer(repo)

	rec := doAdjustMode(srv, "1", "paper", adjustPositionRequest{SLPct: floatPtr(-5)})
	if rec.Code != 400 {
		t.Fatalf("expected 400 for mode=paper, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.updateSLTPCalls) != 0 {
		t.Error("expected no write for mode=paper")
	}
}

// TestHandleAdjustPosition_RejectsMissingMode confirms the mode query param is required, not
// defaulted — a missing mode must never silently target the wrong table.
func TestHandleAdjustPosition_RejectsMissingMode(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{ID: 1, Side: "buy", EntryPx: dec("100"), Leverage: dec("10")}}
	srv := newTestServer(repo)

	rec := doAdjustMode(srv, "1", "", adjustPositionRequest{SLPct: floatPtr(-5)})
	if rec.Code != 400 {
		t.Fatalf("expected 400 for a missing mode, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleAdjustPosition_RejectsEmptyBody(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{ID: 1, Side: "buy", EntryPx: dec("100"), Leverage: dec("10")}}
	srv := newTestServer(repo)

	rec := doAdjust(srv, "1", adjustPositionRequest{})
	if rec.Code != 400 {
		t.Fatalf("expected 400 for an empty body, got %d", rec.Code)
	}
}

func TestPriceFromMarginPct_TableDriven(t *testing.T) {
	cases := []struct {
		name          string
		side          string
		pct           string
		leverage      string
		expectedPrice string
	}{
		{"long loss", "buy", "-5", "10", "99.5"},
		{"long profit", "buy", "5", "10", "100.5"},
		{"short loss", "sell", "-5", "10", "100.5"},
		{"short profit", "sell", "5", "10", "99.5"},
		{"1x leverage", "buy", "-10", "1", "90"},
		{"zero leverage falls back to 1x", "buy", "-10", "0", "90"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := priceFromMarginPct(dec("100"), dec(c.leverage), c.side, dec(c.pct))
			if !got.Equal(dec(c.expectedPrice)) {
				t.Errorf("priceFromMarginPct(100, %s, %q, %s) = %s, want %s", c.leverage, c.side, c.pct, got, c.expectedPrice)
			}
		})
	}
}
