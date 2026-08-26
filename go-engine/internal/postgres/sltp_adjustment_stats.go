package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// SLTPAdjustmentStats aggregates closed baseline-vs-rl_adjusted trades (CLAUDE.md §15.4's A/B
// comparison). Only baseline orders that have at least one rl_adjusted fork are included on the
// "baseline" side (the JOIN below), so a baseline order nobody ever proposed adjusting doesn't
// dilute the comparison either way.
func (r *Repository) SLTPAdjustmentStats(ctx context.Context, instID string, since time.Time) ([]port.VariantStats, error) {
	rows, err := r.pool.Query(ctx, `
		WITH adjusted_baselines AS (
			SELECT DISTINCT b.id
			FROM paper_orders b
			JOIN paper_orders f ON f.parent_order_id = b.id AND f.variant = 'rl_adjusted'
			WHERE ($1 = '' OR b.inst_id = $1) AND b.opened_at >= $2
		)
		SELECT 'baseline' AS variant,
			count(*) FILTER (WHERE closed_at IS NOT NULL) AS closed_count,
			count(*) FILTER (WHERE close_reason = 'tp') AS wins,
			count(*) FILTER (WHERE close_reason = 'sl') AS losses,
			coalesce(sum(realized_pnl) FILTER (WHERE closed_at IS NOT NULL), 0) AS realized_pnl
		FROM paper_orders
		WHERE id IN (SELECT id FROM adjusted_baselines)

		UNION ALL

		SELECT 'rl_adjusted' AS variant,
			count(*) FILTER (WHERE closed_at IS NOT NULL) AS closed_count,
			count(*) FILTER (WHERE close_reason = 'tp') AS wins,
			count(*) FILTER (WHERE close_reason = 'sl') AS losses,
			coalesce(sum(realized_pnl) FILTER (WHERE closed_at IS NOT NULL), 0) AS realized_pnl
		FROM paper_orders
		WHERE variant = 'rl_adjusted'
			AND parent_order_id IN (SELECT id FROM adjusted_baselines)
	`, instID, since)
	if err != nil {
		return nil, fmt.Errorf("sltp adjustment stats: %w", err)
	}
	defer rows.Close()

	var out []port.VariantStats
	for rows.Next() {
		var vs port.VariantStats
		if err := rows.Scan(&vs.Variant, &vs.ClosedCount, &vs.Wins, &vs.Losses, &vs.RealizedPnL); err != nil {
			return nil, fmt.Errorf("scan sltp adjustment stats: %w", err)
		}
		out = append(out, vs)
	}
	return out, rows.Err()
}

// ListSLTPAdjustmentPairs returns every baseline/rl_adjusted pair for instID (or all instruments
// if ""), most-recently-opened first (CLAUDE.md §15.4).
func (r *Repository) ListSLTPAdjustmentPairs(ctx context.Context, instID string) ([]port.SLTPAdjustmentPair, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT
			b.id, b.inst_id, b.strategy_id, b.side, b.entry_px, b.sl_px, b.tp_px, b.size, b.leverage,
			b.opened_at, b.closed_at, b.close_reason, b.close_px, b.realized_pnl, b.features_json, b.mode,
			b.parent_order_id, b.variant,
			f.id, f.inst_id, f.strategy_id, f.side, f.entry_px, f.sl_px, f.tp_px, f.size, f.leverage,
			f.opened_at, f.closed_at, f.close_reason, f.close_px, f.realized_pnl, f.features_json, f.mode,
			f.parent_order_id, f.variant
		FROM paper_orders b
		JOIN paper_orders f ON f.parent_order_id = b.id AND f.variant = 'rl_adjusted'
		WHERE ($1 = '' OR b.inst_id = $1)
		ORDER BY b.opened_at DESC
	`, instID)
	if err != nil {
		return nil, fmt.Errorf("list sltp adjustment pairs: %w", err)
	}
	defer rows.Close()

	var out []port.SLTPAdjustmentPair
	for rows.Next() {
		var b, f port.PaperOrder
		if err := rows.Scan(
			&b.ID, &b.InstID, &b.StrategyID, &b.Side, &b.EntryPx, &b.SLPx, &b.TPPx, &b.Size, &b.Leverage,
			&b.OpenedAt, &b.ClosedAt, &b.CloseReason, &b.ClosePx, &b.RealizedPnL, &b.FeaturesJSON, &b.Mode,
			&b.ParentOrderID, &b.Variant,
			&f.ID, &f.InstID, &f.StrategyID, &f.Side, &f.EntryPx, &f.SLPx, &f.TPPx, &f.Size, &f.Leverage,
			&f.OpenedAt, &f.ClosedAt, &f.CloseReason, &f.ClosePx, &f.RealizedPnL, &f.FeaturesJSON, &f.Mode,
			&f.ParentOrderID, &f.Variant,
		); err != nil {
			return nil, fmt.Errorf("scan sltp adjustment pair: %w", err)
		}
		out = append(out, port.SLTPAdjustmentPair{InstID: b.InstID, BaselineOrder: b, RLAdjustedOrder: f})
	}
	return out, rows.Err()
}
