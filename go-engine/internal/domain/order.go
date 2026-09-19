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
	// Fee and Pnl are the exchange's own accounting for this order — Fee negative for a charge,
	// Pnl the realized profit/loss it booked. Preferred over any locally computed figure, which
	// cannot see fees, funding, or the true fill price (2026-09-08). Zero legitimately means both
	// "no fee/pnl" and "not reported"; callers that need to tell those apart check IsZero at the
	// point of use rather than this type carrying a pointer for it.
	Fee decimal.Decimal
	Pnl decimal.Decimal
}

// IsFilled reports whether the order is fully filled.
func (s OrderStatus) IsFilled() bool { return s.State == "filled" }

// IsTerminal reports whether the order has reached a state that will not change further —
// fully filled or canceled. "partially_filled" is NOT terminal: OKX keeps that state only while
// the remainder is still live and could still fill or later be canceled.
func (s OrderStatus) IsTerminal() bool { return s.State == "filled" || s.State == "canceled" }

// AlgoOrderRequest is a request to place a resting conditional (stop-loss / take-profit) order on
// the exchange — OKX's POST /api/v5/trade/order-algo with ordType "conditional".
//
// This is what makes a real position's protection live on the EXCHANGE rather than only in this
// process (2026-09-09 request: "we should set sl/tp on exchange always"). Before it, a real
// position's SL/TP existed solely as columns in bot_orders that BotTrader's own tick monitor
// watched — so any interruption of this service (crash, restart, deploy, network partition, the
// Kafka tick feed stalling) left real capital running with no protection whatsoever, which is
// exactly the exposure the exchange-side order removes.
//
// Side is the CLOSING side — the opposite of the entry side — since this order is what flattens
// the position when a trigger fires. PosSide, in hedge mode, is the side of the POSITION being
// closed, not this order's own side.
//
// SLTriggerPx and TPTriggerPx are both optional and both may be set on one order: OKX treats a
// conditional order carrying both as OCO (one-cancels-other), so whichever triggers first cancels
// the other automatically — one resting order protects both sides of the trade, and there is never
// a window where a filled stop leaves a stale target behind.
type AlgoOrderRequest struct {
	InstID  string
	TdMode  string // "cross" or "isolated"
	Side    string // the CLOSING side: opposite of the position's entry side
	PosSide string // the POSITION's side ("long"/"short") in hedge mode; empty in net mode
	Sz      decimal.Decimal
	// SLTriggerPx/TPTriggerPx are trigger prices. A zero value means "this side is not set" and is
	// omitted from the request, so one order can carry a stop only, a target only, or both.
	SLTriggerPx decimal.Decimal
	TPTriggerPx decimal.Decimal
}

// AlgoOrderAmend updates an already-resting conditional order's trigger prices in place — OKX's
// POST /api/v5/trade/amend-algos. Used for both the model's in-trade SL/TP adjustments and the
// operator's manual panel edits, so a level changed anywhere is changed on the exchange too rather
// than only in this system's own bookkeeping.
//
// A zero trigger price means "leave this side alone", so a stop-only move does not disturb the
// target sharing the same order.
type AlgoOrderAmend struct {
	InstID      string
	AlgoID      string
	SLTriggerPx decimal.Decimal
	TPTriggerPx decimal.Decimal
}

// AlgoOrderStatus is a resting conditional order's current state on the exchange — the
// verification half of "the exchange holds the protection" (2026-09-09): a successful placement is
// not assumed to stay valid forever, it is re-checked, and a protective order that has vanished is
// re-placed rather than silently trusted.
//
// State "" means OKX reported no such order at all — already triggered and filled, or canceled.
// That is a legitimate answer, not an error, and is deliberately distinguishable from a failed
// request so a caller never treats "the stop is gone" as "the network hiccuped".
type AlgoOrderStatus struct {
	AlgoID      string
	InstID      string
	State       string // "live", "effective" (triggered), "canceled", "order_failed", or "" when absent
	SLTriggerPx decimal.Decimal
	TPTriggerPx decimal.Decimal
	// ActualSide is which half of an OCO order actually fired: "sl" or "tp" (empty until it does).
	// This is the exchange telling us WHY the position closed, and without it a stop-loss that
	// OKX executed was being recorded as an operator's manual close (2026-09-09).
	ActualSide string
	// OrdID is the ordinary order the trigger created to flatten the position. It carries the real
	// fill price, realized PnL and fee — the close data that is otherwise missing entirely, since
	// the position was closed by the exchange and never passed through this system's own flatten.
	OrdID string
}

// TriggeredReason maps a fired conditional order onto this system's own close_reason vocabulary,
// returning ok=false when the order has not fired. "sl"/"tp" match the reasons the in-process
// monitor already records for the same events, so a stop is recorded identically whether the
// exchange executed it or this process did.
func (s AlgoOrderStatus) TriggeredReason() (string, bool) {
	if s.State != "effective" {
		return "", false
	}
	switch s.ActualSide {
	case "sl":
		return "sl", true
	case "tp":
		return "tp", true
	default:
		// Fired, but OKX did not say which side. Reporting a guess would be worse than reporting
		// nothing: the caller falls back to its own close reason rather than inventing one.
		return "", false
	}
}

// IsLive reports whether the conditional order is still resting on the exchange and will fire if
// its trigger is reached. Anything else — triggered, canceled, failed, or absent — means the
// position it protected is no longer protected by it.
func (s AlgoOrderStatus) IsLive() bool { return s.State == "live" }
