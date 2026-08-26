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
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO paper_orders (inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, features_json, mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`, o.InstID, o.StrategyID, o.Side, o.EntryPx, o.SLPx, o.TPPx, o.Size, o.Leverage, o.FeaturesJSON, mode).Scan(&id)
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

// positionSortColumns maps PositionFilter.SortBy to a safe, allowlisted SQL column/expression —
// never interpolate the caller-provided SortBy string directly into the query.
var positionSortColumns = map[string]string{
	"":          "opened_at",
	"opened_at": "opened_at",
	"closed_at": "closed_at",
	"pnl":       "realized_pnl",
	"inst_id":   "inst_id",
}

// ListPositions returns positions across trading modes for the panel (CLAUDE.md §11.4), filtered
// and sorted per f. Backs paper trading today; demo/real rows land in the same table (mode
// column) once cmd/trader writes them, so this query needs no changes when that lands.
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

	query := `
		SELECT id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage,
			opened_at, closed_at, close_reason, close_px, realized_pnl, features_json, mode
		FROM paper_orders
		WHERE ($1 = '' OR mode = $1)
			AND ($2 = '' OR inst_id = $2)
			AND ($3::boolean IS NULL OR (closed_at IS NULL) = $3)
		ORDER BY ` + orderClause

	var openParam *bool
	if f.Open != nil {
		openParam = f.Open
	}
	rows, err := r.pool.Query(ctx, query, f.Mode, f.InstID, openParam)
	if err != nil {
		return nil, fmt.Errorf("list positions: %w", err)
	}
	defer rows.Close()

	var out []port.PaperOrder
	for rows.Next() {
		var o port.PaperOrder
		if err := rows.Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx,
			&o.Size, &o.Leverage, &o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx,
			&o.RealizedPnL, &o.FeaturesJSON, &o.Mode); err != nil {
			return nil, fmt.Errorf("scan position: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
