package rest

import (
	"encoding/json"
	"fmt"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/shopspring/decimal"
)

// allTickerResponse is /api/v1/contract/ticker's shape when called with NO symbol parameter: the
// same object as a single ticker, in an array. Live-verified 2026-09-13: 1200 contracts returned.
//
// Two fields make MEXC's discovery path shorter than OKX's rather than longer:
//
//   - amount24 is 24h turnover already IN DOLLARS ($1,962,147,729 for BTC_USDT), so unlike OKX's
//     volCcy24h it needs no multiplication by price. volume24 is the contract count — the wrong
//     field for cross-instrument ranking, and the one a reader would reach for by name.
//   - riseFallRate is 24h change as a FRACTION (-0.0039 = -0.39%), so MEXC's lack of an open price
//     (noted on GetTicker) costs nothing here; change is reported directly rather than derived.
//
// Numbers arrive as JSON numbers, not strings — json.Number preserves the literal digits so the
// decimal is built from what MEXC sent rather than from a float that has already lost them, the
// same reason the rest of this adapter uses it.
type allTickerResponse struct {
	Symbol       string      `json:"symbol"`
	LastPrice    json.Number `json:"lastPrice"`
	High24       json.Number `json:"high24Price"`
	Lower24      json.Number `json:"lower24Price"`
	Amount24     json.Number `json:"amount24"`
	RiseFallRate json.Number `json:"riseFallRate"`
}

// GetAllTickers fetches every MEXC futures contract's 24h snapshot in one call — the input to token
// discovery (2026-09-13). instType is accepted and ignored: MEXC's futures API has exactly one
// contract family, so there is nothing to select between. Taking the parameter anyway keeps the
// signature identical to OKX's, which is what lets the scanner hold both behind one interface
// without knowing which exchange it is talking to.
func (c *Client) GetAllTickers(instType string) ([]domain.MarketTicker, error) {
	var resp []allTickerResponse
	if err := c.doPublic("/api/v1/contract/ticker", nil, &resp); err != nil {
		return nil, fmt.Errorf("mexc get all tickers: %w", err)
	}

	hundred := decimal.NewFromInt(100)
	out := make([]domain.MarketTicker, 0, len(resp))
	for _, t := range resp {
		out = append(out, domain.MarketTicker{
			InstID:    t.Symbol,
			Last:      num(t.LastPrice),
			High24h:   num(t.High24),
			Low24h:    num(t.Lower24),
			Vol24hUSD: num(t.Amount24),
			// A fraction to a percentage. Open24h is deliberately left zero rather than
			// back-derived from price and rate: a computed value would be indistinguishable from a
			// reported one, and MEXC does not report one (same call as GetTicker).
			Change24hPct: num(t.RiseFallRate).Mul(hundred),
		})
	}
	return out, nil
}
