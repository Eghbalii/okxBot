// Package rest is a signed MEXC futures REST client covering trade, account and market endpoints.
//
// Deliberately a sibling of internal/okx/rest rather than a shared "generic REST client" the two
// exchanges configure. They agree on almost nothing below the method names: MEXC signs
// key+timestamp+params with no passphrase, OKX signs a timestamp+method+path+body prehash WITH one;
// MEXC wraps responses in {success, code, data} and OKX in {code, msg, data} where code is a
// STRING; MEXC returns klines as parallel column arrays and OKX as rows. A shared client would be
// a pile of per-exchange branches pretending to be one thing — the adapter boundary (CLAUDE.md §10)
// exists precisely so each exchange can keep its own wire vocabulary and converge only at domain.
package rest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// maxConcurrentRequests bounds in-flight calls from one Client.
//
// Set to the same conservative 3 as the OKX client, for a different reason: OKX's number was tuned
// against a specific reproducible failure (CLAUDE.md §14's 51001 burst), whereas MEXC's exact
// concurrent-burst behaviour has NOT been characterised here. 3 is a deliberate carry-over of a
// known-safe order of magnitude rather than a measured limit for this exchange — raise it only
// against observed behaviour, not by assuming MEXC is more permissive.
const maxConcurrentRequests = 3

// Client is a signed MEXC futures REST API client.
type Client struct {
	BaseURL   string
	APIKey    string
	APISecret string

	httpClient *http.Client
	sem        chan struct{}
}

// New creates a MEXC futures REST client. baseURL is typically "https://contract.mexc.com".
//
// Note this is contract.mexc.com, not api.mexc.com: the latter serves MEXC's SPOT API and answers
// futures paths with a 404 rather than an error naming the problem, which reads as a bug in this
// client rather than a wrong host.
func New(baseURL, apiKey, apiSecret string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 20
	// Keep-alives are left ENABLED here, unlike the OKX client which disables them. That was a
	// workaround for a specific, live-verified okx.com HTTP/2 multiplexing failure (§14); carrying
	// it over would impose a real per-request connection cost on MEXC to solve a problem this host
	// has not been shown to have. If MEXC ever exhibits the same signature, revisit with evidence.
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		APISecret:  apiSecret,
		httpClient: &http.Client{Timeout: 10 * time.Second, Transport: transport},
		sem:        make(chan struct{}, maxConcurrentRequests),
	}
}

// envelope mirrors MEXC's standard futures response wrapper.
//
// `code` is a NUMBER here (0 = success), where OKX's is a string ("0"). `success` is redundant with
// code == 0 but is what MEXC's own docs treat as authoritative, so both are checked: a response
// that disagrees with itself is exactly the case worth failing loudly on rather than picking a
// winner.
type envelope struct {
	Success bool            `json:"success"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// sign builds MEXC's futures signature: HMAC-SHA256(secret, apiKey + timestamp + paramString),
// hex-encoded.
//
// paramString is the sorted query string for GET/DELETE, or the raw JSON body for POST — MEXC signs
// the request's parameters in the form they are actually sent, so the caller passes exactly what
// goes on the wire. Getting this wrong does not produce a clear "bad signature" from every
// endpoint, so the construction is kept in one place and tested against MEXC's own published
// example rather than reimplemented per call site.
func (c *Client) sign(timestamp, paramString string) string {
	mac := hmac.New(sha256.New, []byte(c.APISecret))
	mac.Write([]byte(c.APIKey + timestamp + paramString))
	return hex.EncodeToString(mac.Sum(nil))
}

// sortedQuery renders params as MEXC's signing string: key=value pairs joined by &, sorted by key.
// Sorting is required — MEXC signs the sorted form, so map iteration order (which Go randomises)
// would otherwise produce a signature that fails intermittently rather than never, which is far
// harder to diagnose.
func sortedQuery(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, "&")
}

// doPublic performs an UNSIGNED request against a public market-data endpoint.
//
// Kept separate from do() rather than signing everything: MEXC's public endpoints work with no
// credentials at all, and this project needs market data long before it needs API keys — the
// operator's own rollout order (WebSocket and public data first, credentials later). Signing here
// would make every public call fail without keys for no reason.
func (c *Client) doPublic(path string, params map[string]string, out any) error {
	c.sem <- struct{}{}
	defer func() { <-c.sem }()

	endpoint := c.BaseURL + path
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		endpoint += "?" + q.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	return c.execute(req, out)
}

// do performs a SIGNED request and decodes the "data" field into out.
func (c *Client) do(method, path string, params map[string]string, body any, out any) error {
	c.sem <- struct{}{}
	defer func() { <-c.sem }()

	timestamp := fmt.Sprintf("%d", time.Now().UnixMilli())
	endpoint := c.BaseURL + path

	var (
		bodyReader  io.Reader
		paramString string
	)
	switch method {
	case http.MethodGet, http.MethodDelete:
		paramString = sortedQuery(params)
		if paramString != "" {
			endpoint += "?" + paramString
		}
	default:
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("marshal request body: %w", err)
			}
			paramString = string(b)
			bodyReader = bytes.NewReader(b)
		}
	}

	req, err := http.NewRequest(method, endpoint, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("ApiKey", c.APIKey)
	req.Header.Set("Request-Time", timestamp)
	req.Header.Set("Signature", c.sign(timestamp, paramString))
	req.Header.Set("Content-Type", "application/json")

	return c.execute(req, out)
}

// execute sends req and unwraps MEXC's response envelope into out.
func (c *Client) execute(req *http.Request, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// A non-JSON body almost always means the wrong host or path (api.mexc.com instead of
		// contract.mexc.com serves an HTML 404), so the status and a snippet are included rather
		// than just "invalid JSON" — the snippet is what actually identifies the mistake.
		return fmt.Errorf("decode response (status %d): %w: %s", resp.StatusCode, err, snippet(raw))
	}

	if !env.Success || env.Code != 0 {
		return &APIError{Code: env.Code, Message: env.Message, Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("decode data: %w", err)
	}
	return nil
}

// snippet bounds an error message's embedded response body so a large HTML error page cannot
// flood the logs.
func snippet(b []byte) string {
	const max = 180
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// APIError is a structured MEXC error, carrying the code rather than only a formatted string.
//
// The code is what callers need: the gateway's retry predicate must distinguish a rate-limit from a
// rejected order, and CLAUDE.md §38.1 records what string-matching an error costs — OKX's per-item
// failures were reported as an empty top-level message for two debugging rounds because the
// structured detail was discarded.
type APIError struct {
	Code    int
	Message string
	Status  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("mexc api error: code=%d msg=%q (http %d)", e.Code, e.Message, e.Status)
}
