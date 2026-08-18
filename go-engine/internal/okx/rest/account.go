package rest

import (
	"fmt"
	"net/url"

	"github.com/rez/okxBot/go-engine/internal/okx"
)

// GetPositions fetches open positions via GET /api/v5/account/positions.
func (c *Client) GetPositions(instType string) ([]okx.Position, error) {
	path := "/api/v5/account/positions"
	if instType != "" {
		path += "?" + url.Values{"instType": {instType}}.Encode()
	}
	var positions []okx.Position
	if err := c.do("GET", path, nil, &positions); err != nil {
		return nil, err
	}
	return positions, nil
}

// GetBalance fetches account balances via GET /api/v5/account/balance.
func (c *Client) GetBalance(ccy string) ([]okx.Balance, error) {
	path := "/api/v5/account/balance"
	if ccy != "" {
		path += "?" + url.Values{"ccy": {ccy}}.Encode()
	}
	var wrapper []struct {
		Details []okx.Balance `json:"details"`
	}
	if err := c.do("GET", path, nil, &wrapper); err != nil {
		return nil, err
	}
	if len(wrapper) == 0 {
		return nil, nil
	}
	return wrapper[0].Details, nil
}

// GetTicker fetches a single instrument ticker via GET /api/v5/market/ticker.
func (c *Client) GetTicker(instID string) (*okx.Ticker, error) {
	if instID == "" {
		return nil, fmt.Errorf("instID is required")
	}
	path := "/api/v5/market/ticker?" + url.Values{"instId": {instID}}.Encode()
	var tickers []okx.Ticker
	if err := c.do("GET", path, nil, &tickers); err != nil {
		return nil, err
	}
	if len(tickers) == 0 {
		return nil, fmt.Errorf("no ticker data returned for %s", instID)
	}
	return &tickers[0], nil
}
