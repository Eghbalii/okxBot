package postgres

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// TestListBotPositions_UntrackedCountsAsOpen is the fix for a real incident (2026-09-28): a
// position reconcile discovers on the exchange with no local record is written as
// status='untracked' specifically so the operator can see and act on it — but the panel's own
// "open positions" filter excluded that status, so the row existed in the database and was STILL
// invisible on the page it was written for. Tests here run against a REAL database or not at all
// (this package's own established rule) — a fake repository cannot reproduce a WHERE clause typo
// that silently excludes a status value.
func TestListBotPositions_UntrackedCountsAsOpen(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	id, err := repo.OpenBotOrder(ctx, port.BotOrder{
		InstID:   "TESTUNTRACKED",
		Side:     "sell",
		EntryPx:  decimal.NewFromInt(50000),
		Size:     decimal.NewFromInt(100),
		Leverage: decimal.NewFromInt(10),
		Status:   "untracked",
	})
	if err != nil {
		t.Fatalf("OpenBotOrder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(ctx, "DELETE FROM bot_orders WHERE id = $1", id)
	})

	open := true
	rows, err := repo.ListBotPositions(ctx, port.PositionFilter{InstID: "TESTUNTRACKED", Open: &open, SortBy: "opened_at"})
	if err != nil {
		t.Fatalf("ListBotPositions: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the untracked position (id=%d) to appear in the OPEN positions filter, got %d rows", id, len(rows))
	}
}
