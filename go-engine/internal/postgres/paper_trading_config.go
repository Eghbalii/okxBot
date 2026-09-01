package postgres

import (
	"context"
	"fmt"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

const paperTradingConfigCols = `trading_state, disable_long, disable_short, active_kinds, disabled_inst_ids, active_bars, updated_at`

// GetPaperTradingConfig returns the panel-editable control-box config (CLAUDE.md), seeding the
// singleton row at its column defaults if it hasn't been written yet (migration 000015 already
// inserts row id=1, but this is defensive against a database that predates that seed insert).
func (r *Repository) GetPaperTradingConfig(ctx context.Context) (port.PaperTradingConfig, error) {
	var c port.PaperTradingConfig
	err := r.pool.QueryRow(ctx, `
		INSERT INTO paper_trading_config (id) VALUES (1)
		ON CONFLICT (id) DO UPDATE SET id = paper_trading_config.id
		RETURNING `+paperTradingConfigCols).
		Scan(&c.TradingState, &c.DisableLong, &c.DisableShort, &c.ActiveKinds, &c.DisabledInstIDs, &c.ActiveBars, &c.UpdatedAt)
	if err != nil {
		return port.PaperTradingConfig{}, fmt.Errorf("get paper trading config: %w", err)
	}
	return c, nil
}

// SavePaperTradingConfig applies patch's non-nil fields onto the singleton row, leaving every
// unset field unchanged — mirrors tester.Store.SaveRuntimeConfig's coalesce-onto-existing shape.
// The slice fields are *[]string (a pointer to a slice) rather than []string so an explicit empty
// slice ("clear this restriction") is distinguishable from a nil pointer ("field omitted, leave
// the saved restriction as-is") — both would otherwise scan/bind identically as SQL NULL.
func (r *Repository) SavePaperTradingConfig(ctx context.Context, patch port.PaperTradingConfigPatch) (port.PaperTradingConfig, error) {
	var activeKinds, disabledInstIDs, activeBars any
	if patch.ActiveKinds != nil {
		activeKinds = *patch.ActiveKinds
	}
	if patch.DisabledInstIDs != nil {
		disabledInstIDs = *patch.DisabledInstIDs
	}
	if patch.ActiveBars != nil {
		activeBars = *patch.ActiveBars
	}

	var c port.PaperTradingConfig
	err := r.pool.QueryRow(ctx, `
		INSERT INTO paper_trading_config (id, trading_state, disable_long, disable_short, active_kinds, disabled_inst_ids, active_bars, updated_at)
		VALUES (1, coalesce($1, 'running'), coalesce($2, false), coalesce($3, false), $4, $5, $6, now())
		ON CONFLICT (id) DO UPDATE SET
			trading_state = coalesce($1, paper_trading_config.trading_state),
			disable_long = coalesce($2, paper_trading_config.disable_long),
			disable_short = coalesce($3, paper_trading_config.disable_short),
			active_kinds = CASE WHEN $7 THEN $4 ELSE paper_trading_config.active_kinds END,
			disabled_inst_ids = CASE WHEN $8 THEN $5 ELSE paper_trading_config.disabled_inst_ids END,
			active_bars = CASE WHEN $9 THEN $6 ELSE paper_trading_config.active_bars END,
			updated_at = now()
		RETURNING `+paperTradingConfigCols,
		patch.TradingState, patch.DisableLong, patch.DisableShort,
		activeKinds, disabledInstIDs, activeBars,
		patch.ActiveKinds != nil, patch.DisabledInstIDs != nil, patch.ActiveBars != nil).
		Scan(&c.TradingState, &c.DisableLong, &c.DisableShort, &c.ActiveKinds, &c.DisabledInstIDs, &c.ActiveBars, &c.UpdatedAt)
	if err != nil {
		return port.PaperTradingConfig{}, fmt.Errorf("save paper trading config: %w", err)
	}
	return c, nil
}

// RequestManualCloseAll flags every currently-open paper order for close, the bulk form of
// RequestManualClose — used when the operator sets trading_state="stopped" from the panel (CLAUDE.md):
// every open position closes at the live price on its very next tick, same manual-close path and
// reward-reporting treatment (conductor.CloseReasonManual -> CategoryClosedEarly) as a single
// order's Close button. Returns how many rows were flagged.
func (r *Repository) RequestManualCloseAll(ctx context.Context) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE paper_orders SET manual_close_requested = true
		WHERE closed_at IS NULL AND mode = 'paper'
	`)
	if err != nil {
		return 0, fmt.Errorf("request manual close all: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// SetAssignmentsEnabledForKinds bulk-enables/disables strategy_assignments so only assignments
// whose strategy's Kind is in activeKinds are enabled — the global per-kind "active strategies"
// toggle (CLAUDE.md), applied uniformly across every token rather than per-assignment. A no-op
// (returns nil without touching any row) when activeKinds is empty: an empty restriction means
// "no restriction configured," preserving today's per-assignment-managed behavior for anyone who
// has never touched this control.
func (r *Repository) SetAssignmentsEnabledForKinds(ctx context.Context, activeKinds []string) error {
	if len(activeKinds) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE strategy_assignments
		SET enabled = (s.kind = ANY($1)), updated_at = now()
		FROM strategies s
		WHERE strategy_assignments.strategy_id = s.id
	`, activeKinds)
	if err != nil {
		return fmt.Errorf("set assignments enabled for kinds %v: %w", activeKinds, err)
	}
	return nil
}
