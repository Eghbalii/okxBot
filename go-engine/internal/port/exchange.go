package port

import (
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
	// GetInstrument fetches one instId's contract-shape metadata (CtVal/LotSz/MinSz) — required to
	// convert a desired size into a valid contract count before placing a real order (CLAUDE.md
	// §14/§27's known gap, found load-bearing 2026-09-04 against OKX's real-trading-eligible
	// BTC-USD_UM_XPERP instrument, whose CtVal/LotSz differ sharply from the classic SWAP
	// instruments this codebase otherwise assumes a multiplier of 1 for).
	GetInstrument(instType, instID string) (domain.Instrument, error)
	// PlaceAlgoOrder places a resting conditional stop-loss/take-profit order on the exchange,
	// returning OKX's algoId. This is what makes a real position's protection survive this process
	// (2026-09-09 request): before it, SL/TP lived only as columns this service's own tick monitor
	// watched, so a crash, restart, deploy, or stalled tick feed left real capital unprotected.
	PlaceAlgoOrder(req domain.AlgoOrderRequest) (string, error)
	// AmendAlgoOrder moves an already-resting conditional order's trigger price(s) in place — the
	// exchange-side half of every SL/TP change, whether the model made it or an operator did from
	// the panel, so the exchange never holds a level this system has since moved on from.
	AmendAlgoOrder(req domain.AlgoOrderAmend) error
	// CancelAlgoOrder removes a resting conditional order — called whenever the position it
	// protects is closed by another route, so a flattened position cannot leave a live protective
	// order behind that would later open a NEW position in the opposite direction.
	CancelAlgoOrder(instID, algoID string) error
	// GetAlgoOrder reports whether a protective order is still resting on the exchange. The
	// exchange is primary for SL/TP, so this is the check that keeps that trust honest rather than
	// assuming a once-successful placement stays valid forever.
	GetAlgoOrder(instID, algoID string) (domain.AlgoOrderStatus, error)
	// GetFundingRateHistory fetches recent settled funding periods for one instrument (CLAUDE.md,
	// 2026-09-06) — an unauthenticated public endpoint, but routed through the same client/gateway
	// path as every other OKX call for consistency. Oldest-first.
	GetFundingRateHistory(instID string, limit int) ([]domain.FundingRate, error)
}
