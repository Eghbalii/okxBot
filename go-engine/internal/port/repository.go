// Package port defines the interfaces use-cases/services depend on, so adapters (Postgres, OKX,
// Redis, ...) can be swapped without touching business logic. See CLAUDE.md §10.
package port

import (
	"context"
	"encoding/json"
	"time"
)

// Candle is one OHLCV bar for an instrument.
type Candle struct {
	InstID string
	Bar    string
	Ts     time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

// StrategyConfig is a persisted, pluggable signal-generator configuration (see CLAUDE.md §9).
type StrategyConfig struct {
	ID         int64
	Name       string
	InstIDs    []string
	Kind       string // e.g. "rsi_sma", "macd", "ict_smc", "custom"
	Config     json.RawMessage
	Enabled    bool
	ClonedFrom *int64
}

// PaperOrder is a virtual (forward-test) trade opened by the Paper Trading Engine (CLAUDE.md §8).
type PaperOrder struct {
	ID           int64
	InstID       string
	StrategyID   *int64
	Side         string // "buy" or "sell"
	EntryPx      float64
	SLPx         *float64
	TPPx         *float64
	Size         float64
	Leverage     float64
	OpenedAt     time.Time
	ClosedAt     *time.Time
	CloseReason  *string // "sl", "tp", "manual", "timeout"
	ClosePx      *float64
	RealizedPnL  *float64
	FeaturesJSON json.RawMessage
}

// Repository is the persistence port. internal/postgres implements this.
type Repository interface {
	SaveCandle(ctx context.Context, c Candle) error

	CreateStrategy(ctx context.Context, s StrategyConfig) (int64, error)
	ListStrategies(ctx context.Context, instID string, enabledOnly bool) ([]StrategyConfig, error)

	OpenPaperOrder(ctx context.Context, o PaperOrder) (int64, error)
	ClosePaperOrder(ctx context.Context, id int64, closePx float64, reason string, realizedPnL float64) error
	ListOpenPaperOrders(ctx context.Context, instID string) ([]PaperOrder, error)
}
