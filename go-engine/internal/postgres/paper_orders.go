package postgres

import (
	"context"
	"fmt"

	"github.com/rez/okxBot/go-engine/internal/port"
)

// OpenPaperOrder inserts a new virtual trade and returns its id.
func (r *Repository) OpenPaperOrder(ctx context.Context, o port.PaperOrder) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO paper_orders (inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, features_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id
	`, o.InstID, o.StrategyID, o.Side, o.EntryPx, o.SLPx, o.TPPx, o.Size, o.Leverage, o.FeaturesJSON).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("open paper order for %s: %w", o.InstID, err)
	}
	return id, nil
}

// ClosePaperOrder marks a virtual trade closed with its realized outcome.
func (r *Repository) ClosePaperOrder(ctx context.Context, id int64, closePx float64, reason string, realizedPnL float64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE paper_orders
		SET closed_at = now(), close_px = $2, close_reason = $3, realized_pnl = $4
		WHERE id = $1
	`, id, closePx, reason, realizedPnL)
	if err != nil {
		return fmt.Errorf("close paper order %d: %w", id, err)
	}
	return nil
}

// ListOpenPaperOrders returns still-open virtual trades for an instrument.
func (r *Repository) ListOpenPaperOrders(ctx context.Context, instID string) ([]port.PaperOrder, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, opened_at, features_json
		FROM paper_orders
		WHERE inst_id = $1 AND closed_at IS NULL
		ORDER BY opened_at
	`, instID)
	if err != nil {
		return nil, fmt.Errorf("list open paper orders for %s: %w", instID, err)
	}
	defer rows.Close()

	var out []port.PaperOrder
	for rows.Next() {
		var o port.PaperOrder
		if err := rows.Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx, &o.Size, &o.Leverage, &o.OpenedAt, &o.FeaturesJSON); err != nil {
			return nil, fmt.Errorf("scan paper order: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
