// Package port defines the interfaces use-cases/services depend on, so adapters (Postgres, OKX,
// Redis, ...) can be swapped without touching business logic. See CLAUDE.md §10.
package port

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// Candle is one persisted OHLCV bar for an instrument (domain.Candle plus storage identity
// fields). The bar's timestamp lives on the embedded domain.Candle.Timestamp.
type Candle struct {
	InstID string
	Bar    string
	domain.Candle
}

// StrategyConfig is a persisted, pluggable signal-generator configuration (see CLAUDE.md §9).
// Every strategy descends from a locked "origin" row (IsOrigin true, ClonedFrom nil) representing
// the built-in implementation as shipped; sub-strategies (ClonedFrom set) store their own
// parameter overrides in Config without mutating the origin (CLAUDE.md §11.3).
type StrategyConfig struct {
	ID         int64
	Name       string
	InstIDs    []string
	Kind       string // e.g. "rsi_sma", "macd", "ict_smc", "custom"
	Config     json.RawMessage
	Enabled    bool
	IsOrigin   bool
	ClonedFrom *int64
}

// StrategyAssignment is a durable token+timeframe binding for a strategy (CLAUDE.md §11.3): which
// strategy variant evaluates on which instrument's candle close. Loaded from Postgres on every
// process start so a crash/restart never loses which variant was running where — this is
// distinct from usecase.StrategyAssignment, which pairs a *live* strategy.Strategy value (already
// resolved from this row's StrategyID+Config) with its Bar for the running PaperTrader.
type StrategyAssignment struct {
	ID         int64
	StrategyID int64
	InstID     string
	Bar        string
	Enabled    bool
}

// StrategyStats summarizes one strategy's paper-trading track record (CLAUDE.md §11.3),
// computed from paper_orders — no separately maintained counters.
type StrategyStats struct {
	StrategyID   int64
	SignalCount  int64
	Wins         int64 // close_reason = 'tp'
	Losses       int64 // close_reason = 'sl'
	OpenCount    int64
	RealizedPnL  decimal.Decimal
	FirstOpened  *time.Time
	LastActivity *time.Time
}

// PositionFilter selects/sorts positions across trading modes for the panel (CLAUDE.md §11.4).
type PositionFilter struct {
	Mode   string // "paper", "demo", "real", or "" for all
	InstID string // "" for all
	Open   *bool  // nil = both open and closed
	// SortBy: "opened_at" (default), "closed_at", "pnl", "inst_id". SortDesc reverses order.
	SortBy   string
	SortDesc bool
}

// PaperOrder is a virtual (forward-test) trade opened by the Paper Trading Engine (CLAUDE.md §8).
type PaperOrder struct {
	ID           int64
	InstID       string
	StrategyID   *int64
	Side         string // "buy" or "sell"
	EntryPx      decimal.Decimal
	SLPx         *decimal.Decimal
	TPPx         *decimal.Decimal
	Size         decimal.Decimal
	Leverage     decimal.Decimal
	OpenedAt     time.Time
	ClosedAt     *time.Time
	CloseReason  *string // "sl", "tp", "manual", "timeout"
	ClosePx      *decimal.Decimal
	RealizedPnL  *decimal.Decimal
	FeaturesJSON json.RawMessage
	Mode         string // "paper", "demo", or "real" (CLAUDE.md §11.4); defaults to "paper"

	// ParentOrderID/Variant implement the SL/TP shadow-fork mechanic (CLAUDE.md §15.4): when the RL
	// agent proposes an in-trade SL/TP adjustment, the original order (Variant="baseline",
	// ParentOrderID=nil) is never edited — a linked fork (Variant="rl_adjusted", ParentOrderID set
	// to the original's ID) carries the adjustment instead, and both are monitored to completion
	// for later comparison. A fork is tracking-only: it must never be double-counted toward a
	// token's budget/reward (§15.6/§15.7) — callers filter to Variant="baseline" for that.
	ParentOrderID *int64
	Variant       string // "baseline" (default) or "rl_adjusted"
}

// VariantStats summarizes one SL/TP-adjustment variant's closed-trade track record for the
// baseline-vs-rl_adjusted comparison (CLAUDE.md §15.4) — one of these per variant, so the caller
// can put them side by side.
type VariantStats struct {
	Variant     string // "baseline" or "rl_adjusted"
	ClosedCount int64
	Wins        int64 // close_reason = 'tp'
	Losses      int64 // close_reason = 'sl'
	RealizedPnL decimal.Decimal
}

// SLTPAdjustmentPair links one baseline order to its rl_adjusted fork (CLAUDE.md §15.4) for
// trade-level (not just aggregate) comparison — e.g. a panel table of "this specific decision
// helped/hurt."  Either side may still be open (ClosedAt/RealizedPnL nil) if the pair hasn't
// resolved yet.
type SLTPAdjustmentPair struct {
	InstID          string
	BaselineOrder   PaperOrder
	RLAdjustedOrder PaperOrder
}

// TokenBudget is a token's running paper/demo-mode sub-budget (CLAUDE.md §15.6/§15.7) — tracked
// separately from summing paper_orders on every check so "this token is at zero" is a fact the
// system can act on directly, and so reset events (ResetCount/LastResetAt) are visible for
// training-run analysis rather than looking like unlimited free money.
type TokenBudget struct {
	InstID      string
	BudgetUSD   decimal.Decimal // configured per-token notional a reset tops back up to
	EquityUSD   decimal.Decimal // current running balance
	ResetCount  int
	LastResetAt *time.Time
	UpdatedAt   time.Time
}

// Repository is the persistence port. internal/postgres implements this.
type Repository interface {
	SaveCandle(ctx context.Context, c Candle) error

	CreateStrategy(ctx context.Context, s StrategyConfig) (int64, error)
	GetStrategy(ctx context.Context, id int64) (StrategyConfig, error)
	ListStrategies(ctx context.Context, instID string, enabledOnly bool) ([]StrategyConfig, error)
	UpdateStrategyConfig(ctx context.Context, id int64, config json.RawMessage, enabled bool) error
	DeleteStrategy(ctx context.Context, id int64) error
	// ResetStrategyToOrigin overwrites a sub-strategy's Config with its origin's current Config.
	ResetStrategyToOrigin(ctx context.Context, id int64) error

	// CreateAssignment binds a strategy to an instrument+timeframe. Durable so a restart reloads
	// exactly which variant was running where (CLAUDE.md §11.3) instead of relying on in-code
	// wiring like cmd/paper-trader/main.go's current hardcoded []usecase.StrategyAssignment.
	CreateAssignment(ctx context.Context, a StrategyAssignment) (int64, error)
	ListAssignments(ctx context.Context, instID string, enabledOnly bool) ([]StrategyAssignment, error)
	SetAssignmentEnabled(ctx context.Context, id int64, enabled bool) error
	DeleteAssignment(ctx context.Context, id int64) error

	// StrategyStatsFor computes per-strategy track record from paper_orders (CLAUDE.md §11.3).
	StrategyStatsFor(ctx context.Context, strategyID int64) (StrategyStats, error)

	OpenPaperOrder(ctx context.Context, o PaperOrder) (int64, error)
	ClosePaperOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal) error
	// UpdatePaperOrderSLTP applies an in-trade SL/TP adjustment to an open order (CLAUDE.md §15.4).
	// Callers MUST have already run the proposed new prices through the ratchet clamp
	// (usecase.RatchetSLTP) before calling this — the repository does not re-validate the ratchet
	// constraint itself. Deprecated for RL-driven adjustments as of the shadow-fork mechanic below
	// (kept for any future non-forking/manual SL-TP edit path); the RL loop calls
	// ForkPaperOrderWithSLTP instead.
	UpdatePaperOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal) error
	// ForkPaperOrderWithSLTP implements the SL/TP shadow-fork mechanic (CLAUDE.md §15.4): creates a
	// new PaperOrder row cloned from the still-open order at parentID (same inst_id/side/entry_px/
	// strategy_id/size/leverage/opened_at) but with slPx/tpPx applied and Variant="rl_adjusted",
	// ParentOrderID=parentID. The parent order itself is left untouched. Returns the new fork's id.
	// Callers MUST have already run slPx/tpPx through the ratchet clamp (usecase.RatchetSLTP).
	ForkPaperOrderWithSLTP(ctx context.Context, parentID int64, slPx, tpPx *decimal.Decimal) (int64, error)
	ListOpenPaperOrders(ctx context.Context, instID string) ([]PaperOrder, error)
	// ListPositions returns positions (open and/or closed) across trading modes for the panel
	// (CLAUDE.md §11.4), filtered/sorted per f.
	ListPositions(ctx context.Context, f PositionFilter) ([]PaperOrder, error)

	// GetTokenBudget returns inst_id's current budget row, creating it (seeded at initialBudgetUSD,
	// EquityUSD=initialBudgetUSD) if it doesn't exist yet — CLAUDE.md §15.7.
	GetTokenBudget(ctx context.Context, instID string, initialBudgetUSD decimal.Decimal) (TokenBudget, error)
	// ApplyTokenPnL adds pnl (signed) to inst_id's running equity. If the resulting equity is
	// <= 0, it is reset back to its configured BudgetUSD and ResetCount/LastResetAt are recorded
	// (CLAUDE.md §15.7's "give it another chance" — paper/demo mode only, never called from a
	// real-money path). Returns the updated row and whether a reset occurred.
	ApplyTokenPnL(ctx context.Context, instID string, pnl decimal.Decimal) (TokenBudget, bool, error)

	// SLTPAdjustmentStats aggregates CLOSED baseline vs. rl_adjusted trades into one VariantStats
	// per variant (CLAUDE.md §15.4's A/B comparison) — instID/since filter the underlying
	// paper_orders; instID="" means all instruments, since=zero time means no lower bound. Only
	// baseline orders that actually have an rl_adjusted fork are counted on the "baseline" side, so
	// the comparison is apples-to-apples (a baseline order nobody ever proposed adjusting isn't
	// counted as evidence either way).
	SLTPAdjustmentStats(ctx context.Context, instID string, since time.Time) ([]VariantStats, error)
	// ListSLTPAdjustmentPairs returns every baseline/rl_adjusted pair for instID (or all
	// instruments if ""), most-recently-opened first, for trade-level (not just aggregate)
	// review — CLAUDE.md §15.4.
	ListSLTPAdjustmentPairs(ctx context.Context, instID string) ([]SLTPAdjustmentPair, error)
}
