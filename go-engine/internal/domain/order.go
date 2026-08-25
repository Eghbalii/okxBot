package domain

import "github.com/shopspring/decimal"

// OrderRequest is a request to place an order on an instrument.
type OrderRequest struct {
	InstID  string
	TdMode  string // "cross" or "isolated"
	Side    string // "buy" or "sell"
	PosSide string // "long" or "short" (hedge mode); empty in net mode
	OrdType string // "market", "limit", ...
	Sz      decimal.Decimal
	Px      decimal.Decimal // required for limit orders
}

// OrderResult is the exchange's response to a placed order.
type OrderResult struct {
	OrdID   string
	ClOrdID string
	SCode   string
	SMsg    string
}

// LeverageChange is a request to set leverage for an instrument.
type LeverageChange struct {
	InstID  string
	Lever   decimal.Decimal
	MgnMode string // "cross" or "isolated"
	PosSide string // required in hedge mode
}
