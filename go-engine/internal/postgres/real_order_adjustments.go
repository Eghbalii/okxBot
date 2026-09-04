package postgres

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// RecordRealOrderAdjustment appends one entry to a real order's in-trade SL/TP adjustment history.
// Mirrors RecordPaperOrderAdjustment, into real_order_adjustments.
func (r *Repository) RecordRealOrderAdjustment(ctx context.Context, orderID int64, field string, oldValue, newValue *decimal.Decimal, source string) error {
	if source == "" {
		source = "model"
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO real_order_adjustments (order_id, field, old_value, new_value, source)
		VALUES ($1, $2, $3, $4, $5)
	`, orderID, field, oldValue, newValue, source)
	if err != nil {
		return fmt.Errorf("record real order adjustment for order %d field %s: %w", orderID, field, err)
	}
	return nil
}

// ListRealOrderAdjustments returns orderID's adjustment history, oldest first. Mirrors
// ListPaperOrderAdjustments, reusing the same PaperOrderAdjustment shape (fields are identical).
func (r *Repository) ListRealOrderAdjustments(ctx context.Context, orderID int64) ([]port.PaperOrderAdjustment, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, order_id, field, old_value, new_value, source, created_at
		FROM real_order_adjustments
		WHERE order_id = $1
		ORDER BY created_at ASC
	`, orderID)
	if err != nil {
		return nil, fmt.Errorf("list real order adjustments for order %d: %w", orderID, err)
	}
	defer rows.Close()

	var out []port.PaperOrderAdjustment
	for rows.Next() {
		var a port.PaperOrderAdjustment
		if err := rows.Scan(&a.ID, &a.OrderID, &a.Field, &a.OldValue, &a.NewValue, &a.Source, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan real order adjustment: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
