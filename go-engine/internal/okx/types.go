package okx

import (
	"bytes"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// decimalOrZero decodes an OKX-returned numeric field the same way decimal.Decimal does, except
// that a blank string ("") or JSON null decodes as zero instead of erroring. Found live
// 2026-09-04 (cmd/okx-apitest's diagnostic): GET /api/v5/trade/order returns avgPx/accFillSz as
// "" — not "0" — for an order that hasn't started filling yet, which decimal.Decimal's own
// UnmarshalJSON rejects outright. RealTrader.waitForFill's polling loop already tolerates a
// GetOrder error by retrying (it never surfaced as a user-visible bug), but every retry logged a
// spurious warning and wasted a poll cycle for the entirely normal case of an order still being
// registered.
type decimalOrZero struct {
	decimal.Decimal
}

func (d *decimalOrZero) UnmarshalJSON(data []byte) error {
	trimmed := bytes.Trim(data, `"`)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		d.Decimal = decimal.Zero
		return nil
	}
	return d.Decimal.UnmarshalJSON(data)
}

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

// ToDomain converts a Ticker to its domain representation.
func (t Ticker) ToDomain() domain.Ticker {
	return domain.Ticker{
		InstID: t.InstID, Last: t.Last, AskPx: t.AskPx, BidPx: t.BidPx,
		Open24h: t.Open24h, High24h: t.High24h, Low24h: t.Low24h, Vol24h: t.Vol24h,
	}
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

// ToDomain converts a Position to its domain representation.
func (p Position) ToDomain() domain.Position {
	return domain.Position{
		InstID: p.InstID, PosSide: p.PosSide, Pos: p.Pos, AvgPx: p.AvgPx, Lever: p.Lever,
		Upl: p.Upl, UplRatio: p.UplRatio, LiqPx: p.LiqPx, MarkPx: p.MarkPx,
		NotionalUsd: p.NotionalUsd, MgnMode: p.MgnMode,
	}
}

// Balance mirrors a single currency entry from /api/v5/account/balance.
type Balance struct {
	Ccy     string          `json:"ccy"`
	Eq      decimal.Decimal `json:"eq"`
	AvailEq decimal.Decimal `json:"availEq"`
}

// ToDomain converts a Balance to its domain representation.
func (b Balance) ToDomain() domain.Balance {
	return domain.Balance{Ccy: b.Ccy, Eq: b.Eq, AvailEq: b.AvailEq}
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

// OrderRequestFromDomain converts a domain.OrderRequest to the OKX wire payload.
func OrderRequestFromDomain(req domain.OrderRequest) OrderRequest {
	return OrderRequest{
		InstID: req.InstID, TdMode: req.TdMode, Side: req.Side, PosSide: req.PosSide,
		OrdType: req.OrdType, Sz: req.Sz, Px: req.Px,
	}
}

// OrderResult mirrors a single entry of the /api/v5/trade/order response data array.
type OrderResult struct {
	OrdID   string `json:"ordId"`
	ClOrdID string `json:"clOrdId"`
	SCode   string `json:"sCode"`
	SMsg    string `json:"sMsg"`
}

// ToDomain converts an OrderResult to its domain representation.
func (r OrderResult) ToDomain() domain.OrderResult {
	return domain.OrderResult{OrdID: r.OrdID, ClOrdID: r.ClOrdID, SCode: r.SCode, SMsg: r.SMsg}
}

// OrderStatus mirrors a single entry of the GET /api/v5/trade/order response data array.
// AvgPx/AccFillSz/Sz use decimalOrZero, not decimal.Decimal directly: OKX returns these as ""
// (not "0") for an order that hasn't started filling yet (found live 2026-09-04), which
// decimal.Decimal's own UnmarshalJSON rejects.
type OrderStatus struct {
	InstID    string        `json:"instId"`
	OrdID     string        `json:"ordId"`
	ClOrdID   string        `json:"clOrdId"`
	State     string        `json:"state"` // "live", "partially_filled", "filled", "canceled"
	AvgPx     decimalOrZero `json:"avgPx"`
	AccFillSz decimalOrZero `json:"accFillSz"`
	Sz        decimalOrZero `json:"sz"`
}

// ToDomain converts an OrderStatus to its domain representation.
func (s OrderStatus) ToDomain() domain.OrderStatus {
	return domain.OrderStatus{
		InstID: s.InstID, OrdID: s.OrdID, ClOrdID: s.ClOrdID, State: s.State,
		AvgPx: s.AvgPx.Decimal, AccFillSz: s.AccFillSz.Decimal, Sz: s.Sz.Decimal,
	}
}

// SetLeverageRequest is the payload for POST /api/v5/account/set-leverage.
type SetLeverageRequest struct {
	InstID  string          `json:"instId"`
	Lever   decimal.Decimal `json:"lever"`
	MgnMode string          `json:"mgnMode"`           // "cross" or "isolated"
	PosSide string          `json:"posSide,omitempty"` // required in hedge mode
}

// SetLeverageRequestFromDomain converts a domain.LeverageChange to the OKX wire payload.
func SetLeverageRequestFromDomain(req domain.LeverageChange) SetLeverageRequest {
	return SetLeverageRequest{InstID: req.InstID, Lever: req.Lever, MgnMode: req.MgnMode, PosSide: req.PosSide}
}
