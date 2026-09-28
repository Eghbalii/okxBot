package postgres

import (
	"context"
	"testing"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// TestSetAssignmentsEnabledForKinds_PaperModeNeverTouchesOrigins is a regression test for a real
// production bug (2026-09-28): the optimize/backtest pipeline disables an origin strategy's paper
// assignment the moment a real tuned candidate is promoted to replace it, but this function's own
// UPDATE re-enabled EVERY assignment (origin or not) whose strategy kind appeared in activeKinds —
// which is exactly every kind the operator has ever turned on — undoing the pipeline's cleanup on
// every single cmd/paper-trader restart. Paper positions kept opening under old strategy names no
// matter how many times the origins were disabled, because they were silently re-enabled again a
// few seconds later at the next restart.
func TestSetAssignmentsEnabledForKinds_PaperModeNeverTouchesOrigins(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	originID, err := repo.CreateStrategy(ctx, port.StrategyConfig{
		Name: "rsi_sma", Kind: "rsi_sma", Config: []byte("{}"), Enabled: true, IsOrigin: true,
	})
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	cloneID, err := repo.CreateStrategy(ctx, port.StrategyConfig{
		Name: "rsi_sma_TESTTOK_R10_G1_B0_P0", Kind: "rsi_sma", Config: []byte(`{"rsi_period":21}`),
		Enabled: true, IsOrigin: false,
	})
	if err != nil {
		t.Fatalf("create clone: %v", err)
	}

	originAssignID, err := repo.CreateAssignment(ctx, port.StrategyAssignment{
		StrategyID: originID, InstID: "TESTTOK", Bar: "5m", Enabled: false, Mode: "paper", Exchange: "okx",
	})
	if err != nil {
		t.Fatalf("create origin assignment: %v", err)
	}
	cloneAssignID, err := repo.CreateAssignment(ctx, port.StrategyAssignment{
		StrategyID: cloneID, InstID: "TESTTOK", Bar: "5m", Enabled: true, Mode: "paper", Exchange: "okx",
	})
	if err != nil {
		t.Fatalf("create clone assignment: %v", err)
	}

	// Exactly what cmd/paper-trader calls on every startup, with "rsi_sma" in the operator's own
	// active-kinds allowlist — the origin's kind, since the clone shares the same kind by design.
	if err := repo.SetAssignmentsEnabledForKinds(ctx, "paper", "okx", []string{"rsi_sma"}, []string{"TESTTOK"}, []string{"5m"}); err != nil {
		t.Fatalf("SetAssignmentsEnabledForKinds: %v", err)
	}

	assignments, err := repo.ListAssignments(ctx, "TESTTOK", false, "paper", "okx")
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	byID := make(map[int64]port.StrategyAssignment, len(assignments))
	for _, a := range assignments {
		byID[a.ID] = a
	}

	if a, ok := byID[originAssignID]; !ok {
		t.Fatal("origin assignment row vanished entirely")
	} else if a.Enabled {
		t.Fatal("origin assignment was re-enabled by the active-kinds filter — this is the exact bug: paper positions kept opening under the old origin strategy no matter how many times the pipeline disabled it")
	}
	if a, ok := byID[cloneAssignID]; !ok {
		t.Fatal("promoted clone's assignment row vanished")
	} else if !a.Enabled {
		t.Fatal("promoted clone's assignment was disabled by the active-kinds filter — it should stay enabled since its kind is in activeKinds")
	}

	// The INSERT half must not resurrect a fresh origin assignment for a lineage that already has
	// a real promoted clone trading it (the ON CONFLICT is keyed by strategy_id, which the origin
	// and its clone never share, so this insert could otherwise sneak a duplicate origin row in
	// even though a real assignment already exists for this exact kind/token/bar).
	var originAssignmentCount int
	for _, a := range assignments {
		if a.StrategyID == originID {
			originAssignmentCount++
		}
	}
	if originAssignmentCount != 1 {
		t.Fatalf("expected exactly 1 origin assignment row for TESTTOK/5m/rsi_sma, got %d — a second one was created", originAssignmentCount)
	}
}

// TestSetAssignmentsEnabledForKinds_BotModeStillBootstrapsOrigins confirms mode="bot" (real
// trading, which has no optimize pipeline) keeps its original behavior unchanged — the exclusion
// above is scoped to paper mode only.
func TestSetAssignmentsEnabledForKinds_BotModeStillBootstrapsOrigins(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	originID, err := repo.CreateStrategy(ctx, port.StrategyConfig{
		Name: "pmax_bot_test", Kind: "pmax_bot_test", Config: []byte("{}"), Enabled: true, IsOrigin: true,
	})
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}

	if err := repo.SetAssignmentsEnabledForKinds(ctx, "bot", "okx", []string{"pmax_bot_test"}, []string{"BOTTOK"}, []string{"5m"}); err != nil {
		t.Fatalf("SetAssignmentsEnabledForKinds: %v", err)
	}

	assignments, err := repo.ListAssignments(ctx, "BOTTOK", false, "bot", "okx")
	if err != nil {
		t.Fatalf("list assignments: %v", err)
	}
	var found bool
	for _, a := range assignments {
		if a.StrategyID == originID && a.Enabled {
			found = true
		}
	}
	if !found {
		t.Fatal("bot mode must still bootstrap a fresh enabled origin assignment for a newly-activated kind with none yet — this is the pre-existing, still-needed behavior")
	}
}
