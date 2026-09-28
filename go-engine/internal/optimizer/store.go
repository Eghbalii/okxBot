// Package optimizer implements cmd/strategy-optimizer (rebuilt 2026-09-27, replacing the
// abandoned live-trial design — CLAUDE.md §21/§33.5 record why cmd/strategy-optimizer/
// cmd/strategy-tester, both removed entirely, never worked): Optuna proposes candidate
// parameter sets per (kind, inst_id, bar, exchange, risk_profile) lineage, each candidate is
// VALIDATED by replaying it against real historical candles through the existing
// internal/backtest.Runner (docs/RL_V8_PLAN.md's warm-start engine — never reimplemented here),
// and only a candidate that clears the panel-configurable thresholds is promoted into
// production's strategies/strategy_assignments so it actually starts paper trading.
package optimizer

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

// Lineage identifies one (kind, inst_id, bar, exchange, risk_profile) tuning target — the unit
// this whole pipeline versions independently, per the operator's explicit "a strategy that only
// works well on one token is a real win" instruction: two lineages sharing a kind never share a
// candidate history, a status, or an in-flight optimizer slot.
type Lineage struct {
	Kind        string
	InstID      string
	Bar         string
	Exchange    string
	RiskProfile string // "low" or "high"
}

// Candidate is one row of strategy_candidates — a proposed, backtested, or live parameter set for
// one Lineage.
type Candidate struct {
	ID       int64
	Lineage  Lineage
	Config   json.RawMessage
	ParentID *int64

	Generation      int
	BacktestUpdates int
	PaperUpdates    int

	Status string // proposed | backtesting | backtest_rejected | backtest_passed | paper_active | paper_replaced

	TrialID *int
	Source  string // origin | manual | optimizer

	BacktestTradeCount    *int
	BacktestWinRatePct    *decimal.Decimal
	BacktestRealizedPnL   *decimal.Decimal
	BacktestResets        *int
	BacktestSignificanceT *decimal.Decimal
	BacktestFrom          *time.Time
	BacktestTo            *time.Time
	BacktestRanAt         *time.Time
	BacktestRejectReason  *string

	PromotedAt *time.Time
	StrategyID *int64

	CreatedAt time.Time
	UpdatedAt time.Time
}

// DisplayName renders the operator's requested naming convention:
// <kind>_<TOKEN>_R<leverage>_G<generation>_B<backtest_updates>_P<paper_updates> — computed here
// rather than stored, so changing the convention later needs no backfill (the same reasoning the
// old tester_strategy_versions used for its own "{kind}_v{version}" display name).
func (c Candidate) DisplayName(leverage int) string {
	return fmt.Sprintf("%s_%s_R%d_G%d_B%d_P%d",
		c.Lineage.Kind, c.Lineage.InstID, leverage, c.Generation, c.BacktestUpdates, c.PaperUpdates)
}

// ValidationConfig is one risk profile's panel-editable promotion thresholds
// (strategy_optimizer_config) — combined deliberately ("ترکیبی باشه نه فقط تمرکز روی یک چیز"),
// matching every dimension internal/backtest.Result already reports per strategy.
type ValidationConfig struct {
	RiskProfile      string
	MinTrades        int
	MinWinRatePct    decimal.Decimal
	MinRealizedPnL   decimal.Decimal
	MinSignificanceT decimal.Decimal
	MaxResets        int
	BacktestLookback string // e.g. "30d", parsed Go-side
	UpdatedAt        time.Time
}

// Store is a thin Postgres access layer over strategy_candidates/strategy_optimizer_config/
// strategy_optimizer_state — a plain struct on the shared pool, matching this codebase's own
// established precedent for a single-database internal tool (e.g. the old internal/optimizer.
// TrialStore, internal/tester.Store before both were removed).
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const candidateColumns = `
	id, kind, inst_id, bar, exchange, risk_profile, config, parent_candidate_id,
	generation, backtest_updates, paper_updates, status, trial_id, source,
	backtest_trade_count, backtest_win_rate_pct, backtest_realized_pnl, backtest_resets,
	backtest_significance_t, backtest_from, backtest_to, backtest_ran_at, backtest_rejection_reason,
	promoted_at, strategy_id, created_at, updated_at`

func scanCandidate(row pgx.Row) (Candidate, error) {
	var c Candidate
	err := row.Scan(
		&c.ID, &c.Lineage.Kind, &c.Lineage.InstID, &c.Lineage.Bar, &c.Lineage.Exchange, &c.Lineage.RiskProfile,
		&c.Config, &c.ParentID,
		&c.Generation, &c.BacktestUpdates, &c.PaperUpdates, &c.Status, &c.TrialID, &c.Source,
		&c.BacktestTradeCount, &c.BacktestWinRatePct, &c.BacktestRealizedPnL, &c.BacktestResets,
		&c.BacktestSignificanceT, &c.BacktestFrom, &c.BacktestTo, &c.BacktestRanAt, &c.BacktestRejectReason,
		&c.PromotedAt, &c.StrategyID, &c.CreatedAt, &c.UpdatedAt,
	)
	return c, err
}

// EnsureOriginCandidate returns the id of a lineage's generation-1 row, creating it (empty
// config, no parent, source='origin') if it doesn't exist yet. Also used to seed each of the 12
// pre-existing "_v2" kinds as an already-existing G1 (operator's explicit instruction, 2026-09-27:
// treat them as first-generation optimizer output, not a special case).
//
// Check-then-insert, not an upsert: strategy_candidates deliberately has no unique constraint on
// (kind, inst_id, bar, exchange, risk_profile, generation) — a lineage accumulates MANY candidates
// over its life, so "the first generation-1 row" is a query, not a key ON CONFLICT can target.
// A race between two callers ensuring the same lineage's origin simultaneously would insert two
// generation-1 rows; acceptable here since this is only ever called from the scheduler's own
// single-goroutine tick loop (never concurrently for the same lineage) — a caller with a genuine
// concurrency need should wrap this in its own advisory lock rather than this function guessing.
func (s *Store) EnsureOriginCandidate(ctx context.Context, l Lineage) (int64, error) {
	if id, ok, err := s.originCandidateID(ctx, l); err != nil {
		return 0, err
	} else if ok {
		return id, nil
	}

	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO strategy_candidates (kind, inst_id, bar, exchange, risk_profile, config, generation, source, status)
		VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, 1, 'origin', 'backtest_passed')
		RETURNING id
	`, l.Kind, l.InstID, l.Bar, l.Exchange, l.RiskProfile).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure origin candidate for %+v: %w", l, err)
	}
	return id, nil
}

func (s *Store) originCandidateID(ctx context.Context, l Lineage) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM strategy_candidates
		WHERE kind = $1 AND inst_id = $2 AND bar = $3 AND exchange = $4 AND risk_profile = $5 AND generation = 1
		ORDER BY id LIMIT 1
	`, l.Kind, l.InstID, l.Bar, l.Exchange, l.RiskProfile).Scan(&id)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find origin candidate for %+v: %w", l, err)
	}
	return id, true, nil
}

// CreateCandidate inserts a new proposed candidate for lineage, cloned from parentID.
// generation/backtestUpdates/paperUpdates are computed by the caller (optimizer_loop.go) from the
// parent's own counters plus the phase the new candidate enters — this function only persists
// what it's given, so the counting RULE lives in one place, not split across the store.
func (s *Store) CreateCandidate(ctx context.Context, l Lineage, config json.RawMessage, parentID int64, generation, backtestUpdates, paperUpdates int, source string, trialID *int) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO strategy_candidates
			(kind, inst_id, bar, exchange, risk_profile, config, parent_candidate_id,
			 generation, backtest_updates, paper_updates, status, source, trial_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'proposed', $11, $12)
		RETURNING id
	`, l.Kind, l.InstID, l.Bar, l.Exchange, l.RiskProfile, config, parentID,
		generation, backtestUpdates, paperUpdates, source, trialID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create candidate for %+v: %w", l, err)
	}
	return id, nil
}

func (s *Store) GetCandidate(ctx context.Context, id int64) (Candidate, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+candidateColumns+` FROM strategy_candidates WHERE id = $1`, id)
	c, err := scanCandidate(row)
	if err != nil {
		return Candidate{}, fmt.Errorf("get candidate %d: %w", id, err)
	}
	return c, nil
}

// ListCandidatesForLineage returns every candidate ever proposed for l, newest first — the full
// history a "best across the lineage's entire history" proposal (mirroring the old tester's
// proposeCandidate precedent) scores from.
func (s *Store) ListCandidatesForLineage(ctx context.Context, l Lineage) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+candidateColumns+` FROM strategy_candidates
		WHERE kind = $1 AND inst_id = $2 AND bar = $3 AND exchange = $4 AND risk_profile = $5
		ORDER BY id DESC
	`, l.Kind, l.InstID, l.Bar, l.Exchange, l.RiskProfile)
	if err != nil {
		return nil, fmt.Errorf("list candidates for %+v: %w", l, err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCandidatesByStatus returns up to limit candidates across EVERY lineage matching status,
// newest first — the panel's "Active / Backtested / Rejected" browser needs this because
// ListCandidatesForLineage only ever answers for one exact (kind, instId, bar, exchange,
// riskProfile) tuple, and iterating that per-lineage call across thousands of lineages to find
// e.g. every currently-paper_active row would mean thousands of sequential queries. One query,
// scoped by status alone, is what the panel actually needs to show "what is the pipeline doing
// right now" without walking its own lineage list first.
func (s *Store) ListCandidatesByStatus(ctx context.Context, status string, limit int) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+candidateColumns+` FROM strategy_candidates
		WHERE status = $1
		ORDER BY id DESC
		LIMIT $2
	`, status, limit)
	if err != nil {
		return nil, fmt.Errorf("list candidates by status %q: %w", status, err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReplacedByFor finds the candidate that replaced a 'paper_replaced' candidate for its own
// lineage — whichever candidate was promoted (promoted_at set) at or immediately after this one
// was demoted (updated_at, the exact moment PromoteCandidate's transaction flipped its status).
// This is what lets the panel show a REAL, service-computed reason a candidate stopped trading
// ("replaced by a candidate scoring $X vs this one's $Y"), rather than a hardcoded label — the
// old backtest_rejection_reason column is only ever set for backtest_rejected candidates, which
// never traded at all; a paper_replaced candidate was never "rejected" in that sense; it just lost
// to something better, and this is what actually happened.
func (s *Store) ReplacedByFor(ctx context.Context, candidateID int64) (Candidate, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+candidateColumns+` FROM strategy_candidates c
		WHERE (c.kind, c.inst_id, c.bar, c.exchange, c.risk_profile) = (
			SELECT kind, inst_id, bar, exchange, risk_profile FROM strategy_candidates WHERE id = $1
		)
		AND c.id <> $1
		AND c.promoted_at >= (SELECT updated_at FROM strategy_candidates WHERE id = $1)
		ORDER BY c.promoted_at ASC
		LIMIT 1
	`, candidateID)
	c, err := scanCandidate(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Candidate{}, false, nil
		}
		return Candidate{}, false, fmt.Errorf("find replacement for candidate %d: %w", candidateID, err)
	}
	return c, true, nil
}

// ActiveCandidate returns l's current 'paper_active' candidate, if any.
func (s *Store) ActiveCandidate(ctx context.Context, l Lineage) (Candidate, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+candidateColumns+` FROM strategy_candidates
		WHERE kind = $1 AND inst_id = $2 AND bar = $3 AND exchange = $4 AND risk_profile = $5 AND status = 'paper_active'
	`, l.Kind, l.InstID, l.Bar, l.Exchange, l.RiskProfile)
	c, err := scanCandidate(row)
	if err == pgx.ErrNoRows {
		return Candidate{}, false, nil
	}
	if err != nil {
		return Candidate{}, false, fmt.Errorf("active candidate for %+v: %w", l, err)
	}
	return c, true, nil
}

// RecordBacktestResult stores a completed backtest.Result's relevant fields against candidateID
// and transitions status to 'backtest_passed' or 'backtest_rejected'.
func (s *Store) RecordBacktestResult(ctx context.Context, candidateID int64, passed bool, rejectReason string,
	tradeCount int, winRatePct, realizedPnL decimal.Decimal, resets int, significanceT *decimal.Decimal, from, to time.Time) error {
	status := "backtest_rejected"
	var reason *string
	if passed {
		status = "backtest_passed"
	} else {
		reason = &rejectReason
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE strategy_candidates SET
			status = $1,
			backtest_trade_count = $2, backtest_win_rate_pct = $3, backtest_realized_pnl = $4,
			backtest_resets = $5, backtest_significance_t = $6,
			backtest_from = $7, backtest_to = $8, backtest_ran_at = now(),
			backtest_rejection_reason = $9,
			updated_at = now()
		WHERE id = $10
	`, status, tradeCount, winRatePct, realizedPnL, resets, significanceT, from, to, reason, candidateID)
	if err != nil {
		return fmt.Errorf("record backtest result for candidate %d: %w", candidateID, err)
	}
	return nil
}

// SetBacktesting marks candidateID as currently being backtested, so a concurrent scheduler tick
// doesn't pick the same 'proposed' candidate twice.
func (s *Store) SetBacktesting(ctx context.Context, candidateID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE strategy_candidates SET status = 'backtesting', updated_at = now() WHERE id = $1`, candidateID)
	if err != nil {
		return fmt.Errorf("set backtesting for candidate %d: %w", candidateID, err)
	}
	return nil
}

// PromoteCandidate transitions candidateID to 'paper_active', demoting whatever was previously
// 'paper_active' for the SAME lineage to 'paper_replaced' — one transaction, so the unique
// "at most one paper_active per lineage" index (migration 000039) is never violated even for an
// instant. This is the ONE place a candidate is ever demoted from 'paper_active', unlike the old
// tester's CreateVersion (§21's bug), which disabled every sibling on every insert regardless of
// whether one was actually being replaced.
func (s *Store) PromoteCandidate(ctx context.Context, candidateID int64, strategyID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin promote tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var l Lineage
	if err := tx.QueryRow(ctx, `SELECT kind, inst_id, bar, exchange, risk_profile FROM strategy_candidates WHERE id = $1`, candidateID).
		Scan(&l.Kind, &l.InstID, &l.Bar, &l.Exchange, &l.RiskProfile); err != nil {
		return fmt.Errorf("look up lineage for candidate %d: %w", candidateID, err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE strategy_candidates SET status = 'paper_replaced', updated_at = now()
		WHERE kind = $1 AND inst_id = $2 AND bar = $3 AND exchange = $4 AND risk_profile = $5
		  AND status = 'paper_active' AND id <> $6
	`, l.Kind, l.InstID, l.Bar, l.Exchange, l.RiskProfile, candidateID); err != nil {
		return fmt.Errorf("demote previous active candidate for %+v: %w", l, err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE strategy_candidates SET status = 'paper_active', promoted_at = now(), strategy_id = $1, updated_at = now()
		WHERE id = $2
	`, strategyID, candidateID); err != nil {
		return fmt.Errorf("promote candidate %d: %w", candidateID, err)
	}

	return tx.Commit(ctx)
}

// IncrementPaperUpdates bumps candidateID's paper_updates counter — called when the optimizer
// updates an already-'paper_active' lineage's live params in place (rather than creating a new
// candidate), matching the naming scheme's own P-counter semantics.
func (s *Store) IncrementPaperUpdates(ctx context.Context, candidateID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE strategy_candidates SET paper_updates = paper_updates + 1, updated_at = now() WHERE id = $1`, candidateID)
	if err != nil {
		return fmt.Errorf("increment paper updates for candidate %d: %w", candidateID, err)
	}
	return nil
}

// GetValidationConfig reads a risk profile's promotion thresholds.
func (s *Store) GetValidationConfig(ctx context.Context, riskProfile string) (ValidationConfig, error) {
	var v ValidationConfig
	err := s.pool.QueryRow(ctx, `
		SELECT risk_profile, min_trades, min_win_rate_pct, min_realized_pnl, min_significance_t, max_resets, backtest_lookback, updated_at
		FROM strategy_optimizer_config WHERE risk_profile = $1
	`, riskProfile).Scan(&v.RiskProfile, &v.MinTrades, &v.MinWinRatePct, &v.MinRealizedPnL, &v.MinSignificanceT, &v.MaxResets, &v.BacktestLookback, &v.UpdatedAt)
	if err != nil {
		return ValidationConfig{}, fmt.Errorf("get validation config for %q: %w", riskProfile, err)
	}
	return v, nil
}

// SaveValidationConfig writes a risk profile's promotion thresholds — the panel-editable surface
// the operator explicitly asked for.
func (s *Store) SaveValidationConfig(ctx context.Context, v ValidationConfig) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE strategy_optimizer_config SET
			min_trades = $2, min_win_rate_pct = $3, min_realized_pnl = $4,
			min_significance_t = $5, max_resets = $6, backtest_lookback = $7, updated_at = now()
		WHERE risk_profile = $1
	`, v.RiskProfile, v.MinTrades, v.MinWinRatePct, v.MinRealizedPnL, v.MinSignificanceT, v.MaxResets, v.BacktestLookback)
	if err != nil {
		return fmt.Errorf("save validation config for %q: %w", v.RiskProfile, err)
	}
	return nil
}

// SetOptimizerState records l's current in-flight backtest candidate (nil clears it).
func (s *Store) SetOptimizerState(ctx context.Context, l Lineage, candidateID *int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO strategy_optimizer_state (kind, inst_id, bar, exchange, risk_profile, candidate_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (kind, inst_id, bar, exchange, risk_profile)
		DO UPDATE SET candidate_id = EXCLUDED.candidate_id, updated_at = now()
	`, l.Kind, l.InstID, l.Bar, l.Exchange, l.RiskProfile, candidateID)
	if err != nil {
		return fmt.Errorf("set optimizer state for %+v: %w", l, err)
	}
	return nil
}

// GetOptimizerState returns l's current in-flight candidate id, if any.
func (s *Store) GetOptimizerState(ctx context.Context, l Lineage) (*int64, error) {
	var id *int64
	err := s.pool.QueryRow(ctx, `
		SELECT candidate_id FROM strategy_optimizer_state
		WHERE kind = $1 AND inst_id = $2 AND bar = $3 AND exchange = $4 AND risk_profile = $5
	`, l.Kind, l.InstID, l.Bar, l.Exchange, l.RiskProfile).Scan(&id)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get optimizer state for %+v: %w", l, err)
	}
	return id, nil
}
