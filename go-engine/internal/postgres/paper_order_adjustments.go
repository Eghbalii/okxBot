package postgres

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// RecordPaperOrderAdjustment appends one entry to an order's in-trade SL/TP adjustment history
// (CLAUDE.md §15.4/§15.12 revision, 2026-09-02) — the audit trail that replaced the shadow-fork
// mechanic's implicit fork-vs-baseline comparison.
func (r *Repository) RecordPaperOrderAdjustment(ctx context.Context, orderID int64, field string, oldValue, newValue *decimal.Decimal, source string) error {
	if source == "" {
		source = "model"
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO paper_order_adjustments (order_id, field, old_value, new_value, source)
		VALUES ($1, $2, $3, $4, $5)
	`, orderID, field, oldValue, newValue, source)
	if err != nil {
		return fmt.Errorf("record paper order adjustment for order %d field %s: %w", orderID, field, err)
	}
	return nil
}

// ListPaperOrderAdjustments returns orderID's adjustment history, oldest first — the shape the
// panel's order-detail modal consumes to render a chronological change table.
func (r *Repository) ListPaperOrderAdjustments(ctx context.Context, orderID int64) ([]port.PaperOrderAdjustment, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, order_id, field, old_value, new_value, source, created_at
		FROM paper_order_adjustments
		WHERE order_id = $1
		ORDER BY created_at ASC
	`, orderID)
	if err != nil {
		return nil, fmt.Errorf("list paper order adjustments for order %d: %w", orderID, err)
	}
	defer rows.Close()

	var out []port.PaperOrderAdjustment
	for rows.Next() {
		var a port.PaperOrderAdjustment
		if err := rows.Scan(&a.ID, &a.OrderID, &a.Field, &a.OldValue, &a.NewValue, &a.Source, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan paper order adjustment: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
