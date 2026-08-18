package rest

import "github.com/rez/okxBot/go-engine/internal/okx"

// PlaceOrder submits an order via POST /api/v5/trade/order.
func (c *Client) PlaceOrder(req okx.OrderRequest) (*okx.OrderResult, error) {
	var results []okx.OrderResult
	if err := c.do("POST", "/api/v5/trade/order", req, &results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return &results[0], nil
}

// CancelOrder cancels an order via POST /api/v5/trade/cancel-order.
func (c *Client) CancelOrder(instID, ordID string) error {
	body := map[string]string{"instId": instID, "ordId": ordID}
	return c.do("POST", "/api/v5/trade/cancel-order", body, nil)
}

// SetLeverage sets leverage for an instrument via POST /api/v5/account/set-leverage.
func (c *Client) SetLeverage(req okx.SetLeverageRequest) error {
	return c.do("POST", "/api/v5/account/set-leverage", req, nil)
}
