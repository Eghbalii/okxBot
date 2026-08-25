package port

import "github.com/rez/okxBot/go-engine/internal/domain"

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
}
