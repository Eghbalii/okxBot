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

// OrderStatus is the exchange's authoritative, current lifecycle state for one order (CLAUDE.md
// §27.5/§27.6 — real trading must confirm fill state against OKX's own order-status response
// rather than trusting the placement acceptance alone or this system's own bookkeeping). State
// mirrors OKX's `state` field on GET /api/v5/trade/order.
type OrderStatus struct {
	InstID    string
	OrdID     string
	ClOrdID   string
	State     string // "live", "partially_filled", "filled", "canceled"
	AvgPx     decimal.Decimal
	AccFillSz decimal.Decimal // cumulative filled size so far
	Sz        decimal.Decimal // requested size
}

// IsFilled reports whether the order is fully filled.
func (s OrderStatus) IsFilled() bool { return s.State == "filled" }

// IsTerminal reports whether the order has reached a state that will not change further —
// fully filled or canceled. "partially_filled" is NOT terminal: OKX keeps that state only while
// the remainder is still live and could still fill or later be canceled.
func (s OrderStatus) IsTerminal() bool { return s.State == "filled" || s.State == "canceled" }
