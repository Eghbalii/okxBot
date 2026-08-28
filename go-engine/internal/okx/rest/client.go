// Package rest is a signed OKX v5 REST client covering trade, account and market endpoints.
package rest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxConcurrentRequests bounds how many OKX REST calls this Client has in flight at once. One
// Client is shared across every PaperTrader engine (one per configured instrument, CLAUDE.md
// §15.2 Phase B) — at 10 instruments, startup candle-seeding fires ~30 requests within
// milliseconds of each other. Live-verified: a burst around 4+ truly concurrent requests to
// /market/candles reliably produces exactly one failure reported as "instrument doesn't exist"
// (code 51001) on an unpredictable instrument each run, at the same latency as a real successful
// call — i.e. a genuine round trip to OKX, not a client-side bug. Sequential requests (including
// over a reused connection) and small concurrent bursts via curl never reproduced it, so the
// limiter is set conservatively below where that started happening rather than at some derived
// "safe" number — OKX's exact limiting behavior on this endpoint isn't otherwise documented.
const maxConcurrentRequests = 3

// Client is a signed OKX v5 REST API client.
type Client struct {
	BaseURL    string
	APIKey     string
	APISecret  string
	Passphrase string
	Simulated  bool

	httpClient *http.Client
	sem        chan struct{}
}

// New creates an OKX REST client. baseURL is typically "https://www.okx.com".
func New(baseURL, apiKey, apiSecret, passphrase string, simulated bool) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 20
	return &Client{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		APISecret:  apiSecret,
		Passphrase: passphrase,
		Simulated:  simulated,
		httpClient: &http.Client{Timeout: 10 * time.Second, Transport: transport},
		sem:        make(chan struct{}, maxConcurrentRequests),
	}
}

// envelope mirrors OKX's standard REST response wrapper.
type envelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// do performs a signed request and decodes the "data" field of the response into out. Blocks
// until fewer than maxConcurrentRequests calls are already in flight on this Client.
func (c *Client) do(method, path string, body any, out any) error {
	c.sem <- struct{}{}
	defer func() { <-c.sem }()

	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyBytes = b
	}

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	prehash := timestamp + method + path + string(bodyBytes)
	sig := sign(prehash, c.APISecret)

	req, err := http.NewRequest(method, c.BaseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OK-ACCESS-KEY", c.APIKey)
	req.Header.Set("OK-ACCESS-SIGN", sig)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", c.Passphrase)
	if c.Simulated {
		req.Header.Set("x-simulated-trading", "1")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	var env envelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("decode response %s %s: %w (raw: %s)", method, path, err, string(respBody))
	}
	if env.Code != "0" {
		return fmt.Errorf("okx api error %s %s: code=%s msg=%s", method, path, env.Code, env.Msg)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decode data field %s %s: %w", method, path, err)
		}
	}
	return nil
}

func sign(prehash, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(prehash))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
