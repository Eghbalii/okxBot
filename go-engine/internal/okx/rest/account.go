package rest

import (
	"fmt"
	"net/url"

	"github.com/rez/okxBot/go-engine/internal/domain"
	"github.com/rez/okxBot/go-engine/internal/okx"
)

// GetPositions fetches open positions via GET /api/v5/account/positions.
func (c *Client) GetPositions(instType string) ([]domain.Position, error) {
	path := "/api/v5/account/positions"
	if instType != "" {
		path += "?" + url.Values{"instType": {instType}}.Encode()
	}
	var positions []okx.Position
	if err := c.do("GET", path, nil, &positions); err != nil {
		return nil, err
	}
	out := make([]domain.Position, len(positions))
	for i, p := range positions {
		out[i] = p.ToDomain()
	}
	return out, nil
}

// GetBalance fetches account balances via GET /api/v5/account/balance.
func (c *Client) GetBalance(ccy string) ([]domain.Balance, error) {
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
	out := make([]domain.Balance, len(wrapper[0].Details))
	for i, b := range wrapper[0].Details {
		out[i] = b.ToDomain()
	}
	return out, nil
}

// GetCandles fetches recent OHLCV candles via GET /api/v5/market/candles, newest first.
func (c *Client) GetCandles(instID, bar string, limit int) ([]domain.Candle, error) {
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

	candles := make([]domain.Candle, 0, len(raw))
	for _, row := range raw {
		if len(row) < 6 {
			continue
		}
		wire := okx.Candle{Ts: row[0], Open: row[1], High: row[2], Low: row[3], Close: row[4], Vol: row[5]}
		c, err := wire.ToDomain()
		if err != nil {
			return nil, fmt.Errorf("parse candle for %s: %w", instID, err)
		}
		candles = append(candles, c)
	}
	return candles, nil
}

// GetTicker fetches a single instrument ticker via GET /api/v5/market/ticker.
func (c *Client) GetTicker(instID string) (domain.Ticker, error) {
	if instID == "" {
		return domain.Ticker{}, fmt.Errorf("instID is required")
	}
	path := "/api/v5/market/ticker?" + url.Values{"instId": {instID}}.Encode()
	var tickers []okx.Ticker
	if err := c.do("GET", path, nil, &tickers); err != nil {
		return domain.Ticker{}, err
	}
	if len(tickers) == 0 {
		return domain.Ticker{}, fmt.Errorf("no ticker data returned for %s", instID)
	}
	return tickers[0].ToDomain(), nil
}
