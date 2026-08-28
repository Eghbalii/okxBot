package rest

import (
	"fmt"
	"net/url"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
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

// GetHistoryCandles fetches older candles via GET /api/v5/market/history-candles, walking backwards
// from `before` (exclusive) toward the past. Returns at most `limit` candles, NEWEST FIRST — the
// order OKX itself returns, kept as-is so the caller decides how to order for storage.
//
// This is a different endpoint from GetCandles: /market/candles only serves the most recent window
// (a few hundred bars) and cannot page backwards, so it can seed a live candle window but cannot
// build history. /market/history-candles exists precisely for backfill.
//
// `before` is OKX's `after` parameter — their naming is from the cursor's perspective ("records
// after this pagination id", i.e. older), which reads backwards to everyone else, so the Go name
// says what it means. A zero `before` starts from the most recent candle.
func (c *Client) GetHistoryCandles(instID, bar string, before time.Time, limit int) ([]domain.Candle, error) {
	if instID == "" {
		return nil, fmt.Errorf("instID is required")
	}
	if limit <= 0 || limit > maxHistoryCandlesPerPage {
		limit = maxHistoryCandlesPerPage
	}

	params := url.Values{
		"instId": {instID},
		"bar":    {bar},
		"limit":  {fmt.Sprintf("%d", limit)},
	}
	if !before.IsZero() {
		// OKX pages by millisecond timestamp; "after" means strictly older than this value.
		params.Set("after", fmt.Sprintf("%d", before.UnixMilli()))
	}

	var raw [][]string
	if err := c.do("GET", "/api/v5/market/history-candles?"+params.Encode(), nil, &raw); err != nil {
		return nil, err
	}

	candles := make([]domain.Candle, 0, len(raw))
	for _, row := range raw {
		if len(row) < 6 {
			continue
		}
		wire := okx.Candle{Ts: row[0], Open: row[1], High: row[2], Low: row[3], Close: row[4], Vol: row[5]}
		parsed, err := wire.ToDomain()
		if err != nil {
			return nil, fmt.Errorf("parse history candle for %s: %w", instID, err)
		}
		candles = append(candles, parsed)
	}
	return candles, nil
}

// maxHistoryCandlesPerPage is OKX's per-request cap on /market/history-candles. Requesting more is
// not an error — the response is simply truncated — so pagination is required regardless of how
// much history is wanted.
const maxHistoryCandlesPerPage = 100

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
