package port

import (
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// ExchangeClient is the port use-cases depend on for market data and trade execution — the
// exchange-agnostic interface an OKX (or future Bybit/Binance) adapter implements. See CLAUDE.md
// §10.
type ExchangeClient interface {
	GetTicker(instID string) (domain.Ticker, error)
	GetPositions(instType string) ([]domain.Position, error)
	GetBalance(ccy string) ([]domain.Balance, error)
	GetCandles(instID, bar string, limit int) ([]domain.Candle, error)
	PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error)
	SetLeverage(req domain.LeverageChange) error
	// CancelOrder cancels a still-open order (CLAUDE.md §27.5's fill-timeout mechanism: an order
	// unfilled after the configured timeout is canceled, never re-priced/retried automatically).
	// Implemented on rest.Client since before this port method existed but was unreachable from
	// usecase — see CLAUDE.md §27's audit.
	CancelOrder(instID, ordID string) error
	// GetOrder fetches an order's authoritative current state from the exchange (CLAUDE.md
	// §27.5/§27.6) — required to distinguish live/partially_filled/filled/canceled rather than
	// trusting PlaceOrder's acceptance response or this system's own bookkeeping alone.
	GetOrder(instID, ordID string) (domain.OrderStatus, error)
}

// HistoryCandleFetcher is the narrow slice of exchange access a candle backfill needs: paginated
// reads of older bars. Deliberately separate from ExchangeClient so usecase.Backfill cannot reach
// order placement or leverage changes — it is a read-only data-loading job, and the type system
// should say so rather than relying on the implementation to be careful.
type HistoryCandleFetcher interface {
	// GetHistoryCandles returns up to `limit` candles strictly older than `before` (a zero time
	// starts from the most recent), NEWEST FIRST. limit<=0 requests the adapter's maximum page
	// size. An empty result means no further history is available, which is a normal end
	// condition rather than an error.
	GetHistoryCandles(instID, bar string, before time.Time, limit int) ([]domain.Candle, error)
}
