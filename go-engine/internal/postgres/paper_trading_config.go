package postgres

import (
	"context"
	"fmt"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

const paperTradingConfigCols = `trading_state, disable_long, disable_short, active_kinds, disabled_inst_ids, auto_disabled_inst_ids, active_bars, updated_at`

// GetPaperTradingConfig returns (mode, exchange)'s panel-editable control-box config (CLAUDE.md
// real-trading readiness plan, 2026-09-04 — paper_trading_config is now one row per (mode,
// exchange), replacing migration 000015's id=1 singleton), seeding it at column defaults if it
// hasn't been written yet. A fresh row defaults trading_state='running' via this INSERT — matches
// paper's own long-standing default; mode='real' already has a seeded row from migration 000020
// with trading_state='stopped' so this INSERT's default is never actually exercised for real mode
// in practice, but stays correct as a fallback rather than assuming the seed row always exists.
//
// exchange="" defaults to "okx" (2026-09-22, multi-exchange paper trading), matching every
// pre-existing call site from before exchange scoping existed — a second, independent
// paper-trading instance against a different exchange gets its own control-box row under the same
// mode="paper".
func (r *Repository) GetPaperTradingConfig(ctx context.Context, mode, exchange string) (port.PaperTradingConfig, error) {
	if exchange == "" {
		exchange = port.PaperTradingConfigExchange
	}
	var c port.PaperTradingConfig
	err := r.pool.QueryRow(ctx, `
		INSERT INTO paper_trading_config (mode, exchange) VALUES ($1, $2)
		ON CONFLICT (mode, exchange) DO UPDATE SET mode = paper_trading_config.mode
		RETURNING `+paperTradingConfigCols, mode, exchange).
		Scan(&c.TradingState, &c.DisableLong, &c.DisableShort, &c.ActiveKinds, &c.DisabledInstIDs, &c.AutoDisabledInstIDs, &c.ActiveBars, &c.UpdatedAt)
	if err != nil {
		return port.PaperTradingConfig{}, fmt.Errorf("get paper trading config for mode %s, exchange %s: %w", mode, exchange, err)
	}
	return c, nil
}

// SavePaperTradingConfig applies patch's non-nil fields onto mode's row, leaving every unset field
// unchanged — mirrors tester.Store.SaveRuntimeConfig's coalesce-onto-existing shape. The slice
// fields are *[]string (a pointer to a slice) rather than []string so an explicit empty slice
// ("clear this restriction") is distinguishable from a nil pointer ("field omitted, leave the
// saved restriction as-is") — both would otherwise scan/bind identically as SQL NULL.
// exchange="" defaults to "okx" (2026-09-22, multi-exchange paper trading), matching every
// pre-existing call site from before exchange scoping existed.
func (r *Repository) SavePaperTradingConfig(ctx context.Context, mode, exchange string, patch port.PaperTradingConfigPatch) (port.PaperTradingConfig, error) {
	if exchange == "" {
		exchange = port.PaperTradingConfigExchange
	}
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
	// Explicitly cast to text[] in the SQL below, unlike the three columns above. Those are bound
	// bare ($4/$5/$6) and Postgres infers their type from the target column; this one is wrapped in
	// coalesce, and a NULL parameter inside coalesce has no column to infer from — Postgres then
	// types the '{}' literal as text and rejects the whole expression against a text[] column
	// ("column is of type text[] but expression is of type text", 2026-09-10). Every save with no
	// auto-disabled patch failed, which is every save the panel makes.
	var autoDisabled any
	if patch.AutoDisabledInstIDs != nil {
		autoDisabled = *patch.AutoDisabledInstIDs
	}

	var c port.PaperTradingConfig
	err := r.pool.QueryRow(ctx, `
		INSERT INTO paper_trading_config (mode, exchange, trading_state, disable_long, disable_short, active_kinds, disabled_inst_ids, active_bars, auto_disabled_inst_ids, updated_at)
		VALUES ($7, $13, coalesce($1, 'running'), coalesce($2, false), coalesce($3, false), $4, $5, $6, coalesce($11::text[], '{}'::text[]), now())
		ON CONFLICT (mode, exchange) DO UPDATE SET
			trading_state = coalesce($1, paper_trading_config.trading_state),
			disable_long = coalesce($2, paper_trading_config.disable_long),
			disable_short = coalesce($3, paper_trading_config.disable_short),
			active_kinds = CASE WHEN $8 THEN $4 ELSE paper_trading_config.active_kinds END,
			disabled_inst_ids = CASE WHEN $9 THEN $5 ELSE paper_trading_config.disabled_inst_ids END,
			active_bars = CASE WHEN $10 THEN $6 ELSE paper_trading_config.active_bars END,
			auto_disabled_inst_ids = CASE WHEN $12 THEN $11::text[] ELSE paper_trading_config.auto_disabled_inst_ids END,
			updated_at = now()
		RETURNING `+paperTradingConfigCols,
		patch.TradingState, patch.DisableLong, patch.DisableShort,
		activeKinds, disabledInstIDs, activeBars, mode,
		patch.ActiveKinds != nil, patch.DisabledInstIDs != nil, patch.ActiveBars != nil,
		autoDisabled, patch.AutoDisabledInstIDs != nil, exchange).
		Scan(&c.TradingState, &c.DisableLong, &c.DisableShort, &c.ActiveKinds, &c.DisabledInstIDs, &c.AutoDisabledInstIDs, &c.ActiveBars, &c.UpdatedAt)
	if err != nil {
		return port.PaperTradingConfig{}, fmt.Errorf("save paper trading config for mode %s, exchange %s: %w", mode, exchange, err)
	}
	return c, nil
}

// RequestManualCloseAll flags every currently-open paper order for close, the bulk form of
// RequestManualClose — used when the operator sets trading_state="stopped" from the panel (CLAUDE.md):
// every open position closes at the live price on its very next tick, same manual-close path and
// reward-reporting treatment (conductor.CloseReasonManual -> CategoryClosedEarly) as a single
// order's Close button. Returns how many rows were flagged. exchange scopes the sweep to one
// exchange's rows ("" defaults to "okx", matching every pre-existing call site) — added
// 2026-09-22 so stopping one exchange's paper trading never touches the other's open orders.
func (r *Repository) RequestManualCloseAll(ctx context.Context, exchange string) (int, error) {
	if exchange == "" {
		exchange = "okx"
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE paper_orders SET manual_close_requested = true
		WHERE closed_at IS NULL AND mode = 'paper' AND exchange = $1
	`, exchange)
	if err != nil {
		return 0, fmt.Errorf("request manual close all (exchange %s): %w", exchange, err)
	}
	return int(tag.RowsAffected()), nil
}

// SetAssignmentsEnabledForKinds bulk-enables/disables (mode, exchange)'s strategy_assignments so
// only assignments whose strategy's Kind is in activeKinds are enabled — the global per-kind
// "active strategies" toggle (CLAUDE.md), applied uniformly across every token rather than
// per-assignment, scoped to one mode and exchange so this never touches another mode's or
// exchange's assignments. A no-op (returns nil without touching any row) when activeKinds is
// empty: an empty restriction means "no restriction configured," preserving today's
// per-assignment-managed behavior for anyone who has never touched this control. exchange=""
// defaults to "okx" (2026-09-22, multi-exchange paper trading), matching every pre-existing call
// site from before exchange scoping existed.
func (r *Repository) SetAssignmentsEnabledForKinds(ctx context.Context, mode, exchange string, activeKinds []string, instIDs, bars []string) error {
	if len(activeKinds) == 0 {
		return nil
	}
	if exchange == "" {
		exchange = "okx"
	}

	// In paper mode, origin-strategy rows are excluded entirely (2026-09-28) — the backtest/
	// optimize pipeline now owns which strategy trades each (token, bar) in paper mode, disabling
	// every origin assignment once a real tuned candidate exists to replace it (optimizer.Promote/
	// DisableOriginAssignments). Before this exclusion, this function's own UPDATE re-enabled every
	// origin row whose kind appeared in activeKinds on EVERY cmd/paper-trader restart (activeKinds
	// is a coarse per-kind allowlist that has nothing to do with origin-vs-promoted, so it matched
	// origins indiscriminately), and its INSERT could even create a brand-new origin assignment
	// for a (kind, inst, bar) that already had a real promoted clone — because the ON CONFLICT
	// target is keyed by strategy_id, and a clone always has a DIFFERENT strategy_id than its
	// origin, so the conflict never fired. Together these silently reverted the pipeline's cleanup
	// on every single restart, which is why paper positions kept opening under old strategy names
	// no matter how many times the origin assignments were disabled by hand or by the pipeline.
	// mode="bot" (real trading) has no such pipeline yet and still needs this function's original
	// bootstrap behavior, so only paper mode gets the exclusion.
	originClause := ""
	if mode == "paper" {
		originClause = "AND NOT s.is_origin"
	}

	_, err := r.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE strategy_assignments
		SET enabled = (s.kind = ANY($1)), updated_at = now()
		FROM strategies s
		WHERE strategy_assignments.strategy_id = s.id AND strategy_assignments.mode = $2 AND strategy_assignments.exchange = $3 %s
	`, originClause), activeKinds, mode, exchange)
	if err != nil {
		return fmt.Errorf("set assignments enabled for kinds %v (mode %s, exchange %s): %w", activeKinds, mode, exchange, err)
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
	//
	// Skipped entirely in paper mode (2026-09-28, same reasoning as above): creating a fresh origin
	// assignment here is exactly what let a fully-cleaned-up lineage silently regrow one.
	if mode == "paper" || len(instIDs) == 0 || len(bars) == 0 {
		return nil
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO strategy_assignments (strategy_id, inst_id, bar, enabled, mode, exchange)
		SELECT s.id, inst.inst_id, bar.bar, true, $4, $5
		FROM strategies s
		CROSS JOIN unnest($2::text[]) AS inst(inst_id)
		CROSS JOIN unnest($3::text[]) AS bar(bar)
		WHERE s.kind = ANY($1) AND s.is_origin
		ON CONFLICT (strategy_id, inst_id, bar, mode, exchange) DO NOTHING
	`, activeKinds, instIDs, bars, mode, exchange)
	if err != nil {
		return fmt.Errorf("create missing assignments for kinds %v (mode %s, exchange %s): %w", activeKinds, mode, exchange, err)
	}
	return nil
}
