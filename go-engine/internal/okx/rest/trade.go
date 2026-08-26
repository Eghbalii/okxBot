package rest

import (
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

// SetLeverage sets leverage for an instrument via POST /api/v5/account/set-leverage.
func (c *Client) SetLeverage(req domain.LeverageChange) error {
	return c.do("POST", "/api/v5/account/set-leverage", okx.SetLeverageRequestFromDomain(req), nil)
}
