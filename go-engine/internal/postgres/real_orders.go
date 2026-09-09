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
			pnl_max_pct, pnl_min_pct, manual_close_requested, exchange_order_id, exchange_algo_order_id,
			manual_override
		FROM real_orders WHERE id = $1
	`, id).Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx, &o.Size,
		&o.Leverage, &o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx, &o.RealizedPnL,
		&o.FeaturesJSON, &o.Status, &bar, &o.PnLMaxPct, &o.PnLMinPct,
		&o.ManualCloseRequested, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID, &o.ManualOverride)
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

// SetRealOrderClosing marks a close as IN FLIGHT: it records the flattening order's id and moves
// the row to status='closing', while deliberately leaving closed_at NULL. The position stays open
// until the exchange confirms the flatten actually filled (CloseRealOrderConfirmed below) — real
// order 3 was recorded closed with nothing having verified OKX agreed, which is exactly the gap
// this split exists to close. A close that fails or times out therefore leaves a row that is
// visibly stuck in 'closing' rather than a row that lies about being flat.
func (r *Repository) SetRealOrderClosing(ctx context.Context, id int64, closeOrderID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders
		SET status = 'closing',
			exchange_close_order_id = $2,
			last_error = NULL,
			last_error_at = NULL
		WHERE id = $1
	`, id, nullableText(closeOrderID))
	if err != nil {
		return fmt.Errorf("mark real order %d closing: %w", id, err)
	}
	return nil
}

// CloseRealOrderConfirmed records a close the exchange has confirmed, storing the EXCHANGE's own
// numbers alongside our own (2026-09-08 request). exchangePnL/exchangeFee/exchangeClosePx are
// nil-able: a nil means OKX did not report that figure, which must stay distinguishable from a
// genuine zero, and the panel falls back to the locally computed value only in that case.
//
// realizedPnL (the locally computed figure) is still written so the two remain comparable — a
// persistent gap between them is itself worth seeing, since it means the local model of fees or
// fill price is wrong.
func (r *Repository) CloseRealOrderConfirmed(ctx context.Context, id int64, closePx decimal.Decimal, reason string,
	realizedPnL decimal.Decimal, exchangePnL, exchangeFee, exchangeClosePx *decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders
		SET closed_at = now(),
			close_px = $2,
			close_reason = $3,
			realized_pnl = $4,
			exchange_realized_pnl = $5,
			exchange_fee = $6,
			exchange_close_px = $7,
			-- Back to a settled fill state. SetRealOrderClosing moved the row to 'closing' while the
			-- flatten was in flight, and leaving it there after the exchange confirmed the fill made
			-- a completed close read as permanently stuck in the panel (observed on real order 5:
			-- closed_at set, no error, flat on OKX, still displaying "closing").
			--
			-- Unconditional rather than only-when-'closing': the open-side fill state this would
			-- otherwise preserve is already gone by this point, since SetRealOrderClosing overwrote
			-- it on the way in. Faithfully recording that a position opened partially filled needs a
			-- column of its own, not a status this path can restore — and status on a CLOSED row is
			-- display-only (nothing branches on it), so claiming 'partial' here would be inventing
			-- information rather than keeping it.
			status = 'filled',
			last_error = NULL,
			last_error_at = NULL
		WHERE id = $1
	`, id, closePx, reason, realizedPnL, exchangePnL, exchangeFee, exchangeClosePx)
	if err != nil {
		return fmt.Errorf("confirm close of real order %d: %w", id, err)
	}
	return nil
}

// SetRealOrderError records the most recent exchange failure for an order so the panel can raise it
// to a human. Deliberately does NOT change status: a failed close leaves the row in 'closing' and a
// failed open leaves it in 'opening', which is what makes a stuck order visible rather than one
// that quietly reverts to looking normal.
func (r *Repository) SetRealOrderError(ctx context.Context, id int64, message string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders SET last_error = $2, last_error_at = now() WHERE id = $1
	`, id, message)
	if err != nil {
		return fmt.Errorf("set error on real order %d: %w", id, err)
	}
	return nil
}

// ClearRealOrderError clears a recorded error once the condition has resolved, so a stale failure
// does not keep alarming in the panel after a later attempt succeeded.
func (r *Repository) ClearRealOrderError(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders SET last_error = NULL, last_error_at = NULL WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("clear error on real order %d: %w", id, err)
	}
	return nil
}

// UpdateRealOrderSLTP applies an in-trade SL/TP adjustment to an open real order. Mirrors
// UpdatePaperOrderSLTP — the caller is responsible for any clamping before calling this.
//
// manualOverride is true only for the operator's own edit (handleAdjustPosition) and false for
// every model-driven adjustment (RealTrader.applyRealAdjustment) — set in the SAME statement as
// the SL/TP write so the two can never observe a gap between "levels changed" and "locked from the
// model" (2026-09-06 request). Once true it is never cleared here: only an operator action should
// be able to hand control back to the model, and there is no such action yet.
func (r *Repository) UpdateRealOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal, manualOverride bool) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE real_orders
		SET sl_px = $2, tp_px = $3, manual_override = manual_override OR $4
		WHERE id = $1 AND closed_at IS NULL
	`, id, slPx, tpPx, manualOverride)
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
		-- 'closing' is included deliberately: a flatten is in flight but unconfirmed, so the
		-- position is still REAL and still needs monitoring. Excluding it here would make the
		-- engine forget a position that may well still be open on the exchange — the exact failure
		-- this whole confirmation flow exists to prevent.
		WHERE inst_id = $1 AND closed_at IS NULL AND status IN ('filled', 'partial', 'closing')
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

// RequestRealManualCloseAll flags every currently-open real order for close, the bulk form of
// RequestRealManualClose — mirrors RequestManualCloseAll. Used by the panel's Stop button
// (operator decision, 2026-09-04): flagging every open row here takes effect on RealTrader's very
// next tick for each instrument, independent of whether/when the trader process itself restarts
// to pick up the trading_state='stopped' new-open gate. Returns how many rows were flagged.
func (r *Repository) RequestRealManualCloseAll(ctx context.Context) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE real_orders SET manual_close_requested = true
		WHERE closed_at IS NULL AND status IN ('filled', 'partial')
	`)
	if err != nil {
		return 0, fmt.Errorf("request real manual close all: %w", err)
	}
	return int(tag.RowsAffected()), nil
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
			ro.exchange_order_id, ro.exchange_algo_order_id, ro.manual_close_requested, ro.manual_override,
			ro.exchange_close_order_id, ro.exchange_realized_pnl, ro.exchange_fee, ro.exchange_close_px,
			ro.last_error, ro.last_error_at,
			-- In-place SL/TP edit count, mirroring ListPositions — backs the panel's "Updated"
			-- column for real rows so it means the same thing in both modes.
			(SELECT COUNT(*) FROM real_order_adjustments a WHERE a.order_id = ro.id)
		FROM real_orders ro
		LEFT JOIN strategies s ON s.id = ro.strategy_id
		WHERE ($1 = '' OR ro.inst_id = $1)
			AND ($2::boolean IS NULL OR (ro.closed_at IS NULL AND (NOT $2 OR ro.status IN ('filled','partial','closing'))) = $2)
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
			&o.ManualCloseRequested, &o.ManualOverride,
			&o.ExchangeCloseOrderID, &o.ExchangeRealizedPnL, &o.ExchangeFee, &o.ExchangeClosePx,
			&o.LastError, &o.LastErrorAt,
			&o.AdjustmentCount); err != nil {
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
