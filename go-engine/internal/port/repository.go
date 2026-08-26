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
	ListOpenPaperOrders(ctx context.Context, instID string) ([]PaperOrder, error)
	// ListPositions returns positions (open and/or closed) across trading modes for the panel
	// (CLAUDE.md §11.4), filtered/sorted per f.
	ListPositions(ctx context.Context, f PositionFilter) ([]PaperOrder, error)
}
