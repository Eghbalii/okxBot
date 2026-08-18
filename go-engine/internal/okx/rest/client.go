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

// Client is a signed OKX v5 REST API client.
type Client struct {
	BaseURL    string
	APIKey     string
	APISecret  string
	Passphrase string
	Simulated  bool

	httpClient *http.Client
}

// New creates an OKX REST client. baseURL is typically "https://www.okx.com".
func New(baseURL, apiKey, apiSecret, passphrase string, simulated bool) *Client {
	return &Client{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		APISecret:  apiSecret,
		Passphrase: passphrase,
		Simulated:  simulated,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// envelope mirrors OKX's standard REST response wrapper.
type envelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// do performs a signed request and decodes the "data" field of the response into out.
func (c *Client) do(method, path string, body any, out any) error {
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
