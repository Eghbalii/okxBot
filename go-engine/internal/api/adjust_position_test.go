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

// stubRepo embeds port.Repository (nil) so only the methods a given test actually exercises need
// overriding — any unstubbed call panics, which is the right failure mode for a test-only fake:
// it surfaces immediately as "this test needs a stub for X" rather than silently doing nothing.
type stubRepo struct {
	port.Repository
	order           port.RealOrder
	getErr          error
	updateSLTPCalls []struct {
		id             int64
		sl, tp         *decimal.Decimal
		manualOverride bool
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

func (s *stubRepo) UpdateRealOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal, manualOverride bool) error {
	s.updateSLTPCalls = append(s.updateSLTPCalls, struct {
		id             int64
		sl, tp         *decimal.Decimal
		manualOverride bool
	}{id, slPx, tpPx, manualOverride})
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

// stubAmender records the amends that reached the "exchange", so a test can assert the trigger
// prices actually sent — the property that matters since 2026-09-09: an operator's panel edit must
// change the level OKX is enforcing, not just the row the panel displays.
type stubAmender struct {
	amends []domain.AlgoOrderAmend
	err    error
}

func (a *stubAmender) AmendAlgoOrder(req domain.AlgoOrderAmend) error {
	if a.err != nil {
		return a.err
	}
	a.amends = append(a.amends, req)
	return nil
}

func newTestServer(repo *stubRepo) *Server {
	return &Server{
		Repo:       repo,
		Protection: &stubAmender{},
		Logger:     slog.Default(),
	}
}

// testAlgoID is the resting protective order every fixture position carries. A real open position
// always has one (openReal closes any position it cannot protect), so a fixture without one would
// be testing a state production does not produce.
const testAlgoID = "algo-test-1"

func algoID() *string { id := testAlgoID; return &id }

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
		EntryPx: dec("100"), Leverage: dec("10"), ExchangeAlgoOrderID: algoID(),
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
		EntryPx: dec("100"), Leverage: dec("10"), ExchangeAlgoOrderID: algoID(),
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
		EntryPx: dec("100"), Leverage: dec("10"), ExchangeAlgoOrderID: algoID(),
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
		EntryPx: dec("100"), Leverage: dec("10"), ExchangeAlgoOrderID: algoID(),
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
		EntryPx: dec("100"), Leverage: dec("20"), ExchangeAlgoOrderID: algoID(),
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

// A manual edit must lock the order out of the model's update loop (2026-09-06 request): the
// handler passes manualOverride=true to UpdateRealOrderSLTP on every manual adjustment, regardless
// of which field (SL or TP, or both) was actually touched.
func TestHandleAdjustPosition_SetsManualOverride(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{
		ID: 1, InstID: "BTC-USDT-SWAP", Side: "buy",
		EntryPx: dec("100"), Leverage: dec("10"), ExchangeAlgoOrderID: algoID(),
	}}
	srv := newTestServer(repo)

	rec := doAdjust(srv, "1", adjustPositionRequest{SLPct: floatPtr(-5)})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.updateSLTPCalls) != 1 || !repo.updateSLTPCalls[0].manualOverride {
		t.Fatalf("expected UpdateRealOrderSLTP to be called with manualOverride=true, got %+v", repo.updateSLTPCalls)
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

// An operator's manual edit must reach the EXCHANGE, not just the database (2026-09-09 request).
// Before this, the handler wrote the new level locally and OKX kept enforcing the old one — the
// operator would believe they had moved their stop when they had not.
func TestHandleAdjustPosition_AmendsTheExchangeOrder(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{
		ID: 1, InstID: "BTC-USDT-SWAP", Side: "buy",
		EntryPx: dec("100"), Leverage: dec("10"), ExchangeAlgoOrderID: algoID(),
	}}
	amender := &stubAmender{}
	srv := newTestServer(repo)
	srv.Protection = amender

	rec := doAdjust(srv, "1", adjustPositionRequest{SLPct: floatPtr(-5)})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(amender.amends) != 1 {
		t.Fatalf("expected exactly 1 exchange amend, got %d", len(amender.amends))
	}
	if amender.amends[0].AlgoID != testAlgoID {
		t.Errorf("amended the wrong order: want %q, got %q", testAlgoID, amender.amends[0].AlgoID)
	}
	// -5% of margin at 10x is a 0.5% price move below entry.
	if !amender.amends[0].SLTriggerPx.Equal(dec("99.5")) {
		t.Errorf("the exchange must receive the new stop 99.5, got %v", amender.amends[0].SLTriggerPx)
	}
}

// A rejected amend must leave the stored levels untouched. The dangerous state is the exchange
// holding one stop while the panel displays another, so a failure changes nothing at all.
func TestHandleAdjustPosition_ExchangeRejectionChangesNothing(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{
		ID: 1, InstID: "BTC-USDT-SWAP", Side: "buy",
		EntryPx: dec("100"), Leverage: dec("10"), ExchangeAlgoOrderID: algoID(),
	}}
	srv := newTestServer(repo)
	srv.Protection = &stubAmender{err: errors.New("rejected")}

	rec := doAdjust(srv, "1", adjustPositionRequest{SLPct: floatPtr(-5)})
	if rec.Code != 502 {
		t.Fatalf("expected 502 when the exchange rejects the amend, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.updateSLTPCalls) != 0 {
		t.Errorf("a rejected amend must not write the new level locally, got %d writes", len(repo.updateSLTPCalls))
	}
	if len(repo.adjustmentsRecorded) != 0 {
		t.Errorf("a rejected amend must record no adjustment row, got %d", len(repo.adjustmentsRecorded))
	}
}

// A position with no resting protective order cannot have its levels moved on the exchange, so the
// edit is refused rather than written locally — the same "never record a level the exchange does
// not have" rule as the rejection case above.
func TestHandleAdjustPosition_RefusesWhenThereIsNoRestingOrder(t *testing.T) {
	repo := &stubRepo{order: port.RealOrder{
		ID: 1, InstID: "BTC-USDT-SWAP", Side: "buy",
		EntryPx: dec("100"), Leverage: dec("10"),
	}}
	srv := newTestServer(repo)

	rec := doAdjust(srv, "1", adjustPositionRequest{SLPct: floatPtr(-5)})
	if rec.Code != 409 {
		t.Fatalf("expected 409 with no resting order, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(repo.updateSLTPCalls) != 0 {
		t.Errorf("nothing must be written locally, got %d writes", len(repo.updateSLTPCalls))
	}
}
