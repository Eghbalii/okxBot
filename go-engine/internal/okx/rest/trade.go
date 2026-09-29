package rest

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
)

// PlaceOrder submits an order via POST /api/v5/trade/order.
func (c *Client) PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error) {
	wireReq := okx.OrderRequestFromDomain(req)
	var results []okx.OrderResult
	if err := c.do("POST", "/api/v5/trade/order", wireReq, &results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	result := results[0].ToDomain()
	return &result, nil
}

// CancelOrder cancels an order via POST /api/v5/trade/cancel-order.
func (c *Client) CancelOrder(instID, ordID string) error {
	body := map[string]string{"instId": instID, "ordId": ordID}
	return c.do("POST", "/api/v5/trade/cancel-order", body, nil)
}

// ClosePosition flattens whatever this instrument's position currently is via POST
// /api/v5/trade/close-position — see domain.ClosePositionRequest's doc for why this, not a
// manually-sized PlaceOrder, is used for every real-money close (2026-09-29).
func (c *Client) ClosePosition(req domain.ClosePositionRequest) (*domain.ClosePositionResult, error) {
	wireReq := okx.ClosePositionRequestFromDomain(req)
	var results []okx.ClosePositionResultWire
	if err := c.do("POST", "/api/v5/trade/close-position", wireReq, &results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	result := results[0].ToDomain()
	return &result, nil
}

// GetOrder fetches one order's current lifecycle state via GET /api/v5/trade/order — the
// authoritative fill-status check CLAUDE.md §27.5/§27.6 requires: PlaceOrder's own response is
// only OKX's acceptance of the request, not confirmation of what happened to it afterward.
func (c *Client) GetOrder(instID, ordID string) (domain.OrderStatus, error) {
	path := "/api/v5/trade/order?" + url.Values{"instId": {instID}, "ordId": {ordID}}.Encode()
	var results []okx.OrderStatus
	if err := c.do("GET", path, nil, &results); err != nil {
		return domain.OrderStatus{}, err
	}
	if len(results) == 0 {
		return domain.OrderStatus{}, fmt.Errorf("no order status returned for instId=%s ordId=%s", instID, ordID)
	}
	return results[0].ToDomain(), nil
}

// GetOrderRaw returns OKX's own order-status payload untouched — every field it sends, not the
// handful domain.OrderStatus models (2026-09-09 request: "read the closed position's details from
// the exchange and show me the whole JSON, not a few parameters I picked").
//
// Deliberately separate from GetOrder rather than replacing it: the trading loop wants a typed,
// validated shape it can branch on, and widening that struct every time OKX adds a field is
// exactly the churn this avoids. This path is for display and post-hoc audit, where the useful
// property is that nothing was dropped on the way through.
func (c *Client) GetOrderRaw(instID, ordID string) (json.RawMessage, error) {
	path := "/api/v5/trade/order?" + url.Values{"instId": {instID}, "ordId": {ordID}}.Encode()
	var results []json.RawMessage
	if err := c.do("GET", path, nil, &results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no order returned for instId=%s ordId=%s", instID, ordID)
	}
	return results[0], nil
}

// SetLeverage sets leverage for an instrument via POST /api/v5/account/set-leverage.
func (c *Client) SetLeverage(req domain.LeverageChange) error {
	return c.do("POST", "/api/v5/account/set-leverage", okx.SetLeverageRequestFromDomain(req), nil)
}
