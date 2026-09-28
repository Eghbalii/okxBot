package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// CreateStrategy inserts a new strategy config and returns its id.
func (r *Repository) CreateStrategy(ctx context.Context, s port.StrategyConfig) (int64, error) {
	cfg := s.Config
	if cfg == nil {
		cfg = []byte("{}")
	}
	instIDs := s.InstIDs
	if instIDs == nil {
		// pgx sends a nil []string as SQL NULL, not "use the column default" — inst_ids is
		// NOT NULL, so a nil slice here would violate that constraint instead of falling back.
		instIDs = []string{}
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO strategies (name, inst_ids, kind, config, enabled, is_origin, cloned_from)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, s.Name, instIDs, s.Kind, cfg, s.Enabled, s.IsOrigin, s.ClonedFrom).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create strategy %q: %w", s.Name, err)
	}
	return id, nil
}

// GetStrategy fetches a single strategy by id.
func (r *Repository) GetStrategy(ctx context.Context, id int64) (port.StrategyConfig, error) {
	var s port.StrategyConfig
	err := r.pool.QueryRow(ctx, `
		SELECT id, name, inst_ids, kind, config, enabled, is_origin, cloned_from
		FROM strategies WHERE id = $1
	`, id).Scan(&s.ID, &s.Name, &s.InstIDs, &s.Kind, &s.Config, &s.Enabled, &s.IsOrigin, &s.ClonedFrom)
	if err != nil {
		return port.StrategyConfig{}, fmt.Errorf("get strategy %d: %w", id, err)
	}
	return s, nil
}

// ListStrategies returns strategies assigned to instID (or all, if instID is empty), optionally
// filtered to only enabled ones.
func (r *Repository) ListStrategies(ctx context.Context, instID string, enabledOnly bool) ([]port.StrategyConfig, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, inst_ids, kind, config, enabled, is_origin, cloned_from
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
		if err := rows.Scan(&s.ID, &s.Name, &s.InstIDs, &s.Kind, &s.Config, &s.Enabled, &s.IsOrigin, &s.ClonedFrom); err != nil {
			return nil, fmt.Errorf("scan strategy: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateStrategyConfig edits a sub-strategy's own config/enabled flag. Callers must not call this
// on an origin row (IsOrigin true) — origins are never edited in place (CLAUDE.md §11.3); the API
// layer is responsible for rejecting such requests before reaching this adapter.
func (r *Repository) UpdateStrategyConfig(ctx context.Context, id int64, config json.RawMessage, enabled bool) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE strategies SET config = $2, enabled = $3, updated_at = now()
		WHERE id = $1 AND NOT is_origin
	`, id, config, enabled)
	if err != nil {
		return fmt.Errorf("update strategy %d: %w", id, err)
	}
	return nil
}

// DeleteStrategy removes a sub-strategy. Origin rows can't be deleted (CLAUDE.md §11.3: "keep the
// origin always").
func (r *Repository) DeleteStrategy(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM strategies WHERE id = $1 AND NOT is_origin`, id)
	if err != nil {
		return fmt.Errorf("delete strategy %d: %w", id, err)
	}
	return nil
}

// ResetStrategyToOrigin overwrites a sub-strategy's config with its origin's current config.
func (r *Repository) ResetStrategyToOrigin(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE strategies AS child
		SET config = origin.config, updated_at = now()
		FROM strategies AS origin
		WHERE child.id = $1 AND origin.id = child.cloned_from AND NOT child.is_origin
	`, id)
	if err != nil {
		return fmt.Errorf("reset strategy %d to origin: %w", id, err)
	}
	return nil
}

// CreateAssignment binds a strategy to an instrument+timeframe+mode+exchange. a.Mode defaults to
// "paper" and a.Exchange defaults to "okx" when unset, matching every pre-existing caller's
// implicit assumption before mode/exchange scoping (CLAUDE.md real-trading readiness plan,
// 2026-09-04; exchange scoping added 2026-09-22 for multi-exchange paper trading).
func (r *Repository) CreateAssignment(ctx context.Context, a port.StrategyAssignment) (int64, error) {
	mode := a.Mode
	if mode == "" {
		mode = "paper"
	}
	exchange := a.Exchange
	if exchange == "" {
		exchange = "okx"
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO strategy_assignments (strategy_id, inst_id, bar, enabled, mode, exchange)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (strategy_id, inst_id, bar, mode, exchange) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()
		RETURNING id
	`, a.StrategyID, a.InstID, a.Bar, a.Enabled, mode, exchange).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create assignment (strategy %d, %s/%s, mode %s, exchange %s): %w", a.StrategyID, a.InstID, a.Bar, mode, exchange, err)
	}
	return id, nil
}

// ListAssignments returns mode/exchange's strategy assignments, optionally filtered to one
// instrument and/or only enabled ones. Callers (e.g. cmd/paper-trader/cmd/trader at startup) use
// this to reload durable token/timeframe→strategy bindings after a crash/restart instead of
// hardcoding them in Go. exchange="" defaults to "okx", matching every pre-existing call site
// from before exchange scoping existed (2026-09-22).
func (r *Repository) ListAssignments(ctx context.Context, instID string, enabledOnly bool, mode, exchange string) ([]port.StrategyAssignment, error) {
	if exchange == "" {
		exchange = "okx"
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, strategy_id, inst_id, bar, enabled, mode, exchange
		FROM strategy_assignments
		WHERE ($1 = '' OR inst_id = $1) AND (NOT $2 OR enabled) AND mode = $3 AND exchange = $4
		ORDER BY id
	`, instID, enabledOnly, mode, exchange)
	if err != nil {
		return nil, fmt.Errorf("list assignments: %w", err)
	}
	defer rows.Close()

	var out []port.StrategyAssignment
	for rows.Next() {
		var a port.StrategyAssignment
		if err := rows.Scan(&a.ID, &a.StrategyID, &a.InstID, &a.Bar, &a.Enabled, &a.Mode, &a.Exchange); err != nil {
			return nil, fmt.Errorf("scan assignment: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAssignmentEnabled toggles an assignment on/off without deleting it.
func (r *Repository) SetAssignmentEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := r.pool.Exec(ctx, `UPDATE strategy_assignments SET enabled = $2, updated_at = now() WHERE id = $1`, id, enabled)
	if err != nil {
		return fmt.Errorf("set assignment %d enabled=%v: %w", id, enabled, err)
	}
	return nil
}

// DeleteAssignment removes a strategy's binding to an instrument+timeframe.
func (r *Repository) DeleteAssignment(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM strategy_assignments WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete assignment %d: %w", id, err)
	}
	return nil
}

// DisableOriginAssignments disables (never deletes) every currently-enabled assignment for mode
// whose strategy is an origin row — see the port.Repository doc comment for why this exists.
func (r *Repository) DisableOriginAssignments(ctx context.Context, mode string) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE strategy_assignments sa
		SET enabled = false, updated_at = now()
		FROM strategies s
		WHERE sa.strategy_id = s.id AND s.is_origin AND sa.mode = $1 AND sa.enabled = true
	`, mode)
	if err != nil {
		return 0, fmt.Errorf("disable origin assignments for mode %q: %w", mode, err)
	}
	return int(tag.RowsAffected()), nil
}

// StrategyStatsFor computes a strategy's paper-trading track record from paper_orders
// (CLAUDE.md §11.3) — no separately maintained counters.
//
// A win is a closed trade with positive realized PnL, NOT close_reason='tp'. Once the RL
// ratchet trails a stop into profit (CLAUDE.md §15.4) an 'sl' close frequently realizes a
// gain, so keying off close_reason reported strategies with real profits as 0% win rate.
//
// Scoped to variant='baseline' only. A high adjustment rate can fork one baseline signal into
// several 'rl_adjusted' rows (CLAUDE.md §16.9's fork-count incident), and those forks are the
// same underlying signal monitored a second time, not independent evidence of the strategy's
// quality — counting them here would let fork volume dilute/skew a strategy's real track record.
// StrategyStatsFor computes strategyID's track record for mode, sourced from paper_orders
// (filtered to variant='baseline', excluding shadow forks) or bot_orders (no variant column at
// all — real trading has no forking, §27.3, so every row already counts) depending on mode.
func (r *Repository) StrategyStatsFor(ctx context.Context, strategyID int64, mode, exchange string) (port.StrategyStats, error) {
	if exchange == "" {
		exchange = "okx"
	}
	stats := port.StrategyStats{StrategyID: strategyID}
	var query string
	var args []any
	if mode == "bot" {
		// bot_orders has no exchange column at all (real trading has no second-exchange instance,
		// 2026-09-22) — exchange is accepted but not filtered on here, matching every other
		// bot-mode call site's "exchange is meaningless outside paper" treatment.
		query = `
			SELECT
				count(*),
				count(*) FILTER (WHERE closed_at IS NOT NULL AND realized_pnl > 0),
				count(*) FILTER (WHERE closed_at IS NOT NULL AND realized_pnl <= 0),
				count(*) FILTER (WHERE closed_at IS NULL),
				coalesce(sum(realized_pnl), 0),
				min(opened_at),
				max(coalesce(closed_at, opened_at))
			FROM bot_orders
			WHERE strategy_id = $1
		`
		args = []any{strategyID}
	} else {
		query = `
			SELECT
				count(*),
				count(*) FILTER (WHERE closed_at IS NOT NULL AND realized_pnl > 0),
				count(*) FILTER (WHERE closed_at IS NOT NULL AND realized_pnl <= 0),
				count(*) FILTER (WHERE closed_at IS NULL),
				coalesce(sum(realized_pnl), 0),
				min(opened_at),
				max(coalesce(closed_at, opened_at))
			FROM paper_orders
			WHERE strategy_id = $1 AND variant = 'baseline' AND exchange = $2
		`
		args = []any{strategyID, exchange}
	}
	err := r.pool.QueryRow(ctx, query, args...).Scan(&stats.SignalCount, &stats.Wins, &stats.Losses, &stats.OpenCount,
		&stats.RealizedPnL, &stats.FirstOpened, &stats.LastActivity)
	if err != nil {
		return port.StrategyStats{}, fmt.Errorf("strategy stats for %d (mode %s, exchange %s): %w", strategyID, mode, exchange, err)
	}
	return stats, nil
}
