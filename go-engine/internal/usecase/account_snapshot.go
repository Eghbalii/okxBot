package usecase

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// AccountSnapshot is one account-wide read of the exchange's own position and balance state,
// shared by every instrument's reconciliation pass in the same cycle (2026-09-10).
//
// The problem it solves: GetPositions and GetBalance are ACCOUNT-scoped — GetPositions takes an
// instType (not an instrument) and returns every open position on the account; GetBalance takes a
// settlement currency. Every engine called them with identical arguments and received identical
// responses, so a 10-token roster issued 20 calls per cycle where 2 would do. That surfaced as
// OKX 50011 "Too Many Requests" against /account/positions and /account/balance (CLAUDE.md §38.2),
// which is not a cosmetic problem: a pass that cannot read positions cannot detect drift, and one
// that cannot read the protective order falls back to recording a close as manual.
//
// Fetch once, pass it to each engine's ReconcileWith. The per-instrument work that genuinely IS
// per-instrument — verifying each open position's own protective order via GetAlgoOrder — is
// unaffected and still runs per engine.
type AccountSnapshot struct {
	// Positions is every open position the exchange reports for the polled instType, unfiltered.
	Positions []domain.Position

	// Balance is the account's raw reported equity in the polled settlement currency. Raw, NOT run
	// through tradableEquity — RecordExchangeBalance applies the reserve itself as a derived view,
	// and pre-subtracting it here would recreate CLAUDE.md §32's incident where the reserve was
	// read as a realized trade loss.
	Balance domain.Balance

	// HasBalance distinguishes "the exchange reported no balance row" from a genuine zero, so a
	// caller never records an empty response as the account having been drained.
	HasBalance bool
}

// PositionFor returns the open position matching execInstID, or nil when the exchange reports the
// instrument flat. A zero-size row counts as flat: OKX keeps returning a position row after it
// closes, with Pos zeroed, and treating that as an open position would make every closed position
// look like it was still running.
func (s AccountSnapshot) PositionFor(execInstID string) *domain.Position {
	for i := range s.Positions {
		if s.Positions[i].InstID == execInstID && !s.Positions[i].Pos.IsZero() {
			return &s.Positions[i]
		}
	}
	return nil
}

// FetchAccountSnapshot performs the two account-wide reads.
//
// A positions failure is fatal to the pass and returns an error: every branch of the drift
// comparison is derived from that response, and acting on an absent one would read as "the
// exchange reports flat", which is exactly the state that closes local positions. A BALANCE
// failure is not fatal — it only costs the equity timeline one sample, and it must never stop the
// protective-order verification that shares this pass.
func FetchAccountSnapshot(exchange port.ExchangeClient, instType, settleCcy string) (AccountSnapshot, error) {
	positions, err := exchange.GetPositions(instType)
	if err != nil {
		return AccountSnapshot{}, fmt.Errorf("get positions: %w", err)
	}
	snap := AccountSnapshot{Positions: positions}
	if balances, err := exchange.GetBalance(settleCcy); err == nil && len(balances) > 0 {
		snap.Balance = balances[0]
		snap.HasBalance = true
	}
	return snap, nil
}

// RecordEquity writes the snapshot's balance to the account's equity timeline.
//
// Called ONCE per snapshot by the driver, not once per engine. It writes a single shared
// account_equity row, so N engines calling it wrote N transactions for one row's worth of work —
// and when the delta was nonzero, whichever engine happened to run first stamped its own instID on
// the history row, making an account-wide balance change look attributable to one arbitrary token.
func (e *RealTrader) RecordEquity(ctx context.Context, snap AccountSnapshot, logger *slog.Logger) {
	if !snap.HasBalance {
		return
	}
	if logger == nil {
		logger = e.Logger
	}
	if logger == nil {
		logger = slog.Default()
	}
	e.recordEquityReal(ctx, snap.Balance.Eq, logger)
}
