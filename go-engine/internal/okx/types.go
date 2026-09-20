package okx

import (
	"bytes"
	"fmt"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// decimalOrZero decodes an OKX-returned numeric field the same way decimal.Decimal does, except
// that a blank string ("") or JSON null decodes as zero instead of erroring. Found live
// 2026-09-04 (cmd/okx-apitest's diagnostic): GET /api/v5/trade/order returns avgPx/accFillSz as
// "" — not "0" — for an order that hasn't started filling yet, which decimal.Decimal's own
// UnmarshalJSON rejects outright. BotTrader.waitForFill's polling loop already tolerates a
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

// FundingRate is GET /public/funding-rate-history's wire shape (2026-09-06), live-verified against
// a real response for BTC-USD_UM_XPERP.
type FundingRate struct {
	InstID      string          `json:"instId"`
	FundingRate decimal.Decimal `json:"fundingRate"`
	FundingTime string          `json:"fundingTime"` // ms-epoch string, same convention as candle.Ts
}

// ToDomain converts a FundingRate to its domain representation.
func (f FundingRate) ToDomain() (domain.FundingRate, error) {
	ms, err := strconv.ParseInt(f.FundingTime, 10, 64)
	if err != nil {
		return domain.FundingRate{}, fmt.Errorf("parse funding rate time for %s: %w", f.InstID, err)
	}
	return domain.FundingRate{
		InstID:      f.InstID,
		FundingTime: time.UnixMilli(ms).UTC(),
		FundingRate: f.FundingRate,
	}, nil
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
	// Fee and Pnl are OKX's OWN accounting for this order (2026-09-08): Fee is negative for a
	// charge, Pnl is the realized profit/loss the order booked. Read from the order rather than
	// from /account/positions because a fully-closed position disappears from that endpoint
	// immediately, while the order that closed it remains queryable. Same decimalOrZero treatment
	// as the fields above — OKX returns "" rather than "0" before an order has filled.
	Fee decimalOrZero `json:"fee"`
	Pnl decimalOrZero `json:"pnl"`
}

// ToDomain converts an OrderStatus to its domain representation.
func (s OrderStatus) ToDomain() domain.OrderStatus {
	return domain.OrderStatus{
		InstID: s.InstID, OrdID: s.OrdID, ClOrdID: s.ClOrdID, State: s.State,
		AvgPx: s.AvgPx.Decimal, AccFillSz: s.AccFillSz.Decimal, Sz: s.Sz.Decimal,
		Fee: s.Fee.Decimal, Pnl: s.Pnl.Decimal,
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

// AccountConfig mirrors one entry of GET /api/v5/account/config's response data array (only the
// field this codebase needs today — OKX returns many more).
type AccountConfig struct {
	PosMode string `json:"posMode"`
}

// ToDomain converts an AccountConfig to its domain representation.
func (a AccountConfig) ToDomain() domain.AccountConfig {
	return domain.AccountConfig{PosMode: a.PosMode}
}

// SetPositionModeRequest is POST /api/v5/account/set-position-mode's body.
type SetPositionModeRequest struct {
	PosMode string `json:"posMode"`
}

// Instrument mirrors a single entry of the GET /api/v5/public/instruments response data array
// (only the fields this codebase needs for order-sizing conversion, CLAUDE.md §14/§27).
type Instrument struct {
	InstID   string        `json:"instId"`
	CtVal    decimalOrZero `json:"ctVal"`
	LotSz    decimalOrZero `json:"lotSz"`
	MinSz    decimalOrZero `json:"minSz"`
	CtValCcy string        `json:"ctValCcy"`
	// TickSz is the price increment every price sent to OKX must be a multiple of. It was not
	// decoded until 2026-09-10, which is why SL/TP prices derived from a percentage were rejected.
	TickSz decimalOrZero `json:"tickSz"`
}

// ToDomain converts an Instrument to its domain representation.
func (i Instrument) ToDomain() domain.Instrument {
	return domain.Instrument{
		InstID: i.InstID, CtVal: i.CtVal.Decimal, LotSz: i.LotSz.Decimal, MinSz: i.MinSz.Decimal,
		CtValCcy: i.CtValCcy, TickSz: i.TickSz.Decimal,
	}
}

// AlgoOrderStatus mirrors a single entry of the GET /api/v5/trade/order-algo response data array —
// a resting conditional (stop-loss/take-profit) order's current state on the exchange.
//
// Trigger prices use decimalOrZero for the same reason OrderStatus's fill fields do: OKX returns
// "" rather than "0" for a side that was never set, and a one-sided order (a stop with no target)
// is the normal case here, not an edge one.
type AlgoOrderStatus struct {
	AlgoID      string        `json:"algoId"`
	InstID      string        `json:"instId"`
	State       string        `json:"state"`
	SLTriggerPx decimalOrZero `json:"slTriggerPx"`
	TPTriggerPx decimalOrZero `json:"tpTriggerPx"`
	// actualSide/ordId are populated once the order fires: which half of the OCO triggered, and
	// the ordinary order it created to flatten the position. Both were being discarded, which is
	// why an exchange-executed stop-loss was recorded as a manual close with no close data.
	ActualSide string `json:"actualSide"`
	OrdID      string `json:"ordId"`
}

// ToDomain converts an AlgoOrderStatus to its domain representation.
func (s AlgoOrderStatus) ToDomain() domain.AlgoOrderStatus {
	return domain.AlgoOrderStatus{
		AlgoID:      s.AlgoID,
		InstID:      s.InstID,
		State:       s.State,
		SLTriggerPx: s.SLTriggerPx.Decimal,
		TPTriggerPx: s.TPTriggerPx.Decimal,
		ActualSide:  s.ActualSide,
		OrdID:       s.OrdID,
	}
}

// MarketTicker is GET /api/v5/market/tickers' wire shape — the all-instruments discovery endpoint
// (2026-09-13), deliberately a SEPARATE type from Ticker above rather than extra fields on it.
//
// Two reasons it has to be separate, both learned from the live response:
//
//   - Its decimals are LooseDecimal, because OKX returns "" for every price field of an instrument
//     that has never traded, and one such row fails the decode for the entire array (see
//     LooseDecimal's own comment). Ticker must stay strict: on the trading path an empty price is a
//     fault, and reading it as zero is how a close gets recorded at the wrong number (§37).
//   - It reads volCcy24h, the BASE-CURRENCY volume, which is what a cross-instrument comparison
//     needs once multiplied by price. Ticker.Vol24h is bound to vol24h, a CONTRACT count — the two
//     differ by the contract multiplier (live-verified on EDGE-USDT-SWAP: 8,559,200 vs 855,920), and
//     ranking a market by contract count orders it by contract size rather than by activity.
type MarketTicker struct {
	InstID    string       `json:"instId"`
	Last      LooseDecimal `json:"last"`
	Open24h   LooseDecimal `json:"open24h"`
	High24h   LooseDecimal `json:"high24h"`
	Low24h    LooseDecimal `json:"low24h"`
	VolCcy24h LooseDecimal `json:"volCcy24h"`
}

// ToDomain normalizes an OKX market ticker into the exchange-agnostic discovery shape. Both
// conversions happen here, at OKX's own boundary, so the scanner never learns that OKX reports
// volume in base currency or that it reports an open price where MEXC reports a rate (CLAUDE.md
// §46's instruction that one exchange's peculiarities must not be imposed on every exchange).
func (t MarketTicker) ToDomain() domain.MarketTicker {
	m := domain.MarketTicker{
		InstID:    t.InstID,
		Last:      t.Last.Decimal,
		Open24h:   t.Open24h.Decimal,
		High24h:   t.High24h.Decimal,
		Low24h:    t.Low24h.Decimal,
		Vol24hUSD: t.VolCcy24h.Mul(t.Last.Decimal),
	}
	// Guarded against a zero open so a newly-listed or never-traded instrument yields 0 rather than
	// a divide-by-zero panic.
	if !t.Open24h.IsZero() {
		m.Change24hPct = t.Last.Sub(t.Open24h.Decimal).Div(t.Open24h.Decimal).Mul(decimal.NewFromInt(100))
	}
	return m
}
