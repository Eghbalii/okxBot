package api

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// TestManualOrderToPosition_FilledOrderCarriesEntryAndOpenedAt confirms the common case: a filled
// manual order's EntryPx/OpenedAt (both pointers on port.ManualOrder, since a resting/unfilled
// order has neither yet) come through as the dereferenced value on port.PaperOrder.
func TestManualOrderToPosition_FilledOrderCarriesEntryAndOpenedAt(t *testing.T) {
	entry := decimal.NewFromInt(100)
	status := "filled"
	o := port.ManualOrder{
		ID: 1, InstID: "BTC", Side: "buy", Status: status,
		EntryPx: &entry, Size: decimal.NewFromInt(50), Leverage: decimal.NewFromInt(10),
	}
	p := manualOrderToPosition(o)
	if !p.EntryPx.Equal(entry) {
		t.Errorf("EntryPx = %s, want %s", p.EntryPx, entry)
	}
	if p.Mode != "manual" {
		t.Errorf("Mode = %q, want manual", p.Mode)
	}
}

// TestManualOrderToPosition_RestingOrderHasNoEntryPrice is the regression guard: a resting (still
// unfilled) limit order has EntryPx=nil/OpenedAt=nil on port.ManualOrder, and this conversion must
// not panic dereferencing a nil pointer — the exact bug `go build` caught before this test existed
// (manualOrderToPosition's first version assigned the pointer fields directly into PaperOrder's
// value fields, which failed to even compile; this test pins the nil-safe behavior so a future
// refactor can't reintroduce the crash silently by making the types match again).
func TestManualOrderToPosition_RestingOrderHasNoEntryPrice(t *testing.T) {
	o := port.ManualOrder{ID: 2, InstID: "BTC", Side: "buy", Status: "resting"}
	p := manualOrderToPosition(o)
	if !p.EntryPx.IsZero() {
		t.Errorf("EntryPx = %s, want zero for a resting order with no fill yet", p.EntryPx)
	}
	if !p.OpenedAt.IsZero() {
		t.Errorf("OpenedAt = %v, want zero for a resting order", p.OpenedAt)
	}
}
