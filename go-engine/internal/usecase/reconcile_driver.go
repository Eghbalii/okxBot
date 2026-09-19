package usecase

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// ReconcileDriver runs ONE account-wide reconciliation pass on behalf of every instrument's engine,
// replacing the per-engine polling that issued the same two account-scoped calls once per token
// (2026-09-10). See AccountSnapshot for the full reasoning; in short, a 10-token roster was making
// 20 calls per cycle where 2 carry the same information, and OKX rate-limited it (CLAUDE.md §38.2).
//
// It owns only the FETCH and the cadence. Every decision about a position stays in
// BotTrader.ReconcileWith, so there remains exactly one definition of how this system responds to
// a position change — the same reason the WebSocket push routes through reconcile rather than
// handling positions itself (CLAUDE.md §35.4).
type ReconcileDriver struct {
	// Engines is keyed by short symbol, matching cmd/trader's own engines map.
	Engines map[string]*BotTrader

	Exchange port.ExchangeClient
	Logger   *slog.Logger

	// InstType/SettleCcy are the account-wide query arguments. They come from config rather than
	// from an arbitrary engine so the driver does not silently inherit one token's overrides.
	InstType  string
	SettleCcy string

	// Interval is the poll cadence. Zero falls back to DefaultReconcileInterval.
	Interval time.Duration

	// mu serializes whole passes against each other, so a WebSocket-triggered pass and the periodic
	// one cannot interleave their fetches. Each engine still holds its own reconcileMu underneath;
	// this one exists so a burst of pushes cannot multiply the account reads this type exists to
	// reduce.
	mu sync.Mutex
}

func (d *ReconcileDriver) interval() time.Duration {
	if d.Interval > 0 {
		return d.Interval
	}
	return DefaultReconcileInterval
}

func (d *ReconcileDriver) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// Run polls until ctx is cancelled, reconciling every engine against one shared snapshot per tick.
func (d *ReconcileDriver) Run(ctx context.Context) error {
	ticker := time.NewTicker(d.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			d.ReconcileAll(ctx)
		}
	}
}

// ReconcileAll fetches one snapshot and reconciles every engine against it.
func (d *ReconcileDriver) ReconcileAll(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()

	logger := d.logger()
	snap, err := FetchAccountSnapshot(d.Exchange, d.InstType, d.SettleCcy)
	if err != nil {
		// Every engine's drift comparison is derived from this response, so a failed fetch skips the
		// whole pass rather than letting an absent position list read as "the exchange reports
		// flat" — the branch that closes local positions.
		logger.Warn("reconcile driver: account snapshot failed; skipping this pass", "error", err)
		return
	}

	for _, engine := range d.Engines {
		engine.ReconcileWith(ctx, snap, logger)
	}

	// Once per snapshot, not once per engine: this writes a single shared account row, and letting
	// each engine write it made N transactions for one row's work and attributed an account-wide
	// balance change to whichever token's engine happened to run first.
	d.recordEquityOnce(ctx, snap, logger)
}

// ReconcileInstrument reconciles a single instrument against a FRESHLY fetched snapshot. This is
// the WebSocket push path: a push means that instrument genuinely changed, so it is worth the two
// account calls to act on current data rather than reusing a snapshot up to a full interval old —
// reusing the stale one would give up exactly the latency the socket exists to provide.
func (d *ReconcileDriver) ReconcileInstrument(ctx context.Context, instID string) bool {
	engine, ok := d.Engines[instID]
	if !ok {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	logger := d.logger()
	snap, err := FetchAccountSnapshot(d.Exchange, d.InstType, d.SettleCcy)
	if err != nil {
		logger.Warn("reconcile driver: account snapshot failed for a pushed event", "instId", instID, "error", err)
		return false
	}
	engine.ReconcileWith(ctx, snap, logger)
	d.recordEquityOnce(ctx, snap, logger)
	return true
}

// recordEquityOnce writes the equity timeline through any one engine — the write targets a shared
// account row and takes no per-instrument state, so which engine carries the call does not matter;
// that it happens exactly once per snapshot does.
func (d *ReconcileDriver) recordEquityOnce(ctx context.Context, snap AccountSnapshot, logger *slog.Logger) {
	for _, engine := range d.Engines {
		engine.RecordEquity(ctx, snap, logger)
		return
	}
}
