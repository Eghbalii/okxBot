package rest

import (
	"fmt"
	"net/url"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
)

// GetAllTickers fetches every instrument of one instType in a single call, via
// GET /api/v5/market/tickers — the input to token discovery (2026-09-13). Verified live: 479
// instruments for instType=SWAP, 207 for FUTURES (179 of them the X-Perp product real trading
// actually executes against, CLAUDE.md §33.2).
//
// One call for the whole market is the entire point: scoring hundreds of tokens by calling
// GetTicker per instrument would be hundreds of requests against a budget shared with the live
// trading path (CLAUDE.md §27.1), for data the exchange is willing to hand over at once.
func (c *Client) GetAllTickers(instType string) ([]domain.MarketTicker, error) {
	if instType == "" {
		return nil, fmt.Errorf("instType is required")
	}
	path := "/api/v5/market/tickers?" + url.Values{"instType": {instType}}.Encode()
	var wire []okx.MarketTicker
	if err := c.do("GET", path, nil, &wire); err != nil {
		return nil, fmt.Errorf("okx get all tickers %s: %w", instType, err)
	}

	out := make([]domain.MarketTicker, 0, len(wire))
	for _, t := range wire {
		out = append(out, t.ToDomain())
	}
	return out, nil
}
