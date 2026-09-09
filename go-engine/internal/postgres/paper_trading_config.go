package postgres

import (
	"context"
	"fmt"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

const paperTradingConfigCols = `trading_state, disable_long, disable_short, active_kinds, disabled_inst_ids, auto_disabled_inst_ids, active_bars, updated_at`

// GetPaperTradingConfig returns mode's panel-editable control-box config (CLAUDE.md real-trading
// readiness plan, 2026-09-04 — paper_trading_config is now one row per mode, PK on mode, replacing
// migration 000015's id=1 singleton), seeding it at column defaults if it hasn't been written yet.
// A fresh row defaults trading_state='running' via this INSERT — matches paper's own long-standing
// default; mode='real' already has a seeded row from migration 000020 with trading_state='stopped'
// so this INSERT's default is never actually exercised for real mode in practice, but stays
// correct as a fallback rather than assuming the seed row always exists.
func (r *Repository) GetPaperTradingConfig(ctx context.Context, mode string) (port.PaperTradingConfig, error) {
	var c port.PaperTradingConfig
	err := r.pool.QueryRow(ctx, `
		INSERT INTO paper_trading_config (mode) VALUES ($1)
		ON CONFLICT (mode) DO UPDATE SET mode = paper_trading_config.mode
		RETURNING `+paperTradingConfigCols, mode).
		Scan(&c.TradingState, &c.DisableLong, &c.DisableShort, &c.ActiveKinds, &c.DisabledInstIDs, &c.AutoDisabledInstIDs, &c.ActiveBars, &c.UpdatedAt)
	if err != nil {
		return port.PaperTradingConfig{}, fmt.Errorf("get paper trading config for mode %s: %w", mode, err)
	}
	return c, nil
}

// SavePaperTradingConfig applies patch's non-nil fields onto mode's row, leaving every unset field
// unchanged — mirrors tester.Store.SaveRuntimeConfig's coalesce-onto-existing shape. The slice
// fields are *[]string (a pointer to a slice) rather than []string so an explicit empty slice
// ("clear this restriction") is distinguishable from a nil pointer ("field omitted, leave the
// saved restriction as-is") — both would otherwise scan/bind identically as SQL NULL.
func (r *Repository) SavePaperTradingConfig(ctx context.Context, mode string, patch port.PaperTradingConfigPatch) (port.PaperTradingConfig, error) {
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
	var autoDisabled any
	if patch.AutoDisabledInstIDs != nil {
		autoDisabled = *patch.AutoDisabledInstIDs
	}

	var c port.PaperTradingConfig
	err := r.pool.QueryRow(ctx, `
		INSERT INTO paper_trading_config (mode, trading_state, disable_long, disable_short, active_kinds, disabled_inst_ids, active_bars, auto_disabled_inst_ids, updated_at)
		VALUES ($7, coalesce($1, 'running'), coalesce($2, false), coalesce($3, false), $4, $5, $6, coalesce($11, '{}'), now())
		ON CONFLICT (mode) DO UPDATE SET
			trading_state = coalesce($1, paper_trading_config.trading_state),
			disable_long = coalesce($2, paper_trading_config.disable_long),
			disable_short = coalesce($3, paper_trading_config.disable_short),
			active_kinds = CASE WHEN $8 THEN $4 ELSE paper_trading_config.active_kinds END,
			disabled_inst_ids = CASE WHEN $9 THEN $5 ELSE paper_trading_config.disabled_inst_ids END,
			active_bars = CASE WHEN $10 THEN $6 ELSE paper_trading_config.active_bars END,
			auto_disabled_inst_ids = CASE WHEN $12 THEN $11 ELSE paper_trading_config.auto_disabled_inst_ids END,
			updated_at = now()
		RETURNING `+paperTradingConfigCols,
		patch.TradingState, patch.DisableLong, patch.DisableShort,
		activeKinds, disabledInstIDs, activeBars, mode,
		patch.ActiveKinds != nil, patch.DisabledInstIDs != nil, patch.ActiveBars != nil,
		autoDisabled, patch.AutoDisabledInstIDs != nil).
		Scan(&c.TradingState, &c.DisableLong, &c.DisableShort, &c.ActiveKinds, &c.DisabledInstIDs, &c.AutoDisabledInstIDs, &c.ActiveBars, &c.UpdatedAt)
	if err != nil {
		return port.PaperTradingConfig{}, fmt.Errorf("save paper trading config for mode %s: %w", mode, err)
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

// SetAssignmentsEnabledForKinds bulk-enables/disables mode's strategy_assignments so only
// assignments whose strategy's Kind is in activeKinds are enabled — the global per-kind "active
// strategies" toggle (CLAUDE.md), applied uniformly across every token rather than per-assignment,
// scoped to one mode so this never touches the other mode's assignments. A no-op (returns nil
// without touching any row) when activeKinds is empty: an empty restriction means "no restriction
// configured," preserving today's per-assignment-managed behavior for anyone who has never touched
// this control.
func (r *Repository) SetAssignmentsEnabledForKinds(ctx context.Context, mode string, activeKinds []string, instIDs, bars []string) error {
	if len(activeKinds) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE strategy_assignments
		SET enabled = (s.kind = ANY($1)), updated_at = now()
		FROM strategies s
		WHERE strategy_assignments.strategy_id = s.id AND strategy_assignments.mode = $2
	`, activeKinds, mode)
	if err != nil {
		return fmt.Errorf("set assignments enabled for kinds %v (mode %s): %w", activeKinds, mode, err)
	}

	// Create the rows a newly-activated kind needs, rather than only flipping rows that already
	// exist (2026-09-09: the operator activated trend_confluence for real mode and saw no signals
	// all day). The UPDATE above can only enable an assignment that is already there, and real mode
	// had assignments for just two kinds — so selecting a third in the panel reported success,
	// wrote activeKinds, and then had nothing to enable. The kind was "active" in the config and
	// entirely absent from the roster the engine actually loads.
	//
	// Rows are created from the ORIGIN strategy of each kind (§11.3's locked template) across the
	// given instruments and bars, and only where one does not already exist — CreateAssignment's
	// ON CONFLICT would otherwise re-enable a per-token assignment an operator had deliberately
	// turned off from the Strategies page, which is a different setting from this global per-kind
	// switch and must not be overwritten by it.
	if len(instIDs) == 0 || len(bars) == 0 {
		return nil
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO strategy_assignments (strategy_id, inst_id, bar, enabled, mode)
		SELECT s.id, inst.inst_id, bar.bar, true, $4
		FROM strategies s
		CROSS JOIN unnest($2::text[]) AS inst(inst_id)
		CROSS JOIN unnest($3::text[]) AS bar(bar)
		WHERE s.kind = ANY($1) AND s.is_origin
		ON CONFLICT (strategy_id, inst_id, bar, mode) DO NOTHING
	`, activeKinds, instIDs, bars, mode)
	if err != nil {
		return fmt.Errorf("create missing assignments for kinds %v (mode %s): %w", activeKinds, mode, err)
	}
	return nil
}
