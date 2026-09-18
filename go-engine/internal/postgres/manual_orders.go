package postgres

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// CreateManualOrderIntent inserts a new open-order request, status "pending". Mirrors the general
// shape of OpenRealOrder but for the handshake table (docs/MANUAL_TRADE_PLAN.md §2.3).
func (r *Repository) CreateManualOrderIntent(ctx context.Context, in port.ManualOrderIntent) (int64, error) {
	orderType := in.OrderType
	if orderType == "" {
		orderType = "market"
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO manual_order_intents (inst_id, side, order_type, limit_px, size_usd, leverage, sl_px, tp_px)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id
	`, in.InstID, in.Side, orderType, in.LimitPx, in.SizeUSD, in.Leverage, in.SLPx, in.TPPx).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create manual order intent for %s: %w", in.InstID, err)
	}
	return id, nil
}

// ClaimPendingManualOrderIntents atomically claims every "pending" intent and returns them.
// UPDATE ... RETURNING is what makes this atomic across concurrent callers — the same conditional-
// write-not-mutex pattern this codebase already relies on for RequestManualClose/
// CloseRealOrderConfirmed's own idempotency guards (a mutex cannot cover two separate processes or
// a restart racing an in-flight claim; a single conditional statement does).
func (r *Repository) ClaimPendingManualOrderIntents(ctx context.Context) ([]port.ManualOrderIntent, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE manual_order_intents
		SET status = 'claimed', claimed_at = now()
		WHERE id IN (
			SELECT id FROM manual_order_intents WHERE status = 'pending' ORDER BY requested_at FOR UPDATE SKIP LOCKED
		)
		RETURNING id, requested_at, inst_id, side, order_type, limit_px, size_usd, leverage, sl_px, tp_px,
			status, manual_order_id, error, claimed_at
	`)
	if err != nil {
		return nil, fmt.Errorf("claim pending manual order intents: %w", err)
	}
	defer rows.Close()

	var out []port.ManualOrderIntent
	for rows.Next() {
		var in port.ManualOrderIntent
		if err := rows.Scan(&in.ID, &in.RequestedAt, &in.InstID, &in.Side, &in.OrderType, &in.LimitPx,
			&in.SizeUSD, &in.Leverage, &in.SLPx, &in.TPPx, &in.Status, &in.ManualOrderID, &in.Error, &in.ClaimedAt); err != nil {
			return nil, fmt.Errorf("scan manual order intent: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// FinishManualOrderIntent marks a claimed intent "done" (manualOrderID set) or "failed" (errMsg
// set) — the terminal write once ManualTrader knows the outcome of an open attempt.
func (r *Repository) FinishManualOrderIntent(ctx context.Context, id int64, manualOrderID *int64, errMsg *string) error {
	status := "done"
	if errMsg != nil {
		status = "failed"
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE manual_order_intents SET status = $2, manual_order_id = $3, error = $4 WHERE id = $1
	`, id, status, manualOrderID, errMsg)
	if err != nil {
		return fmt.Errorf("finish manual order intent %d: %w", id, err)
	}
	return nil
}

// GetManualOrderIntent fetches a single intent by id, for the panel to poll while an order is
// still being placed (status "pending"/"claimed") before a manual_orders row exists yet.
func (r *Repository) GetManualOrderIntent(ctx context.Context, id int64) (port.ManualOrderIntent, error) {
	var in port.ManualOrderIntent
	err := r.pool.QueryRow(ctx, `
		SELECT id, requested_at, inst_id, side, order_type, limit_px, size_usd, leverage, sl_px, tp_px,
			status, manual_order_id, error, claimed_at
		FROM manual_order_intents WHERE id = $1
	`, id).Scan(&in.ID, &in.RequestedAt, &in.InstID, &in.Side, &in.OrderType, &in.LimitPx,
		&in.SizeUSD, &in.Leverage, &in.SLPx, &in.TPPx, &in.Status, &in.ManualOrderID, &in.Error, &in.ClaimedAt)
	if err != nil {
		return port.ManualOrderIntent{}, fmt.Errorf("get manual order intent %d: %w", id, err)
	}
	return in, nil
}

// OpenManualOrder inserts a new manual order and returns its id. Mirrors OpenRealOrder: the caller
// must set o.Status explicitly (normally "pending" for market, "resting" for an accepted-but-
// unfilled limit order — see port.ManualOrder's doc comment).
func (r *Repository) OpenManualOrder(ctx context.Context, o port.ManualOrder) (int64, error) {
	status := o.Status
	if status == "" {
		status = "pending"
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO manual_orders (inst_id, exec_inst_id, side, order_type, limit_px, status, entry_px, sl_px, tp_px, size, leverage, exchange_order_id, contracts)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id
	`, o.InstID, o.ExecInstID, o.Side, o.OrderType, o.LimitPx, status, o.EntryPx, o.SLPx, o.TPPx, o.Size, o.Leverage, o.ExchangeOrderID, o.Contracts).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("open manual order for %s: %w", o.InstID, err)
	}
	return id, nil
}

// GetManualOrder fetches a single manual order by id, open or closed.
func (r *Repository) GetManualOrder(ctx context.Context, id int64) (port.ManualOrder, error) {
	var o port.ManualOrder
	err := r.pool.QueryRow(ctx, `
		SELECT id, inst_id, exec_inst_id, side, order_type, limit_px, status, entry_px, sl_px, tp_px,
			size, leverage, contracts, protected_by_strategy, opened_at, closed_at, close_reason,
			close_px, realized_pnl, exchange_order_id, exchange_algo_order_id, exchange_close_order_id,
			exchange_fee, manual_close_requested, last_error, last_error_at, created_at
		FROM manual_orders WHERE id = $1
	`, id).Scan(&o.ID, &o.InstID, &o.ExecInstID, &o.Side, &o.OrderType, &o.LimitPx, &o.Status,
		&o.EntryPx, &o.SLPx, &o.TPPx, &o.Size, &o.Leverage, &o.Contracts, &o.ProtectedByStrategy,
		&o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx, &o.RealizedPnL,
		&o.ExchangeOrderID, &o.ExchangeAlgoOrderID, &o.ExchangeCloseOrderID, &o.ExchangeFee,
		&o.ManualCloseRequested, &o.LastError, &o.LastErrorAt, &o.CreatedAt)
	if err != nil {
		return port.ManualOrder{}, fmt.Errorf("get manual order %d: %w", id, err)
	}
	return o, nil
}

// UpdateManualOrderStatus transitions a manual order's fill status. Mirrors UpdateRealOrderStatus,
// widened for the "resting" state (a limit order accepted by the exchange, not yet filled) that
// real_orders has never needed. opened_at is stamped the first time the order reaches a state where
// a position genuinely exists ("filled"/"partial") — a still-resting limit order has no opened_at
// yet, since nothing has actually opened.
func (r *Repository) UpdateManualOrderStatus(ctx context.Context, id int64, status string, entryPx, size, contracts *decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE manual_orders
		SET status = $2,
			entry_px = COALESCE($3, entry_px),
			size = COALESCE($4, size),
			contracts = COALESCE($5, contracts),
			opened_at = CASE WHEN opened_at IS NULL AND $2 IN ('filled', 'partial') THEN now() ELSE opened_at END
		WHERE id = $1
	`, id, status, entryPx, size, contracts)
	if err != nil {
		return fmt.Errorf("update manual order %d status: %w", id, err)
	}
	return nil
}

// SetManualOrderProtection records the outcome of ManualTrader's post-fill protection step
// (docs/MANUAL_TRADE_PLAN.md §4/§8.4): either algoOrderID is set (a fresh protective order was
// placed) or protectedByStrategy is true (RealTrader already had one on this token, so none was
// placed) — never both; the caller is responsible for that invariant.
func (r *Repository) SetManualOrderProtection(ctx context.Context, id int64, algoOrderID *string, protectedByStrategy bool) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE manual_orders SET exchange_algo_order_id = $2, protected_by_strategy = $3 WHERE id = $1
	`, id, algoOrderID, protectedByStrategy)
	if err != nil {
		return fmt.Errorf("set protection for manual order %d: %w", id, err)
	}
	return nil
}

// CloseManualOrder records an exchange-confirmed close. Mirrors CloseRealOrderConfirmed's
// idempotency guard exactly (the same "AND closed_at IS NULL" reasoning: two racing close paths
// must not both succeed and overwrite each other's outcome).
func (r *Repository) CloseManualOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal, exchangeFee *decimal.Decimal) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE manual_orders
		SET closed_at = now(), close_px = $2, close_reason = $3, realized_pnl = $4, exchange_fee = $5,
			status = 'closed', last_error = NULL, last_error_at = NULL
		WHERE id = $1 AND closed_at IS NULL
	`, id, closePx, reason, realizedPnL, exchangeFee)
	if err != nil {
		return fmt.Errorf("close manual order %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("manual order %d: %w", id, port.ErrOrderAlreadyClosed)
	}
	return nil
}

// RequestManualOrderClose flags an order for ManualTrader to end on its next tick — either flatten
// (status filled/partial) or cancel (status resting, an unfilled limit order). One flag covers
// both cases safely because a row's status is never simultaneously "resting" and "filled/partial"
// (docs/MANUAL_TRADE_PLAN.md §8.1): ManualTrader.Run reads the CURRENT status when it processes the
// flag and picks the matching action, so there is no ambiguity about which one applies. Mirrors
// RequestRealManualClose's async-intent shape (flag now, act on the process's own next tick).
func (r *Repository) RequestManualOrderClose(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE manual_orders SET manual_close_requested = true
		WHERE id = $1 AND closed_at IS NULL AND status IN ('filled', 'partial', 'resting')
	`, id)
	if err != nil {
		return fmt.Errorf("request manual close for manual order %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("manual order %d is not open or resting", id)
	}
	return nil
}

// CancelManualOrder cancels a still-RESTING (unfilled) limit order — a different exchange call
// (CancelOrder, not a flatten) and a different terminal state (close_reason='canceled', no position
// ever existed) than RequestManualOrderClose, so this is its own method rather than overloading
// that one's semantics (docs/MANUAL_TRADE_PLAN.md §8.1).
//
// Zero rows affected is reported as ErrOrderAlreadyClosed regardless of whether the row was never
// resting at all or a second racing caller already resolved it — the same idempotency guard
// CloseManualOrder/CloseRealOrderConfirmed use, since both cases mean the same thing to a caller
// here: there is nothing left for this call to do.
func (r *Repository) CancelManualOrder(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE manual_orders
		SET closed_at = now(), close_reason = 'canceled', status = 'canceled'
		WHERE id = $1 AND closed_at IS NULL AND status = 'resting'
	`, id)
	if err != nil {
		return fmt.Errorf("cancel manual order %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("manual order %d: %w", id, port.ErrOrderAlreadyClosed)
	}
	return nil
}

// SetManualOrderError records the latest exchange failure for an order. Mirrors SetRealOrderError:
// deliberately does not change status, so a stuck order stays visibly stuck.
func (r *Repository) SetManualOrderError(ctx context.Context, id int64, message string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE manual_orders SET last_error = $2, last_error_at = now() WHERE id = $1
	`, id, message)
	if err != nil {
		return fmt.Errorf("set error on manual order %d: %w", id, err)
	}
	return nil
}

// ClearManualOrderError clears a recorded error once a later attempt succeeded.
func (r *Repository) ClearManualOrderError(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE manual_orders SET last_error = NULL, last_error_at = NULL WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("clear error on manual order %d: %w", id, err)
	}
	return nil
}

// ListOpenManualOrders returns still-open manual positions for an instrument, restricted to
// status IN ('filled','partial') — mirrors ListOpenRealOrders. A resting/pending order is not yet a
// position and must never be double-counted as one.
func (r *Repository) ListOpenManualOrders(ctx context.Context, instID string) ([]port.ManualOrder, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, inst_id, exec_inst_id, side, order_type, limit_px, status, entry_px, sl_px, tp_px,
			size, leverage, contracts, protected_by_strategy, opened_at, exchange_order_id,
			exchange_algo_order_id, manual_close_requested
		FROM manual_orders
		WHERE inst_id = $1 AND closed_at IS NULL AND status IN ('filled', 'partial')
		ORDER BY opened_at
	`, instID)
	if err != nil {
		return nil, fmt.Errorf("list open manual orders for %s: %w", instID, err)
	}
	defer rows.Close()

	var out []port.ManualOrder
	for rows.Next() {
		var o port.ManualOrder
		if err := rows.Scan(&o.ID, &o.InstID, &o.ExecInstID, &o.Side, &o.OrderType, &o.LimitPx, &o.Status,
			&o.EntryPx, &o.SLPx, &o.TPPx, &o.Size, &o.Leverage, &o.Contracts, &o.ProtectedByStrategy,
			&o.OpenedAt, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID, &o.ManualCloseRequested); err != nil {
			return nil, fmt.Errorf("scan manual order: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListManualOrders lists manual orders for the panel, filtered/sorted/paged per f — mirrors
// ListRealPositions. f.Mode is ignored (every row is real by construction).
func (r *Repository) ListManualOrders(ctx context.Context, f port.PositionFilter) ([]port.ManualOrder, error) {
	col, ok := positionSortColumns[f.SortBy]
	if !ok {
		return nil, fmt.Errorf("list manual orders: unknown sort field %q", f.SortBy)
	}
	dir := "ASC"
	if f.SortDesc {
		dir = "DESC"
	}
	orderClause := fmt.Sprintf("%s %s NULLS LAST", col, dir)

	query := `
		SELECT id, inst_id, exec_inst_id, side, order_type, limit_px, status, entry_px, sl_px, tp_px,
			size, leverage, contracts, protected_by_strategy, opened_at, closed_at, close_reason,
			close_px, realized_pnl, exchange_order_id, exchange_algo_order_id, exchange_close_order_id,
			exchange_fee, manual_close_requested, last_error, last_error_at, created_at
		FROM manual_orders
		WHERE ($1 = '' OR inst_id = $1)
			AND ($2::boolean IS NULL OR (closed_at IS NULL AND (NOT $2 OR status IN ('filled','partial'))) = $2)
		ORDER BY ` + orderClause

	args := []any{f.InstID, f.Open}
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		args = append(args, f.Limit, f.Offset)
	}

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list manual orders: %w", err)
	}
	defer rows.Close()

	var out []port.ManualOrder
	for rows.Next() {
		var o port.ManualOrder
		if err := rows.Scan(&o.ID, &o.InstID, &o.ExecInstID, &o.Side, &o.OrderType, &o.LimitPx, &o.Status,
			&o.EntryPx, &o.SLPx, &o.TPPx, &o.Size, &o.Leverage, &o.Contracts, &o.ProtectedByStrategy,
			&o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx, &o.RealizedPnL,
			&o.ExchangeOrderID, &o.ExchangeAlgoOrderID, &o.ExchangeCloseOrderID, &o.ExchangeFee,
			&o.ManualCloseRequested, &o.LastError, &o.LastErrorAt, &o.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan manual order: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
