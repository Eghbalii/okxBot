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

// GetCandles fetches recent OHLCV candles via GET /api/v5/market/candles, newest first.
func (c *Client) GetCandles(instID, bar string, limit int) ([]okx.Candle, error) {
	if instID == "" {
		return nil, fmt.Errorf("instID is required")
	}
	path := "/api/v5/market/candles?" + url.Values{
		"instId": {instID},
		"bar":    {bar},
		"limit":  {fmt.Sprintf("%d", limit)},
	}.Encode()

	var raw [][]string
	if err := c.do("GET", path, nil, &raw); err != nil {
		return nil, err
	}

	candles := make([]okx.Candle, 0, len(raw))
	for _, row := range raw {
		if len(row) < 6 {
			continue
		}
		candle := okx.Candle{Ts: row[0], Open: row[1], High: row[2], Low: row[3], Close: row[4], Vol: row[5]}
		if len(row) >= 9 {
			candle.Confirm = row[8]
		}
		candles = append(candles, candle)
	}
	return candles, nil
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
