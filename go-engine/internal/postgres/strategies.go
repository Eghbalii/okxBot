package postgres

import (
	"context"
	"fmt"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// CreateStrategy inserts a new strategy config and returns its id.
func (r *Repository) CreateStrategy(ctx context.Context, s port.StrategyConfig) (int64, error) {
	cfg := s.Config
	if cfg == nil {
		cfg = []byte("{}")
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO strategies (name, inst_ids, kind, config, enabled, cloned_from)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, s.Name, s.InstIDs, s.Kind, cfg, s.Enabled, s.ClonedFrom).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create strategy %q: %w", s.Name, err)
	}
	return id, nil
}

// ListStrategies returns strategies assigned to instID (or all, if instID is empty), optionally
// filtered to only enabled ones.
func (r *Repository) ListStrategies(ctx context.Context, instID string, enabledOnly bool) ([]port.StrategyConfig, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, inst_ids, kind, config, enabled, cloned_from
		FROM strategies
		WHERE ($1 = '' OR $1 = ANY(inst_ids)) AND (NOT $2 OR enabled)
		ORDER BY id
	`, instID, enabledOnly)
	if err != nil {
		return nil, fmt.Errorf("list strategies: %w", err)
	}
	defer rows.Close()

	var out []port.StrategyConfig
	for rows.Next() {
		var s port.StrategyConfig
		if err := rows.Scan(&s.ID, &s.Name, &s.InstIDs, &s.Kind, &s.Config, &s.Enabled, &s.ClonedFrom); err != nil {
			return nil, fmt.Errorf("scan strategy: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
