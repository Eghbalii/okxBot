package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// OpenBotOrder inserts a new real-money order and returns its id. o.Status must be set explicitly
// by the caller (normally "pending" — see port.BotOrder's doc comment).
func (r *Repository) OpenBotOrder(ctx context.Context, o port.BotOrder) (int64, error) {
	status := o.Status
	if status == "" {
		status = "pending"
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO bot_orders (inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, features_json, status, bar, exchange_order_id, contracts)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id
	`, o.InstID, o.StrategyID, o.Side, o.EntryPx, o.SLPx, o.TPPx, o.Size, o.Leverage, o.FeaturesJSON, status, o.Bar, o.ExchangeOrderID, o.Contracts).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("open bot order for %s: %w", o.InstID, err)
	}
	return id, nil
}

// GetBotOrder fetches a single bot order by id, open or closed. Mirrors GetPaperOrder.
func (r *Repository) GetBotOrder(ctx context.Context, id int64) (port.BotOrder, error) {
	var o port.BotOrder
	var bar *string
	err := r.pool.QueryRow(ctx, `
		SELECT id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, opened_at,
			closed_at, close_reason, close_px, realized_pnl, features_json, status, bar,
			pnl_max_pct, pnl_min_pct, manual_close_requested, exchange_order_id, exchange_algo_order_id,
			exchange_tp_algo_order_id,
			manual_override,
			-- Added 2026-09-09: this read was written before the exchange-truth columns existed and
			-- never picked them up, so a single-order fetch silently reported no close order id even
			-- when one was stored — which made the panel's raw-record view unable to fetch the close
			-- leg at all. ListBotPositions already selected them; only this path was behind.
			exchange_close_order_id, exchange_realized_pnl, exchange_fee, exchange_close_px,
			last_error, last_error_at, exchange_open_raw, exchange_close_raw, contracts
		FROM bot_orders WHERE id = $1
	`, id).Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx, &o.Size,
		&o.Leverage, &o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx, &o.RealizedPnL,
		&o.FeaturesJSON, &o.Status, &bar, &o.PnLMaxPct, &o.PnLMinPct,
		&o.ManualCloseRequested, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID, &o.ExchangeTPAlgoOrderID,
		&o.ManualOverride,
		&o.ExchangeCloseOrderID, &o.ExchangeRealizedPnL, &o.ExchangeFee, &o.ExchangeClosePx,
		&o.LastError, &o.LastErrorAt, &o.ExchangeOpenRaw, &o.ExchangeCloseRaw, &o.Contracts)
	if err != nil {
		return port.BotOrder{}, fmt.Errorf("get bot order %d: %w", id, err)
	}
	if bar != nil {
		o.Bar = *bar
	}
	return o, nil
}

// UpdateBotOrderStatus transitions a real order's fill status once PlaceOrder's outcome is known.
// entryPx/size are nil-able: a "canceled" transition passes neither (nothing to correct); a
// "filled"/"partial" transition passes both, correcting the provisional pre-fill price/size to the
// exchange-confirmed values.
func (r *Repository) UpdateBotOrderStatus(ctx context.Context, id int64, status string, entryPx *decimal.Decimal, size *decimal.Decimal, contracts *decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE bot_orders
		SET status = $2,
			entry_px = COALESCE($3, entry_px),
			size = COALESCE($4, size),
			-- The exchange-confirmed contract count. COALESCE so a status-only transition (marking
			-- an order "opening") cannot blank a count already recorded.
			contracts = COALESCE($5, contracts)
		WHERE id = $1
	`, id, status, entryPx, size, contracts)
	if err != nil {
		return fmt.Errorf("update bot order %d status: %w", id, err)
	}
	return nil
}

// SetBotOrderFeatures records the decision-time observation snapshot, mirroring how FeaturesJSON
// is set on a paper order.
func (r *Repository) SetBotOrderFeatures(ctx context.Context, id int64, featuresJSON json.RawMessage) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE bot_orders SET features_json = $2 WHERE id = $1
	`, id, featuresJSON)
	if err != nil {
		return fmt.Errorf("set bot order %d features: %w", id, err)
	}
	return nil
}

// SetBotOrderExchangeAlgoOrderID mirrors SetExchangeAlgoOrderID for bot_orders. Records the
// STOP-LOSS algo order's ID (2026-09-22: SL and TP are separate resting orders, see BotOrder's
// own doc comment).
func (r *Repository) SetBotOrderExchangeAlgoOrderID(ctx context.Context, id int64, algoOrderID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE bot_orders SET exchange_algo_order_id = $2 WHERE id = $1
	`, id, algoOrderID)
	if err != nil {
		return fmt.Errorf("set exchange algo order id for bot order %d: %w", id, err)
	}
	return nil
}

// SetBotOrderExchangeTPAlgoOrderID records the TAKE-PROFIT algo order's ID — the sibling of
// SetBotOrderExchangeAlgoOrderID (which holds the stop-loss side), placed as a separate order
// (2026-09-22).
func (r *Repository) SetBotOrderExchangeTPAlgoOrderID(ctx context.Context, id int64, algoOrderID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE bot_orders SET exchange_tp_algo_order_id = $2 WHERE id = $1
	`, id, algoOrderID)
	if err != nil {
		return fmt.Errorf("set exchange tp algo order id for bot order %d: %w", id, err)
	}
	return nil
}

// CloseBotOrder marks a bot order closed with its realized outcome. Mirrors ClosePaperOrder.
func (r *Repository) CloseBotOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE bot_orders
		SET closed_at = now(), close_px = $2, close_reason = $3, realized_pnl = $4
		WHERE id = $1 AND closed_at IS NULL
	`, id, closePx, reason, realizedPnL)
	if err != nil {
		return fmt.Errorf("close bot order %d: %w", id, err)
	}
	// Guarded for the same reason as CloseBotOrderConfirmed below — see the note there.
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("bot order %d: %w", id, port.ErrOrderAlreadyClosed)
	}
	return nil
}

// SetBotOrderClosing marks a close as IN FLIGHT: it records the flattening order's id and moves
// the row to status='closing', while deliberately leaving closed_at NULL. The position stays open
// until the exchange confirms the flatten actually filled (CloseBotOrderConfirmed below) — real
// order 3 was recorded closed with nothing having verified OKX agreed, which is exactly the gap
// this split exists to close. A close that fails or times out therefore leaves a row that is
// visibly stuck in 'closing' rather than a row that lies about being flat.
func (r *Repository) SetBotOrderClosing(ctx context.Context, id int64, closeOrderID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE bot_orders
		SET status = 'closing',
			exchange_close_order_id = $2,
			last_error = NULL,
			last_error_at = NULL
		WHERE id = $1
	`, id, nullableText(closeOrderID))
	if err != nil {
		return fmt.Errorf("mark bot order %d closing: %w", id, err)
	}
	return nil
}

// CloseBotOrderConfirmed records a close the exchange has confirmed, storing the EXCHANGE's own
// numbers alongside our own (2026-09-08 request). exchangePnL/exchangeFee/exchangeClosePx are
// nil-able: a nil means OKX did not report that figure, which must stay distinguishable from a
// genuine zero, and the panel falls back to the locally computed value only in that case.
//
// realizedPnL (the locally computed figure) is still written so the two remain comparable — a
// persistent gap between them is itself worth seeing, since it means the local model of fees or
// fill price is wrong.
func (r *Repository) CloseBotOrderConfirmed(ctx context.Context, id int64, closePx decimal.Decimal, reason string,
	realizedPnL decimal.Decimal, exchangePnL, exchangeFee, exchangeClosePx *decimal.Decimal) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE bot_orders
		SET closed_at = now(),
			close_px = $2,
			close_reason = $3,
			realized_pnl = $4,
			exchange_realized_pnl = $5,
			exchange_fee = $6,
			exchange_close_px = $7,
			-- Back to a settled fill state. SetBotOrderClosing moved the row to 'closing' while the
			-- flatten was in flight, and leaving it there after the exchange confirmed the fill made
			-- a completed close read as permanently stuck in the panel (observed on bot order 5:
			-- closed_at set, no error, flat on OKX, still displaying "closing").
			--
			-- Unconditional rather than only-when-'closing': the open-side fill state this would
			-- otherwise preserve is already gone by this point, since SetBotOrderClosing overwrote
			-- it on the way in. Faithfully recording that a position opened partially filled needs a
			-- column of its own, not a status this path can restore — and status on a CLOSED row is
			-- display-only (nothing branches on it), so claiming 'partial' here would be inventing
			-- information rather than keeping it.
			status = 'filled',
			last_error = NULL,
			last_error_at = NULL
		WHERE id = $1 AND closed_at IS NULL
	`, id, closePx, reason, realizedPnL, exchangePnL, exchangeFee, exchangeClosePx)
	if err != nil {
		return fmt.Errorf("confirm close of bot order %d: %w", id, err)
	}
	// "AND closed_at IS NULL" is what makes closing an order idempotent (2026-09-09). Without it
	// this UPDATE overwrote an already-closed row's outcome: bot order 38 was closed twice by two
	// reconciliation passes 3 seconds apart, and the second write replaced a -0.014 loss with a
	// +0.472 gain. Both numbers were wrong, but the point is that the second write should never
	// have been allowed to land at all.
	//
	// The guard lives in the database rather than in Go because the callers cannot see each other:
	// each instrument has its own engine, several paths reach this close (tick monitor, reconcile,
	// manual, timeout), and after a restart a second PROCESS can race the first. An in-process
	// mutex cannot cover any of that; a conditional UPDATE covers all of it atomically.
	//
	// Zero rows affected is reported as ErrOrderAlreadyClosed rather than silently succeeding: a
	// caller that thinks it closed a position when another path already did should not go on to
	// deliver a second reward to the model or publish a second close event.
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("bot order %d: %w", id, port.ErrOrderAlreadyClosed)
	}
	return nil
}

// SetBotOrderExchangeRaw stores OKX's own record for one leg of an order. leg is "open" or
// "close"; anything else is rejected rather than silently writing to the wrong column, since a
// typo would otherwise overwrite the other leg's record with this one's.
func (r *Repository) SetBotOrderExchangeRaw(ctx context.Context, id int64, leg string, raw json.RawMessage) error {
	var column string
	switch leg {
	case "open":
		column = "exchange_open_raw"
	case "close":
		column = "exchange_close_raw"
	default:
		return fmt.Errorf("unknown order leg %q (want open or close)", leg)
	}
	if len(raw) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `UPDATE bot_orders SET `+column+` = $2 WHERE id = $1`, id, []byte(raw))
	if err != nil {
		return fmt.Errorf("store %s exchange record for bot order %d: %w", leg, id, err)
	}
	return nil
}

// SetBotOrderError records the most recent exchange failure for an order so the panel can raise it
// to a human. Deliberately does NOT change status: a failed close leaves the row in 'closing' and a
// failed open leaves it in 'opening', which is what makes a stuck order visible rather than one
// that quietly reverts to looking normal.
func (r *Repository) SetBotOrderError(ctx context.Context, id int64, message string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE bot_orders SET last_error = $2, last_error_at = now() WHERE id = $1
	`, id, message)
	if err != nil {
		return fmt.Errorf("set error on bot order %d: %w", id, err)
	}
	return nil
}

// ClearBotOrderError clears a recorded error once the condition has resolved, so a stale failure
// does not keep alarming in the panel after a later attempt succeeded.
func (r *Repository) ClearBotOrderError(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE bot_orders SET last_error = NULL, last_error_at = NULL WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("clear error on bot order %d: %w", id, err)
	}
	return nil
}

// UpdateBotOrderSLTP applies an in-trade SL/TP adjustment to an open real order. Mirrors
// UpdatePaperOrderSLTP — the caller is responsible for any clamping before calling this.
//
// manualOverride is true only for the operator's own edit (handleAdjustPosition) and false for
// every model-driven adjustment (BotTrader.applyBotAdjustment) — set in the SAME statement as
// the SL/TP write so the two can never observe a gap between "levels changed" and "locked from the
// model" (2026-09-06 request). Once true it is never cleared here: only an operator action should
// be able to hand control back to the model, and there is no such action yet.
func (r *Repository) UpdateBotOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal, manualOverride bool) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE bot_orders
		SET sl_px = $2, tp_px = $3, manual_override = manual_override OR $4
		WHERE id = $1 AND closed_at IS NULL
	`, id, slPx, tpPx, manualOverride)
	if err != nil {
		return fmt.Errorf("update bot order %d SL/TP: %w", id, err)
	}
	return nil
}

// ListOpenBotOrders returns still-open real positions for an instrument — restricted to
// status IN ('filled','partial') so an in-flight pending order is never treated as an open
// position (it isn't one yet).
func (r *Repository) ListOpenBotOrders(ctx context.Context, instID string) ([]port.BotOrder, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, inst_id, strategy_id, side, entry_px, sl_px, tp_px, size, leverage, opened_at, features_json, status, pnl_max_pct, pnl_min_pct, bar, manual_close_requested, exchange_order_id, exchange_algo_order_id, exchange_tp_algo_order_id, contracts
		FROM bot_orders
		-- 'closing' is included deliberately: a flatten is in flight but unconfirmed, so the
		-- position is still REAL and still needs monitoring. Excluding it here would make the
		-- engine forget a position that may well still be open on the exchange — the exact failure
		-- this whole confirmation flow exists to prevent.
		WHERE inst_id = $1 AND closed_at IS NULL AND status IN ('filled', 'partial', 'closing')
		ORDER BY opened_at
	`, instID)
	if err != nil {
		return nil, fmt.Errorf("list open bot orders for %s: %w", instID, err)
	}
	defer rows.Close()

	var out []port.BotOrder
	for rows.Next() {
		var o port.BotOrder
		var bar *string
		if err := rows.Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx, &o.Size, &o.Leverage, &o.OpenedAt, &o.FeaturesJSON, &o.Status, &o.PnLMaxPct, &o.PnLMinPct, &bar, &o.ManualCloseRequested, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID, &o.ExchangeTPAlgoOrderID, &o.Contracts); err != nil {
			return nil, fmt.Errorf("scan bot order: %w", err)
		}
		if bar != nil {
			o.Bar = *bar
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RequestBotManualClose flags an open bot order for BotTrader to close on its next tick.
// Mirrors RequestManualClose. Errors if id is not currently open.
func (r *Repository) RequestBotManualClose(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE bot_orders SET manual_close_requested = true
		WHERE id = $1 AND closed_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("request manual close for bot order %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("bot order %d is not open", id)
	}
	return nil
}

// RequestBotManualCloseAll flags every currently-open bot order for close, the bulk form of
// RequestBotManualClose — mirrors RequestManualCloseAll. Used by the panel's Stop button
// (operator decision, 2026-09-04): flagging every open row here takes effect on BotTrader's very
// next tick for each instrument, independent of whether/when the trader process itself restarts
// to pick up the trading_state='stopped' new-open gate. Returns how many rows were flagged.
func (r *Repository) RequestBotManualCloseAll(ctx context.Context) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE bot_orders SET manual_close_requested = true
		WHERE closed_at IS NULL AND status IN ('filled', 'partial')
	`)
	if err != nil {
		return 0, fmt.Errorf("request bot manual close all: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// UpdateBotOrderPnLExtremes advances an open real order's peak/trough unrealized PnL. Mirrors
// UpdatePaperOrderPnLExtremes.
func (r *Repository) UpdateBotOrderPnLExtremes(ctx context.Context, id int64, maxPct, minPct decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE bot_orders
		SET pnl_max_pct = GREATEST(pnl_max_pct, $2),
			pnl_min_pct = LEAST(pnl_min_pct, $3)
		WHERE id = $1 AND closed_at IS NULL
	`, id, maxPct, minPct)
	if err != nil {
		return fmt.Errorf("update pnl extremes for bot order %d: %w", id, err)
	}
	return nil
}

// ListBotPositions returns real positions (open and/or closed) for the panel, filtered/sorted/
// paged per f — mirrors ListPositions. f.Mode is ignored (every row is real by construction).
// f.Open filters against ClosedAt, same as ListPositions; a "canceled" order (never filled) has
// ClosedAt NULL forever, so it reads as "open" under a naive filter — excluded here by requiring
// status IN ('filled','partial','closing','untracked') whenever f.Open is true, so a canceled
// attempt never occupies a slot in the "open positions" view. 'untracked' (2026-09-28) is a
// position reconcile found open on the exchange with no local record of how it was opened — it
// MUST still count as open here, or the whole point of writing that row (making it visible to the
// operator on the Positions page) is silently defeated one query away from where the row is
// created. An f.Open == false or nil query is unaffected and still returns canceled rows (visible
// under "closed"/"all", per the panel's Status badge design).
func (r *Repository) ListBotPositions(ctx context.Context, f port.PositionFilter) ([]port.BotOrder, error) {
	col, ok := positionSortColumns[f.SortBy]
	if !ok {
		return nil, fmt.Errorf("list bot positions: unknown sort field %q", f.SortBy)
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
			ro.exchange_order_id, ro.exchange_algo_order_id, ro.exchange_tp_algo_order_id,
			ro.manual_close_requested, ro.manual_override,
			ro.exchange_close_order_id, ro.exchange_realized_pnl, ro.exchange_fee, ro.exchange_close_px,
			ro.last_error, ro.last_error_at, ro.contracts,
			-- In-place SL/TP edit count, mirroring ListPositions — backs the panel's "Updated"
			-- column for real rows so it means the same thing in both modes.
			(SELECT COUNT(*) FROM bot_order_adjustments a WHERE a.order_id = ro.id)
		FROM bot_orders ro
		LEFT JOIN strategies s ON s.id = ro.strategy_id
		WHERE ($1 = '' OR ro.inst_id = $1)
			AND ($2::boolean IS NULL OR (ro.closed_at IS NULL AND (NOT $2 OR ro.status IN ('filled','partial','closing','untracked'))) = $2)
		ORDER BY ` + orderClause

	args := []any{f.InstID, f.Open}
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		args = append(args, f.Limit, f.Offset)
	}

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list bot positions: %w", err)
	}
	defer rows.Close()

	var out []port.BotOrder
	for rows.Next() {
		var o port.BotOrder
		var bar *string
		if err := rows.Scan(&o.ID, &o.InstID, &o.StrategyID, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx,
			&o.Size, &o.Leverage, &o.OpenedAt, &o.ClosedAt, &o.CloseReason, &o.ClosePx,
			&o.RealizedPnL, &o.FeaturesJSON, &o.Status, &bar,
			&o.PnLMaxPct, &o.PnLMinPct, &o.StrategyName, &o.ExchangeOrderID, &o.ExchangeAlgoOrderID,
			&o.ExchangeTPAlgoOrderID,
			&o.ManualCloseRequested, &o.ManualOverride,
			&o.ExchangeCloseOrderID, &o.ExchangeRealizedPnL, &o.ExchangeFee, &o.ExchangeClosePx,
			&o.LastError, &o.LastErrorAt, &o.Contracts,
			&o.AdjustmentCount); err != nil {
			return nil, fmt.Errorf("scan bot position: %w", err)
		}
		if bar != nil {
			o.Bar = *bar
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CountBotPositions mirrors CountPositions, same open-filter semantics as ListBotPositions.
func (r *Repository) CountBotPositions(ctx context.Context, f port.PositionFilter) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM bot_orders ro
		WHERE ($1 = '' OR ro.inst_id = $1)
			AND ($2::boolean IS NULL OR (ro.closed_at IS NULL AND (NOT $2 OR ro.status IN ('filled','partial'))) = $2)
	`, f.InstID, f.Open).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count bot positions: %w", err)
	}
	return count, nil
}
