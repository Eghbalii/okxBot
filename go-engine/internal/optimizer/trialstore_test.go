package optimizer

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// redisAvailable does a fast TCP dial to the default local Redis port so this test can skip
// cleanly in environments (like CI containers or this sandbox) with no real Redis running,
// rather than failing — matching the task's "only integration-test the Redis plumbing manually /
// skip it if it needs a real Redis" guidance while still exercising the real client end-to-end
// wherever a Redis instance is reachable (e.g. local dev, docker-compose).
func redisAvailable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func TestTrialStore_OpenCloseBookkeeping(t *testing.T) {
	addr := "localhost:6379"
	if !redisAvailable(addr) {
		t.Skip("no local Redis reachable at " + addr + "; skipping Redis-backed trial store test")
	}

	store := NewTrialStore(addr)
	defer store.CloseConn()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runID := "test-run-" + time.Now().Format("20060102150405.000000000")
	instID := "BTC-USDT-SWAP"

	sl := decimal.NewFromInt(95)
	tp := decimal.NewFromInt(110)
	trial := OpenTrial{
		StudyID:  StudyID(instID, "rsi_sma"),
		TrialID:  1,
		InstID:   instID,
		Side:     "buy",
		EntryPx:  decimal.NewFromInt(100),
		SLPx:     &sl,
		TPPx:     &tp,
		OpenedAt: time.Now(),
	}

	if err := store.Open(ctx, runID, trial, time.Minute); err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	open, err := store.OpenForInst(ctx, runID, instID)
	if err != nil {
		t.Fatalf("OpenForInst failed: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected 1 open trial, got %d", len(open))
	}
	if open[0].TrialID != 1 || !open[0].EntryPx.Equal(decimal.NewFromInt(100)) {
		t.Errorf("unexpected trial round-trip: %+v", open[0])
	}

	if err := store.Close(ctx, runID, trial); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	open, err = store.OpenForInst(ctx, runID, instID)
	if err != nil {
		t.Fatalf("OpenForInst after close failed: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("expected 0 open trials after close, got %d: %+v", len(open), open)
	}
}

func TestTrialStore_MultipleCandidatesIndependentlyTracked(t *testing.T) {
	addr := "localhost:6379"
	if !redisAvailable(addr) {
		t.Skip("no local Redis reachable at " + addr + "; skipping Redis-backed trial store test")
	}

	store := NewTrialStore(addr)
	defer store.CloseConn()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runID := "test-run-multi-" + time.Now().Format("20060102150405.000000000")
	instID := "ETH-USDT-SWAP"

	for i := 1; i <= 3; i++ {
		trial := OpenTrial{
			StudyID:  StudyID(instID, "rsi_sma"),
			TrialID:  i,
			InstID:   instID,
			Side:     "sell",
			EntryPx:  decimal.NewFromInt(int64(1000 + i)),
			OpenedAt: time.Now(),
		}
		if err := store.Open(ctx, runID, trial, time.Minute); err != nil {
			t.Fatalf("Open trial %d failed: %v", i, err)
		}
	}

	open, err := store.OpenForInst(ctx, runID, instID)
	if err != nil {
		t.Fatalf("OpenForInst failed: %v", err)
	}
	if len(open) != 3 {
		t.Fatalf("expected 3 open trials, got %d", len(open))
	}

	// Close just trial 2; 1 and 3 should remain.
	if err := store.Close(ctx, runID, open[1]); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	remaining, err := store.OpenForInst(ctx, runID, instID)
	if err != nil {
		t.Fatalf("OpenForInst after partial close failed: %v", err)
	}
	if len(remaining) != 2 {
		t.Errorf("expected 2 remaining open trials, got %d", len(remaining))
	}
}
