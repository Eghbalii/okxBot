package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// RecordParamChange appends one entry to a strategy's durable parameter-change timeline
// (CLAUDE.md §16, §16.3 step 5).
func (r *Repository) RecordParamChange(ctx context.Context, c port.ParamChange) (int64, error) {
	source := c.Source
	if source == "" {
		source = "optimizer"
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO strategy_param_changes (strategy_id, inst_id, old_config, new_config, source)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, c.StrategyID, c.InstID, c.OldConfig, c.NewConfig, source).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record param change for strategy %d/%s: %w", c.StrategyID, c.InstID, err)
	}
	return id, nil
}

// ListParamChanges returns instID's parameter-change history at or after since (zero time = no
// lower bound), oldest first.
func (r *Repository) ListParamChanges(ctx context.Context, instID string, since time.Time) ([]port.ParamChange, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, strategy_id, inst_id, old_config, new_config, source, created_at
		FROM strategy_param_changes
		WHERE inst_id = $1 AND created_at >= $2
		ORDER BY created_at ASC
	`, instID, since)
	if err != nil {
		return nil, fmt.Errorf("list param changes for %s: %w", instID, err)
	}
	defer rows.Close()

	var out []port.ParamChange
	for rows.Next() {
		var c port.ParamChange
		if err := rows.Scan(&c.ID, &c.StrategyID, &c.InstID, &c.OldConfig, &c.NewConfig, &c.Source, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan param change: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
