package usecase

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// TestReconcile_DoesNotCloseAJustOpenedPositionOnAFlatReading reproduces the 2026-09-13 incident
// that cost real money (CLAUDE.md §51).
//
// Production sequence, from the trader's own logs:
//
//	15:35:02  opened real order id=148 instId=PUMP (SL rested on the exchange)
//	15:35:05  reconcile: "exchange reports flat but local state shows an open position"
//	15:35:06  cancelled the resting protective order; closed real order id=148
//	15:35:24  reconcile: "exchange reports an open position this system has no record of"
//
// OKX's positions endpoint trailed its own fill by ~20 seconds. Reconcile believed the first flat
// reading, marked the order closed AND cancelled its stop — leaving a real, unprotected position
// running on the exchange with no local record, which had to be flattened by hand.
func TestReconcile_DoesNotCloseAJustOpenedPositionOnAFlatReading(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	e := newTestRealTrader(repo, exchange, nil, nil)

	// A position opened moments ago, exactly as order 148 was.
	id, err := repo.OpenRealOrder(context.Background(), port.RealOrder{
		InstID:   e.InstID,
		Side:     "buy",
		Status:   "filled",
		EntryPx:  dec("0.00365"),
		Size:     dec("3.2"),
		Leverage: dec("9"),
		OpenedAt: time.Now().Add(-3 * time.Second),
	})
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}

	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	// The exchange reports FLAT — the lagging reading that caused the incident.
	e.ReconcileWith(context.Background(), AccountSnapshot{}, logger)

	open, err := repo.ListRealPositions(context.Background(), port.PositionFilter{Mode: "real", Open: boolPtr(true)})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("the just-opened position was closed on a lagging flat reading — this is what left "+
			"a real position running with its stop cancelled and no local record (got %d open)", len(open))
	}
	if open[0].ID != id {
		t.Errorf("wrong order left open: got %d want %d", open[0].ID, id)
	}
	if !strings.Contains(logged.String(), "deferring rather than closing") {
		t.Errorf("the deferral was not logged, so an operator could not see why:\n%s", logged.String())
	}
}

// TestReconcile_StillClosesAnOlderPositionReportedFlat is the other half: the grace period must
// narrow the check, not disable it. A position genuinely closed outside this system — a
// liquidation, or a manual close in OKX's own app — must still be reconciled once it is past the
// window, or the database would drift from the exchange permanently.
func TestReconcile_StillClosesAnOlderPositionReportedFlat(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{}
	e := newTestRealTrader(repo, exchange, nil, nil)

	// Opened well beyond the grace period.
	if _, err := repo.OpenRealOrder(context.Background(), port.RealOrder{
		InstID:   e.InstID,
		Side:     "buy",
		Status:   "filled",
		EntryPx:  dec("0.00365"),
		Size:     dec("3.2"),
		Leverage: dec("9"),
		OpenedAt: time.Now().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("seed order: %v", err)
	}

	e.ReconcileWith(context.Background(), AccountSnapshot{}, slog.New(slog.NewTextHandler(&strings.Builder{}, nil)))

	open, err := repo.ListRealPositions(context.Background(), port.PositionFilter{Mode: "real", Open: boolPtr(true)})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("a position the exchange no longer holds stayed open locally — the grace period is "+
			"too broad and the database would drift permanently (got %d open)", len(open))
	}
}

func boolPtr(b bool) *bool { return &b }
