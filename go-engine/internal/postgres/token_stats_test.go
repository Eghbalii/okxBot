package postgres

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// Regression for the 2026-09-22 operator instruction: TokenStatsAllTime (renamed from
// TokenStats24h) must include a trade closed long before the old 24h window — the whole reason
// for the rename was that the window hid most of a token's real track record. A test against a
// fake repository cannot catch a stale WHERE clause the way it caught nothing before this file
// existed (§36.3's own lesson): the fake never had a time filter to begin with, only the real SQL
// did.
func TestTokenStatsAllTime_IncludesTradesOlderThan24Hours(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	instID := "TESTOLDTOKEN"
	id, err := repo.OpenPaperOrder(ctx, port.PaperOrder{
		InstID:   instID,
		Side:     "buy",
		EntryPx:  decimal.RequireFromString("100"),
		Size:     decimal.RequireFromString("10"),
		Leverage: decimal.RequireFromString("1"),
		Mode:     "paper",
		Variant:  "baseline",
	})
	if err != nil {
		t.Fatalf("open paper order: %v", err)
	}
	t.Cleanup(func() {
		_, _ = repo.pool.Exec(ctx, `DELETE FROM paper_orders WHERE id = $1`, id)
	})

	if err := repo.ClosePaperOrder(ctx, id, decimal.RequireFromString("110"), "manual",
		decimal.RequireFromString("1.5"), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("close paper order: %v", err)
	}
	// Backdate well past the old 24h window this function used to filter on.
	if _, err := repo.pool.Exec(ctx, `UPDATE paper_orders SET closed_at = now() - interval '30 days' WHERE id = $1`, id); err != nil {
		t.Fatalf("backdate closed_at: %v", err)
	}

	stats, err := repo.TokenStatsAllTime(ctx, "paper")
	if err != nil {
		t.Fatalf("token stats all-time: %v", err)
	}
	for _, s := range stats {
		if s.InstID == instID {
			if s.PositionCount != 1 {
				t.Errorf("expected the 30-day-old close to be counted, got position count %d", s.PositionCount)
			}
			return
		}
	}
	t.Errorf("a trade closed 30 days ago was not returned — the all-time query still has a time window")
}
