package rest

import (
	"fmt"
	"net/url"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
)

// algoOrderResult mirrors one entry of the order-algo / amend-algos / cancel-algos response data
// array. OKX reports per-order success in sCode INSIDE a 200 response, so a call that "succeeded"
// at the HTTP level can still have been rejected — every method below checks it.
type algoOrderResult struct {
	AlgoID string `json:"algoId"`
	SCode  string `json:"sCode"`
	SMsg   string `json:"sMsg"`
}

// PlaceAlgoOrder places a resting conditional stop-loss/take-profit order via
// POST /api/v5/trade/order-algo, returning OKX's algoId for later amend/cancel.
//
// Wire shape verified against the real account by cmd/okx-apitest (CLAUDE.md §33) before being
// promoted here — that throwaway diagnostic deliberately held this call outside the production
// client until it was actually needed, which is now.
//
// slOrdPx/tpOrdPx are "-1", OKX's sentinel for "execute at market when triggered". A limit price
// would be the alternative, and is the wrong choice for a protective stop: it can fail to fill in
// exactly the fast move that triggered it, leaving the position open while price runs away.
//
// Setting both trigger prices makes this an OCO order — whichever side fires cancels the other, so
// a filled stop can never leave a stale target resting behind it.
func (c *Client) PlaceAlgoOrder(req domain.AlgoOrderRequest) (string, error) {
	body := map[string]string{
		"instId":  req.InstID,
		"tdMode":  req.TdMode,
		"side":    req.Side,
		"ordType": "conditional",
		"sz":      req.Sz.String(),
	}
	if req.PosSide != "" {
		body["posSide"] = req.PosSide
	}
	// triggerPxType is sent explicitly rather than left to OKX's default. It is REQUIRED whenever a
	// take-profit side is present — omitting it is rejected with "Parameter newTpTriggerPxType
	// error" (2026-09-10, found on the amend path, where it made every SL/TP edit fail and, on the
	// place path, silently cost every position its take-profit). "last" matches how this system
	// evaluates its own levels: the in-process monitor compares against the last traded price, so
	// the exchange and the backup agree on what counts as a touch. Mark price would have them
	// disagree at exactly the moment it matters.
	if req.SLTriggerPx.IsPositive() {
		body["slTriggerPx"] = req.SLTriggerPx.String()
		body["slOrdPx"] = "-1"
		body["slTriggerPxType"] = "last"
	}
	if req.TPTriggerPx.IsPositive() {
		body["tpTriggerPx"] = req.TPTriggerPx.String()
		body["tpOrdPx"] = "-1"
		body["tpTriggerPxType"] = "last"
	}
	if _, ok := body["slTriggerPx"]; !ok {
		if _, ok := body["tpTriggerPx"]; !ok {
			return "", fmt.Errorf("algo order needs at least one of slTriggerPx/tpTriggerPx")
		}
	}

	var results []algoOrderResult
	if err := c.do("POST", "/api/v5/trade/order-algo", body, &results); err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "", fmt.Errorf("empty order-algo response")
	}
	if results[0].SCode != "0" {
		return "", fmt.Errorf("order-algo rejected: sCode=%s sMsg=%s", results[0].SCode, results[0].SMsg)
	}
	if results[0].AlgoID == "" {
		return "", fmt.Errorf("order-algo returned no algoId")
	}
	return results[0].AlgoID, nil
}

// AmendAlgoOrder moves a resting conditional order's trigger price(s) via
// POST /api/v5/trade/amend-algos — the exchange-side half of an SL/TP adjustment, so a level moved
// by the model or by an operator moves on OKX too rather than only in this system's bookkeeping.
//
// A zero trigger price is omitted, leaving that side untouched: a stop-only move must not disturb
// the target resting in the same order.
func (c *Client) AmendAlgoOrder(req domain.AlgoOrderAmend) error {
	body := map[string]string{
		"instId": req.InstID,
		"algoId": req.AlgoID,
	}
	// See PlaceAlgoOrder above for why the trigger type is explicit — this is the call that
	// surfaced the requirement.
	if req.SLTriggerPx.IsPositive() {
		body["newSlTriggerPx"] = req.SLTriggerPx.String()
		body["newSlOrdPx"] = "-1"
		body["newSlTriggerPxType"] = "last"
	}
	if req.TPTriggerPx.IsPositive() {
		body["newTpTriggerPx"] = req.TPTriggerPx.String()
		body["newTpOrdPx"] = "-1"
		body["newTpTriggerPxType"] = "last"
	}
	if _, hasSL := body["newSlTriggerPx"]; !hasSL {
		if _, hasTP := body["newTpTriggerPx"]; !hasTP {
			return fmt.Errorf("algo amend needs at least one of slTriggerPx/tpTriggerPx")
		}
	}

	var results []algoOrderResult
	if err := c.do("POST", "/api/v5/trade/amend-algos", body, &results); err != nil {
		return err
	}
	// amend-algos reports per-order failure in sCode inside a 200 the same way order-algo does, so
	// an unchecked call here would silently leave the exchange holding the OLD trigger price while
	// this system recorded the new one — the exact divergence between local and exchange state that
	// placing these orders exists to eliminate.
	if len(results) > 0 && results[0].SCode != "0" {
		return fmt.Errorf("amend-algos rejected: sCode=%s sMsg=%s", results[0].SCode, results[0].SMsg)
	}
	return nil
}

// CancelAlgoOrder cancels a resting conditional order via POST /api/v5/trade/cancel-algos — called
// when the position it protects is closed by any other route (model early close, manual close,
// timeout), so a flattened position never leaves a live protective order behind that could later
// open a NEW position in the opposite direction.
func (c *Client) CancelAlgoOrder(instID, algoID string) error {
	body := []map[string]string{{"instId": instID, "algoId": algoID}}
	var results []algoOrderResult
	if err := c.do("POST", "/api/v5/trade/cancel-algos", body, &results); err != nil {
		return err
	}
	if len(results) > 0 && results[0].SCode != "0" {
		return fmt.Errorf("cancel-algos rejected: sCode=%s sMsg=%s", results[0].SCode, results[0].SMsg)
	}
	return nil
}

// GetAlgoOrder fetches one resting conditional order's current state via
// GET /api/v5/trade/order-algo — the verification half of "exchange is primary" (2026-09-09
// decision): the protective order is trusted to do its job, and this is what confirms it is still
// actually there rather than assuming a successful placement stays valid forever.
//
// Returns state "" with no error when OKX reports no such order at all, which is a real and
// expected answer (an order that already triggered and filled, or was canceled) — distinguishing
// "gone" from "the request failed" is the entire point of this call, so a missing order must not
// surface as an error the caller would treat as a transient network problem.
func (c *Client) GetAlgoOrder(instID, algoID string) (domain.AlgoOrderStatus, error) {
	path := "/api/v5/trade/order-algo?" + url.Values{"algoId": {algoID}}.Encode()
	var results []okx.AlgoOrderStatus
	if err := c.do("GET", path, nil, &results); err != nil {
		return domain.AlgoOrderStatus{}, err
	}
	if len(results) == 0 {
		return domain.AlgoOrderStatus{AlgoID: algoID, InstID: instID}, nil
	}
	return results[0].ToDomain(), nil
}
