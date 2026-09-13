// Package gatewayclient implements port.ExchangeClient by calling cmd/okx-gateway over HTTP
// instead of talking to OKX directly (CLAUDE.md §27.1). This is the client half of the
// migration: a service swaps its rest.New(...) construction for gatewayclient.New(...) and every
// existing call site (which already only depends on the port.ExchangeClient interface) keeps
// working unchanged — the rate limiting/priority/retry logic all lives behind the gateway, not
// here.
//
// Deliberately does NOT expose GetHistoryCandles — the candle-backfill feature that endpoint
// served was removed from the codebase entirely (2026-09-01, explicit operator instruction: that
// OKX endpoint must never be called again), and neither cmd/okx-gateway nor this client proxy it.
package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// Client calls an okx-gateway instance over HTTP, identifying itself via the X-Gateway-Consumer
// header so the gateway can apply per-consumer rate limiting and priority (CLAUDE.md §27.1).
type Client struct {
	BaseURL    string
	Consumer   string // e.g. "trader" — the ONLY value the gateway treats as priority
	httpClient *http.Client
}

// New builds a gatewayclient.Client. consumer identifies the calling service to the gateway;
// pass "trader" only from cmd/trader (CLAUDE.md §27.1's "strict priority for the real trader,
// no other consumer").
func New(baseURL, consumer string) *Client {
	return &Client{
		BaseURL:  baseURL,
		Consumer: consumer,
		// A longer timeout than rest.Client's own 10s: a gateway-proxied call may itself wait on
		// the gateway's rate limiter before it even starts the underlying OKX request, on top of
		// OKX's own round trip.
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Gateway-Consumer", c.Consumer)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("gateway request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read gateway response body: %w", err)
	}
	if resp.StatusCode >= 300 {
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(respBody, &errResp)
		if errResp.Error != "" {
			return fmt.Errorf("gateway %s %s: %s", method, path, errResp.Error)
		}
		return fmt.Errorf("gateway %s %s: status %d: %s", method, path, resp.StatusCode, string(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode gateway response %s %s: %w", method, path, err)
		}
	}
	return nil
}

// Health is the ONE way cmd/trader learns whether it's trading against real or demo OKX
// credentials, now that the gateway is the only process holding them (CLAUDE.md §27.1/§27's
// mode-safety design: mode must never disagree with the credentials actually in use, and with
// credentials moved out of cmd/trader, this is the single remaining source of truth for that).
type HealthStatus struct {
	OK        bool `json:"ok"`
	Simulated bool `json:"simulated"`
}

func (c *Client) Health(ctx context.Context) (HealthStatus, error) {
	var out HealthStatus
	err := c.do(ctx, http.MethodGet, "/health", nil, &out)
	return out, err
}

func (c *Client) GetTicker(instID string) (domain.Ticker, error) {
	var out domain.Ticker
	path := "/ticker?" + url.Values{"instId": {instID}}.Encode()
	err := c.do(context.Background(), http.MethodGet, path, nil, &out)
	return out, err
}

// GetAllTickers fetches one instType's whole market in a single call, for token discovery.
func (c *Client) GetAllTickers(instType string) ([]domain.MarketTicker, error) {
	var out []domain.MarketTicker
	path := "/tickers?" + url.Values{"instType": {instType}}.Encode()
	err := c.do(context.Background(), http.MethodGet, path, nil, &out)
	return out, err
}

func (c *Client) GetPositions(instType string) ([]domain.Position, error) {
	var out []domain.Position
	path := "/positions"
	if instType != "" {
		path += "?" + url.Values{"instType": {instType}}.Encode()
	}
	err := c.do(context.Background(), http.MethodGet, path, nil, &out)
	return out, err
}

func (c *Client) GetBalance(ccy string) ([]domain.Balance, error) {
	var out []domain.Balance
	path := "/balance"
	if ccy != "" {
		path += "?" + url.Values{"ccy": {ccy}}.Encode()
	}
	err := c.do(context.Background(), http.MethodGet, path, nil, &out)
	return out, err
}

func (c *Client) GetCandles(instID, bar string, limit int) ([]domain.Candle, error) {
	var out []domain.Candle
	path := "/candles?" + url.Values{
		"instId": {instID}, "bar": {bar}, "limit": {strconv.Itoa(limit)},
	}.Encode()
	err := c.do(context.Background(), http.MethodGet, path, nil, &out)
	return out, err
}

func (c *Client) PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error) {
	var out domain.OrderResult
	if err := c.do(context.Background(), http.MethodPost, "/order", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CancelOrder(instID, ordID string) error {
	body := map[string]string{"instId": instID, "ordId": ordID}
	return c.do(context.Background(), http.MethodPost, "/order/cancel", body, nil)
}

func (c *Client) GetOrder(instID, ordID string) (domain.OrderStatus, error) {
	var out domain.OrderStatus
	path := "/order?" + url.Values{"instId": {instID}, "ordId": {ordID}}.Encode()
	err := c.do(context.Background(), http.MethodGet, path, nil, &out)
	return out, err
}

// GetOrderRaw returns OKX's own order payload untouched, for the panel's exchange-report view.
// Decoding into json.RawMessage rather than a struct is the whole point — nothing is dropped.
func (c *Client) GetOrderRaw(instID, ordID string) (json.RawMessage, error) {
	var out json.RawMessage
	path := "/order/raw?" + url.Values{"instId": {instID}, "ordId": {ordID}}.Encode()
	if err := c.do(context.Background(), http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) SetLeverage(req domain.LeverageChange) error {
	return c.do(context.Background(), http.MethodPost, "/leverage", req, nil)
}

func (c *Client) GetInstrument(instType, instID string) (domain.Instrument, error) {
	var out domain.Instrument
	path := "/instrument?" + url.Values{"instType": {instType}, "instId": {instID}}.Encode()
	err := c.do(context.Background(), http.MethodGet, path, nil, &out)
	return out, err
}

func (c *Client) GetFundingRateHistory(instID string, limit int) ([]domain.FundingRate, error) {
	var out []domain.FundingRate
	path := "/funding-rate-history?" + url.Values{
		"instId": {instID},
		"limit":  {fmt.Sprintf("%d", limit)},
	}.Encode()
	err := c.do(context.Background(), http.MethodGet, path, nil, &out)
	return out, err
}

// PlaceAlgoOrder places a resting SL/TP order through the gateway, returning OKX's algoId
// (2026-09-09: every real position's protection lives on the exchange, not only in the trading
// process's own memory).
func (c *Client) PlaceAlgoOrder(req domain.AlgoOrderRequest) (string, error) {
	var out struct {
		AlgoID string `json:"algoId"`
	}
	if err := c.do(context.Background(), http.MethodPost, "/order/algo", req, &out); err != nil {
		return "", err
	}
	return out.AlgoID, nil
}

// AmendAlgoOrder moves a resting SL/TP order's trigger price(s) through the gateway.
func (c *Client) AmendAlgoOrder(req domain.AlgoOrderAmend) error {
	return c.do(context.Background(), http.MethodPost, "/order/algo/amend", req, nil)
}

// CancelAlgoOrder removes a resting SL/TP order through the gateway.
func (c *Client) CancelAlgoOrder(instID, algoID string) error {
	body := map[string]string{"instId": instID, "algoId": algoID}
	return c.do(context.Background(), http.MethodPost, "/order/algo/cancel", body, nil)
}

// GetAlgoOrder reports a resting SL/TP order's state through the gateway.
func (c *Client) GetAlgoOrder(instID, algoID string) (domain.AlgoOrderStatus, error) {
	var out domain.AlgoOrderStatus
	path := "/order/algo?" + url.Values{"instId": {instID}, "algoId": {algoID}}.Encode()
	err := c.do(context.Background(), http.MethodGet, path, nil, &out)
	return out, err
}
