package rest

import (
	"fmt"
	"net/url"

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

// GetFundingRateHistory fetches recent SETTLED funding periods for one instrument (2026-09-06),
// via GET /api/v5/public/funding-rate-history — unauthenticated like GetInstrument, routed through
// the same signed do() for consistency. Returns oldest-first (OKX returns newest-first; reversed
// here) so a caller storing rows can insert in chronological order.
func (c *Client) GetFundingRateHistory(instID string, limit int) ([]domain.FundingRate, error) {
	if instID == "" {
		return nil, fmt.Errorf("instID is required")
	}
	if limit <= 0 {
		limit = 10
	}
	path := "/api/v5/public/funding-rate-history?" + url.Values{
		"instId": {instID},
		"limit":  {fmt.Sprintf("%d", limit)},
	}.Encode()

	var wire []okx.FundingRate
	if err := c.do("GET", path, nil, &wire); err != nil {
		return nil, err
	}

	out := make([]domain.FundingRate, 0, len(wire))
	for _, w := range wire {
		fr, err := w.ToDomain()
		if err != nil {
			return nil, fmt.Errorf("get funding rate history for %s: %w", instID, err)
		}
		out = append(out, fr)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// GetAccountConfig fetches account-wide settings via GET /api/v5/account/config — today only
// PosMode ("net_mode" or "long_short_mode") is read, the one setting genuinely account-wide rather
// than per-order (domain.AccountConfig's own doc comment).
func (c *Client) GetAccountConfig() (domain.AccountConfig, error) {
	var configs []okx.AccountConfig
	if err := c.do("GET", "/api/v5/account/config", nil, &configs); err != nil {
		return domain.AccountConfig{}, err
	}
	if len(configs) == 0 {
		return domain.AccountConfig{}, fmt.Errorf("no account config returned")
	}
	return configs[0].ToDomain(), nil
}

// SetPositionMode switches the account between net mode and hedge (long/short) mode via
// POST /api/v5/account/set-position-mode. OKX itself rejects this call when the account has any
// open position or pending order — this method does not pre-check that (the caller does, for a
// friendlier error message before ever reaching the exchange; CLAUDE.md §27.6/§49.2's "ask the
// exchange, don't just trust local state" precedent still applies to the exchange's own rejection
// being authoritative either way).
func (c *Client) SetPositionMode(posMode string) error {
	return c.do("POST", "/api/v5/account/set-position-mode", okx.SetPositionModeRequest{PosMode: posMode}, nil)
}

// GetInstrument fetches one instId's contract-shape metadata via
// GET /api/v5/public/instruments — an unauthenticated, unsigned endpoint (no OK-ACCESS-* headers
// needed for /public/*), but routed through the same signed do() as every other call for
// consistency; OKX accepts the extra signature headers on public endpoints without complaint.
func (c *Client) GetInstrument(instType, instID string) (domain.Instrument, error) {
	if instType == "" || instID == "" {
		return domain.Instrument{}, fmt.Errorf("instType and instID are required")
	}
	path := "/api/v5/public/instruments?" + url.Values{"instType": {instType}, "instId": {instID}}.Encode()
	var instruments []okx.Instrument
	if err := c.do("GET", path, nil, &instruments); err != nil {
		return domain.Instrument{}, err
	}
	if len(instruments) == 0 {
		return domain.Instrument{}, fmt.Errorf("no instrument data returned for instType=%s instId=%s", instType, instID)
	}
	return instruments[0].ToDomain(), nil
}
