package postgres

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// RecordManualOrderAdjustment appends one entry to a manual order's in-trade SL/TP adjustment
// history. Mirrors RecordBotOrderAdjustment, minus a source column — every adjustment on a manual
// order is manual by construction.
func (r *Repository) RecordManualOrderAdjustment(ctx context.Context, orderID int64, field string, oldValue, newValue *decimal.Decimal) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO manual_order_adjustments (order_id, field, old_value, new_value)
		VALUES ($1, $2, $3, $4)
	`, orderID, field, oldValue, newValue)
	if err != nil {
		return fmt.Errorf("record manual order adjustment for order %d field %s: %w", orderID, field, err)
	}
	return nil
}

// ListManualOrderAdjustments returns orderID's adjustment history, oldest first.
func (r *Repository) ListManualOrderAdjustments(ctx context.Context, orderID int64) ([]port.ManualOrderAdjustment, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, order_id, field, old_value, new_value, created_at
		FROM manual_order_adjustments
		WHERE order_id = $1
		ORDER BY created_at ASC
	`, orderID)
	if err != nil {
		return nil, fmt.Errorf("list manual order adjustments for order %d: %w", orderID, err)
	}
	defer rows.Close()

	var out []port.ManualOrderAdjustment
	for rows.Next() {
		var a port.ManualOrderAdjustment
		if err := rows.Scan(&a.ID, &a.OrderID, &a.Field, &a.OldValue, &a.NewValue, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan manual order adjustment: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
