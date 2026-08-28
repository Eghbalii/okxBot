package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

const accountEquityCols = `mode, initial_usd, equity_usd, reset_count, last_reset_at, updated_at`

// GetAccountEquity returns mode's current balance row, seeding it at initialUSD (plus a "seed"
// history point) if it doesn't exist yet — CLAUDE.md §15.6.
func (r *Repository) GetAccountEquity(ctx context.Context, mode string, initialUSD decimal.Decimal) (port.AccountEquity, error) {
	var ae port.AccountEquity
	// xmax = 0 identifies a row this statement actually inserted, as opposed to one the no-op
	// DO UPDATE just returned — that's what tells us whether to write the seed history point,
	// without a second round trip to check for existence first.
	var inserted bool
	err := r.pool.QueryRow(ctx, `
		INSERT INTO account_equity (mode, initial_usd, equity_usd)
		VALUES ($1, $2, $2)
		ON CONFLICT (mode) DO UPDATE SET mode = account_equity.mode
		RETURNING `+accountEquityCols+`, (xmax = 0)
	`, mode, initialUSD).Scan(&ae.Mode, &ae.InitialUSD, &ae.EquityUSD, &ae.ResetCount, &ae.LastResetAt, &ae.UpdatedAt, &inserted)
	if err != nil {
		return port.AccountEquity{}, fmt.Errorf("get account equity for mode %s: %w", mode, err)
	}

	if inserted {
		// Best-effort: the balance row itself is authoritative, so failing to write the opening
		// point of the chart must not fail the caller's read.
		if _, err := r.pool.Exec(ctx, `
			INSERT INTO account_equity_history (mode, equity_usd, delta_usd, reason)
			VALUES ($1, $2, 0, 'seed')
		`, mode, ae.EquityUSD); err != nil {
			return ae, nil
		}
	}
	return ae, nil
}

// ApplyRealizedPnL adds pnl to mode's running balance, recording an EquityPoint for it, and resets
// a drained balance back to InitialUSD unless mode is "real" — CLAUDE.md §15.6/§15.7. Real money
// never auto-tops-up: running out is a stop condition for a human, not a bookkeeping event.
//
// Runs in one transaction so a balance change and its history point can't diverge: a chart built
// from history rows that's missing the very drop that drained the account would be actively
// misleading, which is the failure this whole timeline exists to prevent.
func (r *Repository) ApplyRealizedPnL(
	ctx context.Context,
	mode string,
	pnl decimal.Decimal,
	orderID *int64,
	instID string,
) (port.AccountEquity, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("begin apply realized pnl: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit succeeds

	var ae port.AccountEquity
	err = tx.QueryRow(ctx, `
		UPDATE account_equity
		SET equity_usd = equity_usd + $2, updated_at = now()
		WHERE mode = $1
		RETURNING `+accountEquityCols, mode, pnl).
		Scan(&ae.Mode, &ae.InitialUSD, &ae.EquityUSD, &ae.ResetCount, &ae.LastResetAt, &ae.UpdatedAt)
	if err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("apply realized pnl for mode %s: %w", mode, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, equity_usd, delta_usd, reason, order_id, inst_id)
		VALUES ($1, $2, $3, 'trade', $4, $5)
	`, mode, ae.EquityUSD, pnl, orderID, nullableText(instID)); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("record equity history for mode %s: %w", mode, err)
	}

	// Real-money mode is deliberately excluded from the reset: see §15.7's carve-out.
	if ae.EquityUSD.Sign() > 0 || mode == "real" {
		if err := tx.Commit(ctx); err != nil {
			return port.AccountEquity{}, false, fmt.Errorf("commit apply realized pnl: %w", err)
		}
		return ae, false, nil
	}

	drained := ae.EquityUSD
	err = tx.QueryRow(ctx, `
		UPDATE account_equity
		SET equity_usd = initial_usd,
			reset_count = reset_count + 1,
			last_reset_at = now(),
			updated_at = now()
		WHERE mode = $1
		RETURNING `+accountEquityCols, mode).
		Scan(&ae.Mode, &ae.InitialUSD, &ae.EquityUSD, &ae.ResetCount, &ae.LastResetAt, &ae.UpdatedAt)
	if err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("reset drained account for mode %s: %w", mode, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_equity_history (mode, equity_usd, delta_usd, reason, inst_id)
		VALUES ($1, $2, $3, 'reset', $4)
	`, mode, ae.EquityUSD, ae.EquityUSD.Sub(drained), nullableText(instID)); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("record equity reset for mode %s: %w", mode, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return port.AccountEquity{}, false, fmt.Errorf("commit account reset: %w", err)
	}
	return ae, true, nil
}

// ListEquityHistory returns mode's balance timeline oldest-first, ready to plot directly.
func (r *Repository) ListEquityHistory(ctx context.Context, mode string, since time.Time, limit int) ([]port.EquityPoint, error) {
	// Ordered DESC with the LIMIT so a capped read returns the MOST RECENT points (a chart wants
	// the latest window, not the first N rows ever written), then reversed to oldest-first below.
	q := `
		SELECT id, mode, equity_usd, delta_usd, reason, order_id, COALESCE(inst_id, ''), created_at
		FROM account_equity_history
		WHERE mode = $1 AND ($2::timestamptz IS NULL OR created_at >= $2)
		ORDER BY created_at DESC, id DESC`
	args := []any{mode, nullableTime(since)}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list equity history for mode %s: %w", mode, err)
	}
	defer rows.Close()

	var out []port.EquityPoint
	for rows.Next() {
		var p port.EquityPoint
		if err := rows.Scan(&p.ID, &p.Mode, &p.EquityUSD, &p.DeltaUSD, &p.Reason, &p.OrderID, &p.InstID, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan equity history row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate equity history: %w", err)
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
