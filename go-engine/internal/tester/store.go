// Package tester implements cmd/strategy-tester (2026-08-30 request): a standalone paper-trading
// copy that opens real virtual positions against live prices to validate whether a strategy's
// signal is worth anything, entirely independent of the RL agent and cmd/paper-trader's
// production path. No RL, no observation, no in-trade SL/TP update mechanic, no shared-account
// sizing — every position uses a fixed notional/leverage from config. Storage is two tables
// (migration 000010, tester_orders/tester_strategy_versions) that share nothing with
// paper_orders/strategies, so this service cannot collide with or be mistaken for production data.
package tester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

// Version is one numbered parameter set for a strategy kind (e.g. kind="grid_like", version=2).
// Version 1 is the unmodified origin defaults; every later version comes from an operator editing
// params in the panel, which creates a NEW row rather than overwriting — ParentVersionID points at
// what it was cloned from, so the panel can diff "what changed" (2026-08-30 request).
type Version struct {
	ID              int64
	Kind            string
	Version         int
	Config          json.RawMessage
	ParentVersionID *int64
	Enabled         bool
	CreatedAt       time.Time
}

// Order is one virtual position (mirrors paper_orders' shape but in its own table/namespace).
type Order struct {
	ID          int64
	InstID      string
	VersionID   int64
	Bar         string
	Side        string
	EntryPx     decimal.Decimal
	SLPx        *decimal.Decimal
	TPPx        *decimal.Decimal
	Size        decimal.Decimal
	Leverage    decimal.Decimal
	OpenedAt    time.Time
	ClosedAt    *time.Time
	CloseReason *string // "sl" or "tp" — no "manual"/"rl_early", this service never closes any other way
	ClosePx     *decimal.Decimal
	RealizedPnL *decimal.Decimal
}

// VersionStats is one version's aggregated track record, computed from tester_orders — no
// separately maintained counters, same pattern as port.Repository.StrategyStatsFor.
type VersionStats struct {
	VersionID   int64
	SignalCount int64 // total orders opened (== closed + open, since every order here comes from a real signal)
	Wins        int64 // closed with realized_pnl > 0
	Losses      int64 // closed with realized_pnl <= 0
	TPCloses    int64
	SLCloses    int64
	OpenCount   int64
	RealizedPnL decimal.Decimal
}

// Store is a thin Postgres access layer over tester_orders/tester_strategy_versions — a plain
// struct on the shared pool rather than an interface, matching internal/optimizer.TrialStore's
// precedent: this is an internal, single-database tool with no swappable-adapter requirement.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps an existing pgxpool.Pool (shared with the postgres.Repository the caller already
// constructed for Migrate(), rather than opening a second pool to the same database).
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// EnsureOriginVersion returns the id of kind's version 1 row, creating it (empty config, no
// parent) if it doesn't exist yet — the seed every kind needs before it can be traded or cloned.
func (s *Store) EnsureOriginVersion(ctx context.Context, kind string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO tester_strategy_versions (kind, version, config, parent_version_id, enabled)
		VALUES ($1, 1, '{}'::jsonb, NULL, true)
		ON CONFLICT (kind, version) DO UPDATE SET kind = EXCLUDED.kind
		RETURNING id
	`, kind).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure origin version for kind %q: %w", kind, err)
	}
	return id, nil
}

// CreateVersion inserts the next version number for kind, cloned from parentVersionID, and
// disables the parent so exactly one version per kind trades live at a time (the panel still
// shows every version's historical stats — see ListVersions).
func (s *Store) CreateVersion(ctx context.Context, kind string, config json.RawMessage, parentVersionID int64) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var nextVersion int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) + 1 FROM tester_strategy_versions WHERE kind = $1`, kind).Scan(&nextVersion); err != nil {
		return 0, fmt.Errorf("compute next version for kind %q: %w", kind, err)
	}

	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO tester_strategy_versions (kind, version, config, parent_version_id, enabled)
		VALUES ($1, $2, $3, $4, true)
		RETURNING id
	`, kind, nextVersion, config, parentVersionID).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert version: %w", err)
	}

	if _, err := tx.Exec(ctx, `UPDATE tester_strategy_versions SET enabled = false WHERE kind = $1 AND id <> $2`, kind, id); err != nil {
		return 0, fmt.Errorf("disable prior versions for kind %q: %w", kind, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

// ListVersions returns every version across every kind, newest-first within each kind.
func (s *Store) ListVersions(ctx context.Context) ([]Version, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, version, config, parent_version_id, enabled, created_at
		FROM tester_strategy_versions
		ORDER BY kind, version DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.Kind, &v.Version, &v.Config, &v.ParentVersionID, &v.Enabled, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// EnabledVersions returns the single currently-enabled version per kind — what the trading loop
// actually runs.
func (s *Store) EnabledVersions(ctx context.Context) ([]Version, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, version, config, parent_version_id, enabled, created_at
		FROM tester_strategy_versions WHERE enabled = true
		ORDER BY kind
	`)
	if err != nil {
		return nil, fmt.Errorf("list enabled versions: %w", err)
	}
	defer rows.Close()

	var out []Version
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.ID, &v.Kind, &v.Version, &v.Config, &v.ParentVersionID, &v.Enabled, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersion fetches one version by id — used by the panel's "compare to parent" view.
func (s *Store) GetVersion(ctx context.Context, id int64) (Version, error) {
	var v Version
	err := s.pool.QueryRow(ctx, `
		SELECT id, kind, version, config, parent_version_id, enabled, created_at
		FROM tester_strategy_versions WHERE id = $1
	`, id).Scan(&v.ID, &v.Kind, &v.Version, &v.Config, &v.ParentVersionID, &v.Enabled, &v.CreatedAt)
	if err != nil {
		return Version{}, fmt.Errorf("get version %d: %w", id, err)
	}
	return v, nil
}

// OpenOrder inserts a new virtual position.
func (s *Store) OpenOrder(ctx context.Context, o Order) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO tester_orders (inst_id, version_id, bar, side, entry_px, sl_px, tp_px, size, leverage, opened_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`, o.InstID, o.VersionID, o.Bar, o.Side, o.EntryPx, o.SLPx, o.TPPx, o.Size, o.Leverage, o.OpenedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("open tester order: %w", err)
	}
	return id, nil
}

// ListOpenOrders returns every open position for instID (all versions/bars) — used to enforce
// "at most one open position per instrument" the same way production does.
func (s *Store) ListOpenOrders(ctx context.Context, instID string) ([]Order, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, inst_id, version_id, bar, side, entry_px, sl_px, tp_px, size, leverage, opened_at
		FROM tester_orders WHERE inst_id = $1 AND closed_at IS NULL
	`, instID)
	if err != nil {
		return nil, fmt.Errorf("list open tester orders for %s: %w", instID, err)
	}
	defer rows.Close()

	var out []Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.InstID, &o.VersionID, &o.Bar, &o.Side, &o.EntryPx, &o.SLPx, &o.TPPx, &o.Size, &o.Leverage, &o.OpenedAt); err != nil {
			return nil, fmt.Errorf("scan tester order: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CloseOrder marks an order closed with its outcome.
func (s *Store) CloseOrder(ctx context.Context, id int64, reason string, closePx, pnl decimal.Decimal) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tester_orders SET closed_at = now(), close_reason = $2, close_px = $3, realized_pnl = $4
		WHERE id = $1
	`, id, reason, closePx, pnl)
	if err != nil {
		return fmt.Errorf("close tester order %d: %w", id, err)
	}
	return nil
}

// VersionStatsFor computes one version's track record from tester_orders. A win is a closed
// order with positive realized PnL (not close_reason='tp') — same correction as
// port.Repository.StrategyStatsFor and for the identical reason: this service's own SL can
// realize a gain in principle once levels move, so PnL sign is the honest measure of outcome.
// Today this service has no in-trade adjustment at all, so in practice sl<->loss and tp<->win will
// coincide almost always — but keeping one consistent definition across both services means a
// reader never has to remember which page uses which rule.
func (s *Store) VersionStatsFor(ctx context.Context, versionID int64) (VersionStats, error) {
	stats := VersionStats{VersionID: versionID}
	err := s.pool.QueryRow(ctx, `
		SELECT
			count(*),
			count(*) FILTER (WHERE closed_at IS NOT NULL AND realized_pnl > 0),
			count(*) FILTER (WHERE closed_at IS NOT NULL AND realized_pnl <= 0),
			count(*) FILTER (WHERE close_reason = 'tp'),
			count(*) FILTER (WHERE close_reason = 'sl'),
			count(*) FILTER (WHERE closed_at IS NULL),
			coalesce(sum(realized_pnl), 0)
		FROM tester_orders WHERE version_id = $1
	`, versionID).Scan(&stats.SignalCount, &stats.Wins, &stats.Losses, &stats.TPCloses, &stats.SLCloses,
		&stats.OpenCount, &stats.RealizedPnL)
	if err != nil {
		return VersionStats{}, fmt.Errorf("version stats for %d: %w", versionID, err)
	}
	return stats, nil
}

// RuntimeConfig is the panel-editable overrides row (tester_config) — any NULL field falls back
// to config.yaml/env at startup (see cmd/strategy-tester's loadEffectiveConfig).
type RuntimeConfig struct {
	Bar         *string
	NotionalUSD *decimal.Decimal
	Leverage    *decimal.Decimal
	// MaxOpenDuration is a Go duration string (e.g. "6h"), same format as config.yaml's
	// tester.max_open_duration — parsed the same way at startup rather than inventing a second
	// representation for the same value.
	MaxOpenDuration *string
}

// GetRuntimeConfig reads the singleton config row, if one has ever been saved.
func (s *Store) GetRuntimeConfig(ctx context.Context) (RuntimeConfig, error) {
	var rc RuntimeConfig
	err := s.pool.QueryRow(ctx, `SELECT bar, notional_usd, leverage, max_open_duration FROM tester_config WHERE id = 1`).
		Scan(&rc.Bar, &rc.NotionalUSD, &rc.Leverage, &rc.MaxOpenDuration)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RuntimeConfig{}, nil
		}
		return RuntimeConfig{}, fmt.Errorf("get tester runtime config: %w", err)
	}
	return rc, nil
}

// SaveRuntimeConfig upserts the singleton config row. Fields left nil are NOT cleared — only the
// fields the panel actually submitted are updated, via coalesce against the existing row.
func (s *Store) SaveRuntimeConfig(ctx context.Context, rc RuntimeConfig) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tester_config (id, bar, notional_usd, leverage, max_open_duration, updated_at)
		VALUES (1, $1, $2, $3, $4, now())
		ON CONFLICT (id) DO UPDATE SET
			bar = coalesce($1, tester_config.bar),
			notional_usd = coalesce($2, tester_config.notional_usd),
			leverage = coalesce($3, tester_config.leverage),
			max_open_duration = coalesce($4, tester_config.max_open_duration),
			updated_at = now()
	`, rc.Bar, rc.NotionalUSD, rc.Leverage, rc.MaxOpenDuration)
	if err != nil {
		return fmt.Errorf("save tester runtime config: %w", err)
	}
	return nil
}

// SetEnabled toggles whether a version is the one actively trading for its kind. Enabling one
// version does not disable others of the same kind automatically here — CreateVersion already
// enforces "one enabled version per kind" at creation time; this setter is for the panel's manual
// override (e.g. reverting to an older version) and disables siblings the same way.
func (s *Store) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM tester_strategy_versions WHERE id = $1`, id).Scan(&kind); err != nil {
		return fmt.Errorf("lookup kind for version %d: %w", id, err)
	}
	if enabled {
		if _, err := tx.Exec(ctx, `UPDATE tester_strategy_versions SET enabled = false WHERE kind = $1`, kind); err != nil {
			return fmt.Errorf("disable siblings for kind %q: %w", kind, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE tester_strategy_versions SET enabled = $2 WHERE id = $1`, id, enabled); err != nil {
		return fmt.Errorf("set enabled for version %d: %w", id, err)
	}
	return tx.Commit(ctx)
}
