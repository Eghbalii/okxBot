package postgres

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// This package had NO tests at all until 2026-09-10, which is exactly how a SQL type error reached
// production: every caller of SavePaperTradingConfig was covered by a fake repository, and a fake
// cannot reproduce Postgres's type inference. The bug was that coalesce($11, '{}') types its
// literal as text when the parameter is NULL — there is no column to infer text[] from inside a
// coalesce — so every save that did not set auto_disabled_inst_ids was rejected with
// "column is of type text[] but expression is of type text". That is every save the panel makes,
// including the one that resumes real trading.
//
// Tests here run against a REAL database or not at all. Skipping is the honest outcome when none
// is reachable: a mocked version of this test would pass against the broken SQL.
func testDSN() string {
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://okxbot:okxbot@localhost:5432/okxbot"
}

func postgresAvailable(dsn string) bool {
	host := "localhost:5432"
	conn, err := net.DialTimeout("tcp", host, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func testRepo(t *testing.T) *Repository {
	t.Helper()
	dsn := testDSN()
	if !postgresAvailable(dsn) {
		t.Skip("no local Postgres reachable; skipping database-backed config test")
	}
	repo, err := New(context.Background(), dsn)
	if err != nil {
		t.Skipf("could not connect to Postgres (%v); skipping", err)
	}
	return repo
}

// A patch that leaves auto_disabled_inst_ids alone must save. This is the exact call the panel
// makes to resume trading, and it failed outright.
func TestSavePaperTradingConfig_SavesWithoutTouchingAutoDisabled(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	before, err := repo.GetPaperTradingConfig(ctx, "paper", "")
	if err != nil {
		t.Fatalf("read current config: %v", err)
	}
	t.Cleanup(func() {
		state := before.TradingState
		_, _ = repo.SavePaperTradingConfig(ctx, "paper", "", port.PaperTradingConfigPatch{TradingState: &state})
	})

	state := "running"
	got, err := repo.SavePaperTradingConfig(ctx, "paper", "", port.PaperTradingConfigPatch{TradingState: &state})
	if err != nil {
		t.Fatalf("a patch that omits auto_disabled_inst_ids must still save: %v", err)
	}
	if got.TradingState != "running" {
		t.Errorf("trading state: want running, got %q", got.TradingState)
	}
}

// And a patch that DOES set it round-trips as an array, not as a string.
func TestSavePaperTradingConfig_RoundTripsAutoDisabled(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	before, err := repo.GetPaperTradingConfig(ctx, "paper", "")
	if err != nil {
		t.Fatalf("read current config: %v", err)
	}
	t.Cleanup(func() {
		restore := before.AutoDisabledInstIDs
		if restore == nil {
			restore = []string{}
		}
		_, _ = repo.SavePaperTradingConfig(ctx, "paper", "", port.PaperTradingConfigPatch{AutoDisabledInstIDs: &restore})
	})

	want := []string{"AAA", "BBB"}
	got, err := repo.SavePaperTradingConfig(ctx, "paper", "", port.PaperTradingConfigPatch{AutoDisabledInstIDs: &want})
	if err != nil {
		t.Fatalf("save with auto-disabled set: %v", err)
	}
	if len(got.AutoDisabledInstIDs) != 2 || got.AutoDisabledInstIDs[0] != "AAA" || got.AutoDisabledInstIDs[1] != "BBB" {
		t.Fatalf("auto-disabled must round-trip as an array, got %v", got.AutoDisabledInstIDs)
	}

	// Clearing it to empty must also work, and must be distinguishable from "leave it alone".
	empty := []string{}
	cleared, err := repo.SavePaperTradingConfig(ctx, "paper", "", port.PaperTradingConfigPatch{AutoDisabledInstIDs: &empty})
	if err != nil {
		t.Fatalf("clear auto-disabled: %v", err)
	}
	if len(cleared.AutoDisabledInstIDs) != 0 {
		t.Errorf("an explicit empty list must clear the column, got %v", cleared.AutoDisabledInstIDs)
	}
}
