package okx

import "github.com/shopspring/decimal"

// Ticker is a normalized OKX v5 "tickers" channel / REST market ticker payload.
type Ticker struct {
	InstID    string          `json:"instId"`
	Last      decimal.Decimal `json:"last"`
	AskPx     decimal.Decimal `json:"askPx"`
	BidPx     decimal.Decimal `json:"bidPx"`
	Open24h   decimal.Decimal `json:"open24h"`
	High24h   decimal.Decimal `json:"high24h"`
	Low24h    decimal.Decimal `json:"low24h"`
	Vol24h    decimal.Decimal `json:"vol24h"`
	Timestamp string          `json:"ts"`
}

// Position mirrors OKX's /api/v5/account/positions entry (fields we care about).
type Position struct {
	InstID      string          `json:"instId"`
	PosSide     string          `json:"posSide"`
	Pos         decimal.Decimal `json:"pos"`
	AvgPx       decimal.Decimal `json:"avgPx"`
	Lever       decimal.Decimal `json:"lever"`
	Upl         decimal.Decimal `json:"upl"`
	UplRatio    decimal.Decimal `json:"uplRatio"`
	LiqPx       decimal.Decimal `json:"liqPx"`
	MarkPx      decimal.Decimal `json:"markPx"`
	NotionalUsd decimal.Decimal `json:"notionalUsd"`
	MgnMode     string          `json:"mgnMode"`
}

// Balance mirrors a single currency entry from /api/v5/account/balance.
type Balance struct {
	Ccy     string          `json:"ccy"`
	Eq      decimal.Decimal `json:"eq"`
	AvailEq decimal.Decimal `json:"availEq"`
}

// OrderRequest is the payload for POST /api/v5/trade/order.
type OrderRequest struct {
	InstID  string          `json:"instId"`
	TdMode  string          `json:"tdMode"`            // "cross" or "isolated"
	Side    string          `json:"side"`              // "buy" or "sell"
	PosSide string          `json:"posSide,omitempty"` // "long" or "short" (hedge mode)
	OrdType string          `json:"ordType"`           // "market", "limit", ...
	Sz      decimal.Decimal `json:"sz"`                // size in contracts
	Px      decimal.Decimal `json:"px,omitempty"`      // required for limit orders
}

// OrderResult mirrors a single entry of the /api/v5/trade/order response data array.
type OrderResult struct {
	OrdID   string `json:"ordId"`
	ClOrdID string `json:"clOrdId"`
	SCode   string `json:"sCode"`
	SMsg    string `json:"sMsg"`
}

// SetLeverageRequest is the payload for POST /api/v5/account/set-leverage.
type SetLeverageRequest struct {
	InstID  string          `json:"instId"`
	Lever   decimal.Decimal `json:"lever"`
	MgnMode string          `json:"mgnMode"`           // "cross" or "isolated"
	PosSide string          `json:"posSide,omitempty"` // required in hedge mode
}
