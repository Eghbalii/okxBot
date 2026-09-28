package optimizer

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// TestPromote_DisablesOriginAssignmentOnFirstPromotion is a regression test for a real production
// bug (2026-09-28): a lineage's FIRST-ever promotion left both the origin strategy AND the newly
// promoted clone trading the same (kind, inst, bar) simultaneously, forever. Promote() only
// disabled a PREVIOUSLY 'paper_active' candidate's own assignment (the hadPrevious branch) — but
// the origin is never itself tracked as a paper_active candidate (it's a separate, always-enabled
// bootstrap assignment created by ensureDefaultAssignment/SeedOrigins that predates the optimizer
// pipeline entirely), so on a lineage's first promotion there was nothing to demote and the origin
// kept trading forever, duplicating every signal that lineage's real kind produced.
func TestPromote_DisablesOriginAssignmentOnFirstPromotion(t *testing.T) {
	store := testStore(t)
	repo := testRepo(t)
	ctx := context.Background()

	// A kind unique to this test, not shared with loop_test.go's own "rsi_sma" seeding — reusing a
	// kind another test also seeds creates a second origin row for that kind in the same database,
	// and findOriginStrategyID (which just returns the first match) can then resolve to a
	// DIFFERENT origin row than the one this test's assignment is attached to, silently disabling
	// nothing and producing a false pass/fail depending on test run order. Found exactly this way:
	// this test passed in isolation and failed only when run after TestLoop_ProposeAndPass.
	const kind = "rsi_sma_promote_test_only"
	originID := seedOrigin(t, repo, kind)
	originAssignID, err := repo.CreateAssignment(ctx, port.StrategyAssignment{
		StrategyID: originID, InstID: "PROMOTETEST", Bar: "5m", Enabled: true, Mode: "paper", Exchange: "okx",
	})
	if err != nil {
		t.Fatalf("create origin assignment: %v", err)
	}

	lin := Lineage{Kind: kind, InstID: "PROMOTETEST", Bar: "5m", Exchange: "okx", RiskProfile: "low"}
	candidateID, err := store.CreateCandidate(ctx, lin, []byte(`{"rsi_period":21}`), originID, 2, 0, 0, "optimizer", nil)
	if err != nil {
		t.Fatalf("create candidate: %v", err)
	}
	now := time.Now()
	if err := store.RecordBacktestResult(ctx, candidateID, true, "", 50, decimal.NewFromInt(60), decimal.NewFromInt(10), 0, nil, now.Add(-30*24*time.Hour), now); err != nil {
		t.Fatalf("record backtest result: %v", err)
	}

	if _, err := Promote(ctx, store, repo, candidateID, 10); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	assignments, err := repo.ListAssignments(ctx, "PROMOTETEST", false, "paper", "okx")
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	var originStillEnabled bool
	var cloneEnabled bool
	for _, a := range assignments {
		if a.ID == originAssignID {
			originStillEnabled = a.Enabled
		}
		if a.StrategyID != originID && a.Bar == "5m" {
			cloneEnabled = a.Enabled
		}
	}
	if originStillEnabled {
		t.Fatal("origin assignment is still enabled after the lineage's first-ever promotion — this is the exact bug: origin and clone both trade the same lineage forever")
	}
	if !cloneEnabled {
		t.Fatal("the newly promoted clone's assignment must be enabled")
	}
}
