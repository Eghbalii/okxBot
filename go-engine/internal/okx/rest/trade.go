package rest

import (
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

// SetLeverage sets leverage for an instrument via POST /api/v5/account/set-leverage.
func (c *Client) SetLeverage(req domain.LeverageChange) error {
	return c.do("POST", "/api/v5/account/set-leverage", okx.SetLeverageRequestFromDomain(req), nil)
}
