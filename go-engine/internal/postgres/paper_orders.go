package postgres

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// OpenPaperOrder inserts a new virtual trade and returns its id.
func (r *Repository) OpenPaperOrder(ctx context.Context, o port.PaperOrder) (int64, error) {
	mode := o.Mode
	if mode == "" {
		mode = "paper"
	}
	variant := o.Variant
	if variant == "" {
		variant = "baseline"
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO paper_orders (inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, features_json, mode, parent_order_id, variant, bar)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id
	`, o.InstID, o.StrategyID, o.Side, o.EntryPx, o.SLPx, o.TPPx, o.Size, o.Leverage, o.FeaturesJSON, mode, o.ParentOrderID, variant, o.Bar).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("open paper order for %s: %w", o.InstID, err)
	}
	return id, nil
}

// ClosePaperOrder marks a virtual trade closed with its realized outcome.
func (r *Repository) ClosePaperOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal) error {
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

// UpdatePaperOrderSLTP applies an in-trade SL/TP adjustment to an open order (CLAUDE.md §15.4).
// The caller must have already run the proposed prices through the ratchet clamp
// (usecase.RatchetSLTP) — this only persists whatever it's given, scoped to still-open orders so a
// stale/late adjustment can never resurrect a since-closed order's SL/TP.
func (r *Repository) UpdatePaperOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE paper_orders
		SET sl_px = $2, tp_px = $3
		WHERE id = $1 AND closed_at IS NULL
	`, id, slPx, tpPx)
	if err != nil {
		return fmt.Errorf("update paper order %d SL/TP: %w", id, err)
	}
	return nil
}

// ListOpenPaperOrders returns still-open virtual trades for an instrument (both baseline and
// rl_adjusted-fork variants — the SL/TP monitor loop treats them uniformly, each hits its own
// SL/TP independently; only reward/budget attribution filters by Variant, CLAUDE.md §15.4).
func (r *Repository) ListOpenPaperOrders(ctx context.Context, instID string) ([]port.PaperOrder, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, opened_at, features_json, parent_order_id, variant, pnl_max_pct, pnl_min_pct, bar, manual_close_requested
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
		var bar *string
		if err := rows.Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx, &o.Size, &o.Leverage, &o.OpenedAt, &o.FeaturesJSON, &o.ParentOrderID, &o.Variant, &o.PnLMaxPct, &o.PnLMinPct, &bar, &o.ManualCloseRequested); err != nil {
			return nil, fmt.Errorf("scan paper order: %w", err)
		}
		if bar != nil {
			o.Bar = *bar
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RequestManualClose flags an open order for PaperTrader to close on its next tick (CLAUDE.md
// 2026-08-31 panel Close button). Errors if id is not currently open — closing an already-closed
// or nonexistent order silently would hide a stale/duplicate request from the panel.
func (r *Repository) RequestManualClose(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE paper_orders SET manual_close_requested = true
		WHERE id = $1 AND closed_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("request manual close for order %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("order %d is not open", id)
	}
	return nil
}

// positionSortColumns maps PositionFilter.SortBy to a safe, allowlisted SQL column/expression —
// never interpolate the caller-provided SortBy string directly into the query. Default changed to
// closed_at (2026-09-02): a closed-position query is what actually got slow as the table grew,
// and the panel's own default view for that filter is "most recently closed first" — opened_at
// was never the interesting axis there.
var positionSortColumns = map[string]string{
	"":          "closed_at",
	"opened_at": "opened_at",
	"closed_at": "closed_at",
	"pnl":       "realized_pnl",
	"inst_id":   "inst_id",
}

// ListPositions returns positions across trading modes for the panel (CLAUDE.md §11.4), filtered,
// sorted, and paged per f. Backs paper trading today; demo/real rows land in the same table (mode
// column) once cmd/trader writes them, so this query needs no changes when that lands.
//
// f.Limit/Offset (added 2026-09-02): with closed positions numbering in the hundreds, the panel's
// old unbounded fetch-everything-then-paginate-client-side approach became a genuinely slow query
// and a multi-MB payload (FeaturesJSON alone averages ~3.8KB/row) on every 5s poll. Limit<=0 keeps
// the old unbounded behavior for callers that need the full set (e.g. an open-positions-only scan,
// which is a small row count regardless).
func (r *Repository) ListPositions(ctx context.Context, f port.PositionFilter) ([]port.PaperOrder, error) {
	col, ok := positionSortColumns[f.SortBy]
	if !ok {
		return nil, fmt.Errorf("list positions: unknown sort field %q", f.SortBy)
	}
	dir := "ASC"
	if f.SortDesc {
		dir = "DESC"
	}
	// NULLS LAST keeps still-open positions (closed_at/realized_pnl NULL) from jumping to the top
	// of a DESC sort, which would otherwise bury every closed position the panel wants to compare.
	orderClause := fmt.Sprintf("%s %s NULLS LAST", col, dir)

	// LEFT JOIN, not JOIN: an order with no strategy attribution (strategy_id NULL) or one whose
	// strategy row was later deleted must still show up in the positions list — it just renders
	// with an empty strategy name rather than disappearing from the panel entirely.
	query := `
		SELECT po.id, po.inst_id, po.strategy_id, po.side, po.entry_px, po.sl_px, po.tp_px, po.size, po.leverage,
			po.opened_at, po.closed_at, po.close_reason, po.close_px, po.realized_pnl, po.features_json, po.mode,
			po.parent_order_id, po.variant, po.bar, COALESCE(s.name, '')
		FROM paper_orders po
		LEFT JOIN strategies s ON s.id = po.strategy_id
		WHERE ($1 = '' OR po.mode = $1)
			AND ($2 = '' OR po.inst_id = $2)
			AND ($3::boolean IS NULL OR (po.closed_at IS NULL) = $3)
		ORDER BY ` + orderClause

	args := []any{f.Mode, f.InstID, f.Open}
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		args = append(args, f.Limit, f.Offset)
	}

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list positions: %w", err)
	}
	defer rows.Close()

	var out []port.PaperOrder
	for rows.Next() {
		var o port.PaperOrder
		var bar *string
		if err := rows.Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx,
			&o.Size, &o.Leverage, &o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx,
			&o.RealizedPnL, &o.FeaturesJSON, &o.Mode, &o.ParentOrderID, &o.Variant, &bar, &o.StrategyName); err != nil {
			return nil, fmt.Errorf("scan position: %w", err)
		}
		if bar != nil {
			o.Bar = *bar
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CountPositions returns how many rows f's Mode/InstID/Open filters match — what the panel's
// pagination control needs to compute total page count without pulling every row back.
func (r *Repository) CountPositions(ctx context.Context, f port.PositionFilter) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM paper_orders po
		WHERE ($1 = '' OR po.mode = $1)
			AND ($2 = '' OR po.inst_id = $2)
			AND ($3::boolean IS NULL OR (po.closed_at IS NULL) = $3)
	`, f.Mode, f.InstID, f.Open).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count positions: %w", err)
	}
	return count, nil
}

// UpdatePaperOrderPnLExtremes advances an open order's peak/trough unrealized PnL (CLAUDE.md
// §15.11). GREATEST/LEAST are applied in SQL rather than in Go so a concurrent writer can never
// walk a high-water mark backwards — the tick handler and any other caller may both be updating
// the same order, and a read-modify-write in Go would lose whichever update landed first.
func (r *Repository) UpdatePaperOrderPnLExtremes(ctx context.Context, id int64, maxPct, minPct decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE paper_orders
		SET pnl_max_pct = GREATEST(pnl_max_pct, $2),
			pnl_min_pct = LEAST(pnl_min_pct, $3)
		WHERE id = $1 AND closed_at IS NULL
	`, id, maxPct, minPct)
	if err != nil {
		return fmt.Errorf("update pnl extremes for paper order %d: %w", id, err)
	}
	return nil
}
