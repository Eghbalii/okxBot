package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// OpenRealOrder inserts a new real-money order and returns its id. o.Status must be set explicitly
// by the caller (normally "pending" — see port.RealOrder's doc comment).
func (r *Repository) OpenRealOrder(ctx context.Context, o port.RealOrder) (int64, error) {
	status := o.Status
	if status == "" {
		status = "pending"
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO real_orders (inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, features_json, status, bar, exchange_order_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id
	`, o.InstID, o.StrategyID, o.Side, o.EntryPx, o.SLPx, o.TPPx, o.Size, o.Leverage, o.FeaturesJSON, status, o.Bar, o.ExchangeOrderID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("open real order for %s: %w", o.InstID, err)
	}
	return id, nil
}

// GetRealOrder fetches a single real order by id, open or closed. Mirrors GetPaperOrder.
func (r *Repository) GetRealOrder(ctx context.Context, id int64) (port.RealOrder, error) {
	var o port.RealOrder
	var bar *string
	err := r.pool.QueryRow(ctx, `
		SELECT id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, opened_at,
			closed_at, close_reason, close_px, realized_pnl, features_json, status, bar,
			pnl_max_pct, pnl_min_pct, manual_close_requested, exchange_order_id, exchange_algo_order_id
		FROM real_orders WHERE id = $1
	`, id).Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx, &o.Size,
		&o.Leverage, &o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx, &o.RealizedPnL,
		&o.FeaturesJSON, &o.Status, &bar, &o.PnLMaxPct, &o.PnLMinPct,
		&o.ManualCloseRequested, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID)
	if err != nil {
		return port.RealOrder{}, fmt.Errorf("get real order %d: %w", id, err)
	}
	if bar != nil {
		o.Bar = *bar
	}
	return o, nil
}

// UpdateRealOrderStatus transitions a real order's fill status once PlaceOrder's outcome is known.
// entryPx/size are nil-able: a "canceled" transition passes neither (nothing to correct); a
// "filled"/"partial" transition passes both, correcting the provisional pre-fill price/size to the
// exchange-confirmed values.
func (r *Repository) UpdateRealOrderStatus(ctx context.Context, id int64, status string, entryPx *decimal.Decimal, size *decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders
		SET status = $2,
			entry_px = COALESCE($3, entry_px),
			size = COALESCE($4, size)
		WHERE id = $1
	`, id, status, entryPx, size)
	if err != nil {
		return fmt.Errorf("update real order %d status: %w", id, err)
	}
	return nil
}

// SetRealOrderFeatures records the decision-time observation snapshot, mirroring how FeaturesJSON
// is set on a paper order.
func (r *Repository) SetRealOrderFeatures(ctx context.Context, id int64, featuresJSON json.RawMessage) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders SET features_json = $2 WHERE id = $1
	`, id, featuresJSON)
	if err != nil {
		return fmt.Errorf("set real order %d features: %w", id, err)
	}
	return nil
}

// SetRealOrderExchangeAlgoOrderID mirrors SetExchangeAlgoOrderID for real_orders.
func (r *Repository) SetRealOrderExchangeAlgoOrderID(ctx context.Context, id int64, algoOrderID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders SET exchange_algo_order_id = $2 WHERE id = $1
	`, id, algoOrderID)
	if err != nil {
		return fmt.Errorf("set exchange algo order id for real order %d: %w", id, err)
	}
	return nil
}

// CloseRealOrder marks a real order closed with its realized outcome. Mirrors ClosePaperOrder.
func (r *Repository) CloseRealOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders
		SET closed_at = now(), close_px = $2, close_reason = $3, realized_pnl = $4
		WHERE id = $1
	`, id, closePx, reason, realizedPnL)
	if err != nil {
		return fmt.Errorf("close real order %d: %w", id, err)
	}
	return nil
}

// UpdateRealOrderSLTP applies an in-trade SL/TP adjustment to an open real order. Mirrors
// UpdatePaperOrderSLTP — the caller is responsible for any clamping before calling this.
func (r *Repository) UpdateRealOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders
		SET sl_px = $2, tp_px = $3
		WHERE id = $1 AND closed_at IS NULL
	`, id, slPx, tpPx)
	if err != nil {
		return fmt.Errorf("update real order %d SL/TP: %w", id, err)
	}
	return nil
}

// ListOpenRealOrders returns still-open real positions for an instrument — restricted to
// status IN ('filled','partial') so an in-flight pending order is never treated as an open
// position (it isn't one yet).
func (r *Repository) ListOpenRealOrders(ctx context.Context, instID string) ([]port.RealOrder, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, opened_at, features_json, status, pnl_max_pct, pnl_min_pct, bar, manual_close_requested, exchange_order_id, exchange_algo_order_id
		FROM real_orders
		WHERE inst_id = $1 AND closed_at IS NULL AND status IN ('filled', 'partial')
		ORDER BY opened_at
	`, instID)
	if err != nil {
		return nil, fmt.Errorf("list open real orders for %s: %w", instID, err)
	}
	defer rows.Close()

	var out []port.RealOrder
	for rows.Next() {
		var o port.RealOrder
		var bar *string
		if err := rows.Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx, &o.Size, &o.Leverage, &o.OpenedAt, &o.FeaturesJSON, &o.Status, &o.PnLMaxPct, &o.PnLMinPct, &bar, &o.ManualCloseRequested, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID); err != nil {
			return nil, fmt.Errorf("scan real order: %w", err)
		}
		if bar != nil {
			o.Bar = *bar
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RequestRealManualClose flags an open real order for RealTrader to close on its next tick.
// Mirrors RequestManualClose. Errors if id is not currently open.
func (r *Repository) RequestRealManualClose(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE real_orders SET manual_close_requested = true
		WHERE id = $1 AND closed_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("request manual close for real order %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("real order %d is not open", id)
	}
	return nil
}

// UpdateRealOrderPnLExtremes advances an open real order's peak/trough unrealized PnL. Mirrors
// UpdatePaperOrderPnLExtremes.
func (r *Repository) UpdateRealOrderPnLExtremes(ctx context.Context, id int64, maxPct, minPct decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders
		SET pnl_max_pct = GREATEST(pnl_max_pct, $2),
			pnl_min_pct = LEAST(pnl_min_pct, $3)
		WHERE id = $1 AND closed_at IS NULL
	`, id, maxPct, minPct)
	if err != nil {
		return fmt.Errorf("update pnl extremes for real order %d: %w", id, err)
	}
	return nil
}

// ListRealPositions returns real positions (open and/or closed) for the panel, filtered/sorted/
// paged per f — mirrors ListPositions. f.Mode is ignored (every row is real by construction).
// f.Open filters against ClosedAt, same as ListPositions; a "canceled" order (never filled) has
// ClosedAt NULL forever, so it reads as "open" under a naive filter — excluded here by requiring
// status IN ('filled','partial') whenever f.Open is true, so a canceled attempt never occupies a
// slot in the "open positions" view. An f.Open == false or nil query is unaffected and still
// returns canceled rows (visible under "closed"/"all", per the panel's Status badge design).
func (r *Repository) ListRealPositions(ctx context.Context, f port.PositionFilter) ([]port.RealOrder, error) {
	col, ok := positionSortColumns[f.SortBy]
	if !ok {
		return nil, fmt.Errorf("list real positions: unknown sort field %q", f.SortBy)
	}
	dir := "ASC"
	if f.SortDesc {
		dir = "DESC"
	}
	orderClause := fmt.Sprintf("%s %s NULLS LAST", col, dir)

	query := `
		SELECT ro.id, ro.inst_id, ro.strategy_id, ro.side, ro.entry_px, ro.sl_px, ro.tp_px, ro.size, ro.leverage,
			ro.opened_at, ro.closed_at, ro.close_reason, ro.close_px, ro.realized_pnl, ro.features_json, ro.status,
			ro.bar, ro.pnl_max_pct, ro.pnl_min_pct, COALESCE(s.name, ''),
			ro.exchange_order_id, ro.exchange_algo_order_id, ro.manual_close_requested
		FROM real_orders ro
		LEFT JOIN strategies s ON s.id = ro.strategy_id
		WHERE ($1 = '' OR ro.inst_id = $1)
			AND ($2::boolean IS NULL OR (ro.closed_at IS NULL AND (NOT $2 OR ro.status IN ('filled','partial'))) = $2)
		ORDER BY ` + orderClause

	args := []any{f.InstID, f.Open}
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		args = append(args, f.Limit, f.Offset)
	}

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list real positions: %w", err)
	}
	defer rows.Close()

	var out []port.RealOrder
	for rows.Next() {
		var o port.RealOrder
		var bar *string
		if err := rows.Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx,
			&o.Size, &o.Leverage, &o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx,
			&o.RealizedPnL, &o.FeaturesJSON, &o.Status, &bar,
			&o.PnLMaxPct, &o.PnLMinPct, &o.StrategyName, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID,
			&o.ManualCloseRequested); err != nil {
			return nil, fmt.Errorf("scan real position: %w", err)
		}
		if bar != nil {
			o.Bar = *bar
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CountRealPositions mirrors CountPositions, same open-filter semantics as ListRealPositions.
func (r *Repository) CountRealPositions(ctx context.Context, f port.PositionFilter) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM real_orders ro
		WHERE ($1 = '' OR ro.inst_id = $1)
			AND ($2::boolean IS NULL OR (ro.closed_at IS NULL AND (NOT $2 OR ro.status IN ('filled','partial'))) = $2)
	`, f.InstID, f.Open).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count real positions: %w", err)
	}
	return count, nil
}
