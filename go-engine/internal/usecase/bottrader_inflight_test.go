package usecase

import (
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/risk"
	"github.com/shopspring/decimal"
)

// TestReconcile_DoesNotHaltWhileAnOpenIsInFlight reproduces the production outage of 2026-09-13
// (CLAUDE.md §48).
//
// Exchange.PlaceOrder is a blocking network call and the exchange fills the position DURING it, so
// there is an unavoidable window where the position exists remotely with no local row. A reconcile
// pass landing in that window saw an "untracked" position and halted ALL real trading across every
// instrument — measured at ~1s wide in production, and made reliably reproducible by the private
// WebSocket, since the fill itself pushes an event that triggers the pass.
func TestReconcile_DoesNotHaltWhileAnOpenIsInFlight(t *testing.T) {
	rm := risk.NewManager(risk.Limits{}, decimal.NewFromInt(100))
	e := &BotTrader{InstID: "DOGE", Mode: "bot", RiskManager: rm}

	// Simulate being mid-open, exactly as evaluateStrategies does while it holds openMu.
	e.setOpenInFlight(true)

	remote := &domain.Position{InstID: "DOGE", PosSide: "short", Pos: decimal.NewFromInt(-45)}
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	if !e.shouldDeferUntrackedHalt(logger, remote) {
		t.Fatal("did not defer while an open was in flight — reconcile would halt ALL real " +
			"trading because of a position the system is deliberately opening")
	}
	// The halt must genuinely not have been tripped.
	if halted, reason := rm.Halted(); halted {
		t.Fatalf("trading halted: %s", reason)
	}
}

// TestReconcile_StillHaltsOnAGenuinelyUntrackedPosition is the other half. The guard must narrow
// the check to the in-flight window only — a position the exchange reports while nothing is being
// opened is real drift (a manual trade, a missed fill) and must still stop trading.
func TestReconcile_StillHaltsOnAGenuinelyUntrackedPosition(t *testing.T) {
	rm := risk.NewManager(risk.Limits{}, decimal.NewFromInt(100))
	e := &BotTrader{InstID: "DOGE", Mode: "bot", RiskManager: rm}

	// No open in flight — the default, and the genuine-drift case.
	remote := &domain.Position{InstID: "DOGE", PosSide: "short", Pos: decimal.NewFromInt(-45)}
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	if e.shouldDeferUntrackedHalt(logger, remote) {
		t.Fatal("deferred a genuinely untracked position — the guard is too broad, and real " +
			"drift (a manual trade, a missed fill) would go unnoticed")
	}
	_ = rm
}

// TestOpenInFlight_IsRaceFree runs the flag under -race across concurrent writers and readers,
// mirroring production: the open path sets it on one goroutine while reconcile reads it on another
// (the 5s poll and the WebSocket-pushed pass are both separate goroutines).
func TestOpenInFlight_IsRaceFree(t *testing.T) {
	e := &BotTrader{InstID: "DOGE"}
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			e.openMu.Lock()
			e.setOpenInFlight(true)
			e.setOpenInFlight(false)
			e.openMu.Unlock()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = e.isOpenInFlight()
		}
	}()
	wg.Wait()

	if e.isOpenInFlight() {
		t.Error("flag left set after all opens completed — reconcile would be suppressed forever")
	}
}
