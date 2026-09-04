package main

// Minimal, standalone signed OKX v5 REST helpers for endpoints internal/okx/rest.Client does not
// (yet) cover: placing an order with attached TP/SL fields, and order-algo place/amend/cancel.
// Deliberately NOT added to internal/okx/rest itself — CLAUDE.md §27.3 currently states real
// trading watches SL/TP in-process rather than via a resting exchange-side algo order, so adding
// order-algo to the production REST client would silently expand production surface area ahead of
// that design actually changing. This file exists only for cmd/okx-apitest's diagnostic use.
//
// Signing mirrors internal/okx/rest/client.go's sign()/do() exactly (same prehash construction:
// timestamp + method + path + body, HMAC-SHA256, base64) since that method is unexported and this
// binary needs to call endpoints the exported Client doesn't wrap.

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

type rawClient struct {
	apiKey     string
	apiSecret  string
	passphrase string
	simulated  bool
	baseURL    string
}

type rawEnvelope struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func (c rawClient) do(method, path string, body any) (json.RawMessage, string, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, "", fmt.Errorf("marshal request body: %w", err)
		}
		bodyBytes = b
	}

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	prehash := timestamp + method + path + string(bodyBytes)
	sig := sign(prehash, c.apiSecret)

	req, err := http.NewRequest(method, c.baseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OK-ACCESS-KEY", c.apiKey)
	req.Header.Set("OK-ACCESS-SIGN", sig)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", c.passphrase)
	if c.simulated {
		req.Header.Set("x-simulated-trading", "1")
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response body: %w", err)
	}

	var env rawEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, string(respBody), fmt.Errorf("decode response %s %s: %w (raw: %s)", method, path, err, string(respBody))
	}
	if env.Code != "0" {
		return env.Data, string(respBody), fmt.Errorf("okx api error %s %s: code=%s msg=%s", method, path, env.Code, env.Msg)
	}
	return env.Data, string(respBody), nil
}

func sign(prehash, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(prehash))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type orderPlaceResult struct {
	OrdID string `json:"ordId"`
	SCode string `json:"sCode"`
	SMsg  string `json:"sMsg"`
}

func decodeOrderPlaceResult(data json.RawMessage) (orderPlaceResult, error) {
	var results []orderPlaceResult
	if err := json.Unmarshal(data, &results); err != nil {
		return orderPlaceResult{}, fmt.Errorf("decode order place response: %w", err)
	}
	if len(results) == 0 {
		return orderPlaceResult{}, fmt.Errorf("empty order place response")
	}
	return results[0], nil
}

// --- Way 1: attached TP/SL on the parent order (POST /api/v5/trade/order) ---

// placeAttachedOrder places an order with an attachAlgoOrds entry carrying TP/SL — OKX creates
// the linked SL/TP algo order automatically; it activates only once this parent order fills.
// Flat tpTriggerPx/slTriggerPx fields on the parent order are rejected for some instrument
// categories (found live 2026-09-04 against BTC-USD_UM_XPERP-310404: 54070 "use attachAlgoOrds"),
// so this uses the array form unconditionally rather than only for instruments known to require
// it.
func (c rawClient) placeAttachedOrder(instID, tdMode, side, posSide, ordType, szStr, pxStr, tpTrigPx, slTrigPx string) (orderPlaceResult, string, error) {
	body := map[string]any{
		"instId":  instID,
		"tdMode":  tdMode,
		"side":    side,
		"ordType": ordType,
		"sz":      szStr,
		"px":      pxStr,
		"attachAlgoOrds": []map[string]string{
			{
				"tpTriggerPx": tpTrigPx,
				"tpOrdPx":     "-1",
				"slTriggerPx": slTrigPx,
				"slOrdPx":     "-1",
			},
		},
	}
	if posSide != "" {
		body["posSide"] = posSide
	}
	data, raw, err := c.do("POST", "/api/v5/trade/order", body)
	if err != nil {
		return orderPlaceResult{}, raw, err
	}
	result, err := decodeOrderPlaceResult(data)
	return result, raw, err
}

// placePlainOrder places a plain order with no attached TP/SL.
func (c rawClient) placePlainOrder(instID, tdMode, side, posSide, ordType, szStr, pxStr string) (orderPlaceResult, string, error) {
	body := map[string]string{
		"instId":  instID,
		"tdMode":  tdMode,
		"side":    side,
		"ordType": ordType,
		"sz":      szStr,
		"px":      pxStr,
	}
	if posSide != "" {
		body["posSide"] = posSide
	}
	data, raw, err := c.do("POST", "/api/v5/trade/order", body)
	if err != nil {
		return orderPlaceResult{}, raw, err
	}
	result, err := decodeOrderPlaceResult(data)
	return result, raw, err
}

// --- Way 2: standalone algo order (POST /api/v5/trade/order-algo) for SL/TP attach + amend ---

type algoClient struct {
	client rawClient
}

// placeSLTP attaches a stop-loss to an existing (unfilled, in this test) order via
// POST /api/v5/trade/order-algo, using ordType "conditional" — OKX's SL/TP algo-order type. The
// algo order's side is the CLOSING side (opposite of the entry side), since it's the order that
// flattens the position when triggered.
func (a *algoClient) placeSLTP(instID, tdMode, entrySide, entryPosSide, sz, slTriggerPx string) (algoID string, raw string, err error) {
	body := map[string]string{
		"instId":      instID,
		"tdMode":      tdMode,
		"side":        oppositeSide(entrySide),
		"ordType":     "conditional",
		"sz":          sz,
		"slTriggerPx": slTriggerPx,
		"slOrdPx":     "-1",
	}
	if entryPosSide != "" {
		// The algo order's posSide must match the POSITION it closes, not its own (opposite) side.
		body["posSide"] = entryPosSide
	}
	data, rawBody, err := a.client.do("POST", "/api/v5/trade/order-algo", body)
	if err != nil {
		return "", rawBody, err
	}
	var results []struct {
		AlgoID string `json:"algoId"`
		SCode  string `json:"sCode"`
		SMsg   string `json:"sMsg"`
	}
	if err := json.Unmarshal(data, &results); err != nil {
		return "", rawBody, fmt.Errorf("decode order-algo response: %w", err)
	}
	if len(results) == 0 {
		return "", rawBody, fmt.Errorf("empty order-algo response")
	}
	if results[0].SCode != "0" {
		return "", rawBody, fmt.Errorf("order-algo rejected: sCode=%s sMsg=%s", results[0].SCode, results[0].SMsg)
	}
	return results[0].AlgoID, rawBody, nil
}

// amendSL updates an existing algo order's SL trigger price via POST /api/v5/trade/amend-algos.
func (a *algoClient) amendSL(instID, algoID, newSLTriggerPx string) (raw string, err error) {
	body := map[string]string{
		"instId":         instID,
		"algoId":         algoID,
		"newSlTriggerPx": newSLTriggerPx,
		"newSlOrdPx":     "-1",
	}
	_, rawBody, err := a.client.do("POST", "/api/v5/trade/amend-algos", body)
	return rawBody, err
}

// cancel cancels an algo order via POST /api/v5/trade/cancel-algos.
func (a *algoClient) cancel(instID, algoID string) (raw string, err error) {
	body := []map[string]string{{"instId": instID, "algoId": algoID}}
	_, rawBody, err := a.client.do("POST", "/api/v5/trade/cancel-algos", body)
	return rawBody, err
}

func oppositeSide(side string) string {
	if side == "buy" {
		return "sell"
	}
	return "buy"
}
