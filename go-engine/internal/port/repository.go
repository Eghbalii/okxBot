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

	// PnLMaxPct/PnLMinPct are the peak and trough unrealized PnL this position has reached while
	// open (CLAUDE.md §15.11) — model input, not reporting. A trade that ran to 90% of its target
	// and gave it back is a different lesson from one that drifted sideways to the same current PnL.
	// Persisted rather than kept in memory because a restart would otherwise reset the high-water
	// mark to the current PnL, telling the model a round-tripped trade had never been in profit.
	// PnLMinPct is negative-ranged.
	PnLMaxPct decimal.Decimal
	PnLMinPct decimal.Decimal

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

// AccountEquity is one trading mode's shared running balance (CLAUDE.md §15.6, revised
// 2026-08-28). This replaced the per-token sub-budgets: every token trades against ONE pool, and
// how much of it goes into any single position is the RL agent's decision (bounded by Go-side
// caps), not a config constant split evenly across tokens up front.
//
// Tracked as its own row rather than summed from paper_orders on every check so "the account is at
// zero" is a fact the system can act on directly, and so reset events (ResetCount/LastResetAt) stay
// visible for training-run analysis rather than looking like unlimited free money.
type AccountEquity struct {
	Mode        string          // "paper", "demo", or "real"
	InitialUSD  decimal.Decimal // configured starting balance a reset returns to
	EquityUSD   decimal.Decimal // current running balance
	ResetCount  int
	LastResetAt *time.Time
	UpdatedAt   time.Time
}

// EquityPoint is one entry in a mode's balance timeline (CLAUDE.md §15.7). Every balance change
// writes one, so a drain-and-reset that happens overnight is visible in the panel's chart
// afterward instead of only in logs nobody was watching at the time.
type EquityPoint struct {
	ID        int64
	Mode      string
	EquityUSD decimal.Decimal // balance AFTER this change
	DeltaUSD  decimal.Decimal // signed: realized PnL for a trade, top-up amount for a reset
	Reason    string          // "trade", "reset", or "seed"
	OrderID   *int64
	InstID    string
	CreatedAt time.Time
}

// ParamChange is one recorded strategy parameter-change event (CLAUDE.md §16): either
// cmd/strategy-optimizer persisting a winning tuned candidate (Source="optimizer") or an operator
// editing a sub-strategy's params by hand via the panel (Source="manual"). Backs the panel's
// price-chart marker-line view of "when did this token's strategy params last change."
type ParamChange struct {
	ID         int64
	StrategyID int64
	InstID     string
	OldConfig  json.RawMessage // nil if the strategy had no prior config (first-ever change)
	NewConfig  json.RawMessage
	Source     string // "optimizer" or "manual"
	CreatedAt  time.Time
}

// Repository is the persistence port. internal/postgres implements this.
type Repository interface {
	SaveCandle(ctx context.Context, c Candle) error
	// ListCandles returns the most recent `limit` finalized candles for instID/bar, oldest first —
	// backs the panel's price-chart marker overlay (GET /api/candles, CLAUDE.md §16 point 6). Reads
	// the same durable `candles` hypertable PaperTrader writes to (CLAUDE.md §7); no separate
	// candle store.
	ListCandles(ctx context.Context, instID, bar string, limit int) ([]Candle, error)

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
	// UpdatePaperOrderPnLExtremes records new peak/trough unrealized PnL for an open order
	// (CLAUDE.md §15.11). Both are written together since they move as one high-water pair.
	UpdatePaperOrderPnLExtremes(ctx context.Context, id int64, maxPct, minPct decimal.Decimal) error
	// ListPositions returns positions (open and/or closed) across trading modes for the panel
	// (CLAUDE.md §11.4), filtered/sorted per f.
	ListPositions(ctx context.Context, f PositionFilter) ([]PaperOrder, error)

	// GetAccountEquity returns mode's current balance row, creating it (seeded at initialUSD, with
	// a reason="seed" history point) if it doesn't exist yet — CLAUDE.md §15.6.
	GetAccountEquity(ctx context.Context, mode string, initialUSD decimal.Decimal) (AccountEquity, error)
	// ApplyRealizedPnL adds pnl (signed) to mode's running balance and records an EquityPoint for
	// it. If the resulting equity is <= 0 AND mode is not "real", it is reset back to InitialUSD,
	// ResetCount/LastResetAt are recorded, and a second reason="reset" point is written
	// (CLAUDE.md §15.7's "give it another chance"). Real mode NEVER auto-resets — a drained real
	// account is a stop condition requiring a human decision, so it is left at/below zero and
	// reported as drained instead. Returns the updated row and whether a reset occurred.
	ApplyRealizedPnL(ctx context.Context, mode string, pnl decimal.Decimal, orderID *int64, instID string) (AccountEquity, bool, error)
	// ListEquityHistory returns mode's balance timeline for the panel's chart (CLAUDE.md §15.7),
	// oldest-first for direct plotting. A zero since means no lower bound; limit<=0 means no cap.
	ListEquityHistory(ctx context.Context, mode string, since time.Time, limit int) ([]EquityPoint, error)

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

	// RecordParamChange appends one entry to a strategy's durable parameter-change timeline
	// (CLAUDE.md §16, §16.3 step 5) — called both by cmd/strategy-optimizer after persisting a
	// winning candidate and by cmd/api's manual strategy-update handler, so the panel's timeline
	// reflects every change regardless of source.
	RecordParamChange(ctx context.Context, c ParamChange) (int64, error)
	// ListParamChanges returns instID's parameter-change history at or after since (zero time =
	// no lower bound), oldest first — the shape the panel's marker-overlay chart consumes.
	ListParamChanges(ctx context.Context, instID string, since time.Time) ([]ParamChange, error)
}
