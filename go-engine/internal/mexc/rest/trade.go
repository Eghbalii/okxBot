package rest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/shopspring/decimal"
)

// This file translates between MEXC's trading vocabulary and the domain types the rest of this
// system speaks. The domain deliberately keeps OKX-derived names (TdMode, state "live"/"filled")
// rather than being renamed: those names are already load-bearing across a live real-money path,
// and §31 records what churning that path costs. Translating here is what an adapter is for — the
// domain is a shared language, not OKX's wire format, even where the two happen to coincide.
//
// NOTE ON VERIFICATION: every endpoint in this file is authenticated, so unlike market.go's
// public endpoints none of it could be exercised against the live exchange without credentials.
// The request/response shapes follow MEXC's published futures API; the numeric-code mappings below
// are the part most likely to need correction once a real key is available, and are written to fail
// loudly (unknown state -> error naming the value) rather than guess.

// MEXC side codes. MEXC encodes direction and intent in ONE integer, where OKX splits them across
// side + posSide. 1/3 open, 2/4 close — so "sell" means different codes depending on whether the
// order opens a short or closes a long, which is why this cannot be a simple side lookup.
const (
	sideOpenLong   = 1
	sideCloseShort = 2
	sideOpenShort  = 3
	sideCloseLong  = 4
)

// MEXC order types: 5 = market, 1 = limit.
const (
	ordTypeLimit  = 1
	ordTypeMarket = 5
)

// MEXC open types: 1 = isolated, 2 = cross.
const (
	openTypeIsolated = 1
	openTypeCross    = 2
)

// openTypeFor maps the domain's margin-mode vocabulary onto MEXC's integer.
//
// Defaults to ISOLATED for an unrecognised value rather than cross. That is the conservative
// direction: isolated bounds a liquidation to one position's margin, while cross exposes the whole
// account, and §27.2 chose isolated for exactly that reason. A silent default must fail safe.
func openTypeFor(mode string) int {
	if strings.EqualFold(mode, "cross") {
		return openTypeCross
	}
	return openTypeIsolated
}

// sideCodeFor converts the domain's (side, posSide) pair into MEXC's single side integer.
//
// posSide carries whether this order opens or closes. In hedge mode the domain sets it to
// "long"/"short"; a close is expressed by side opposing posSide — selling while long closes, buying
// while long opens. With no posSide (net mode) the order is treated as opening, which matches how
// RealTrader places entries.
func sideCodeFor(side, posSide string) (int, error) {
	buy := strings.EqualFold(side, "buy")
	sell := strings.EqualFold(side, "sell")
	if !buy && !sell {
		return 0, fmt.Errorf("unknown side %q", side)
	}
	switch strings.ToLower(posSide) {
	case "long":
		if buy {
			return sideOpenLong, nil
		}
		return sideCloseLong, nil
	case "short":
		if sell {
			return sideOpenShort, nil
		}
		return sideCloseShort, nil
	case "":
		if buy {
			return sideOpenLong, nil
		}
		return sideOpenShort, nil
	default:
		return 0, fmt.Errorf("unknown posSide %q", posSide)
	}
}

// placeOrderReq is MEXC's POST /api/v1/private/order/submit body.
type placeOrderReq struct {
	Symbol      string `json:"symbol"`
	Price       string `json:"price,omitempty"`
	Vol         string `json:"vol"`
	Side        int    `json:"side"`
	Type        int    `json:"type"`
	OpenType    int    `json:"openType"`
	Leverage    int    `json:"leverage,omitempty"`
	ExternalOID string `json:"externalOid,omitempty"`
}

// PlaceOrder submits an order.
//
// Size is in CONTRACTS, already converted by the caller via the instrument's CtVal (§33.3 — this
// system learned that assuming a multiplier of 1 would size orders ~10,000x wrong). This adapter
// does not re-convert; it sends what it is given.
func (c *Client) PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error) {
	sideCode, err := sideCodeFor(req.Side, req.PosSide)
	if err != nil {
		return nil, fmt.Errorf("mexc place order %s: %w", req.InstID, err)
	}
	ordType := ordTypeMarket
	if strings.EqualFold(req.OrdType, "limit") {
		ordType = ordTypeLimit
	}
	body := placeOrderReq{
		Symbol:   req.InstID,
		Vol:      req.Sz.String(),
		Side:     sideCode,
		Type:     ordType,
		OpenType: openTypeFor(req.TdMode),
	}
	if ordType == ordTypeLimit {
		if !req.Px.IsPositive() {
			return nil, fmt.Errorf("mexc place order %s: limit order requires a price", req.InstID)
		}
		body.Price = req.Px.String()
	}

	// MEXC returns the order id as a bare value in `data`, not an object.
	var raw json.RawMessage
	if err := c.do("POST", "/api/v1/private/order/submit", nil, body, &raw); err != nil {
		return nil, fmt.Errorf("mexc place order %s: %w", req.InstID, err)
	}
	ordID := strings.Trim(string(raw), `"`)

	return &domain.OrderResult{
		OrdID: ordID,
		// SCode "0" is this system's success sentinel, inherited from OKX's vocabulary. A MEXC
		// failure never reaches here — it returns an error from do() above — so reaching this
		// point IS the success case, and reporting anything else would make callers branch on a
		// condition that cannot occur.
		SCode: "0",
	}, nil
}

// CancelOrder cancels one still-open order.
//
// MEXC's cancel takes an ARRAY of order ids and reports per-item results, so a single-order cancel
// is a one-element batch. instID is unused (the id is globally unique) but stays in the signature
// because port.ExchangeClient defines it — OKX needs it.
func (c *Client) CancelOrder(instID, ordID string) error {
	if err := c.do("POST", "/api/v1/private/order/cancel", nil, []string{ordID}, nil); err != nil {
		return fmt.Errorf("mexc cancel order %s: %w", ordID, err)
	}
	return nil
}

// orderStatusResp is /api/v1/private/order/get/{order_id}'s data object.
type orderStatusResp struct {
	OrderID      json.Number `json:"orderId"`
	Symbol       string      `json:"symbol"`
	Price        json.Number `json:"price"`
	Vol          json.Number `json:"vol"`
	DealVol      json.Number `json:"dealVol"`
	DealAvgPrice json.Number `json:"dealAvgPrice"`
	State        int         `json:"state"`
	Profit       json.Number `json:"profit"`
	Fee          json.Number `json:"takerFee"`
	MakerFee     json.Number `json:"makerFee"`
	ExternalOID  string      `json:"externalOid"`
}

// MEXC order states. 1 uninformed, 2 uncompleted, 3 completed, 4 cancelled, 5 invalid.
const (
	mexcStateUninformed  = 1
	mexcStateUncompleted = 2
	mexcStateCompleted   = 3
	mexcStateCancelled   = 4
	mexcStateInvalid     = 5
)

// domainState maps MEXC's numeric order state onto the domain's string vocabulary.
//
// The partially-filled distinction is NOT available from the state code alone — MEXC reports
// "uncompleted" for both an untouched resting order and one that is half filled — so it is derived
// from dealVol, which is the only field that actually carries it. Getting this wrong matters: §27.5
// treats a partial fill as a real, smaller position, and a flatten that only partly fills must not
// be recorded as closed.
func domainState(state int, dealVol decimal.Decimal) (string, error) {
	switch state {
	case mexcStateCompleted:
		return "filled", nil
	case mexcStateCancelled, mexcStateInvalid:
		return "canceled", nil
	case mexcStateUncompleted, mexcStateUninformed:
		if dealVol.IsPositive() {
			return "partially_filled", nil
		}
		return "live", nil
	default:
		// Never guess. An unmapped state reaching the fill-timeout logic as "live" would make the
		// caller wait out the full timeout on an order that may already be dead.
		return "", fmt.Errorf("unknown mexc order state %d", state)
	}
}

// GetOrder fetches an order's authoritative current state.
func (c *Client) GetOrder(instID, ordID string) (domain.OrderStatus, error) {
	var resp orderStatusResp
	if err := c.do("GET", "/api/v1/private/order/get/"+ordID, nil, nil, &resp); err != nil {
		return domain.OrderStatus{}, fmt.Errorf("mexc get order %s: %w", ordID, err)
	}
	dealVol := num(resp.DealVol)
	state, err := domainState(resp.State, dealVol)
	if err != nil {
		return domain.OrderStatus{}, fmt.Errorf("mexc get order %s: %w", ordID, err)
	}
	return domain.OrderStatus{
		InstID:    resp.Symbol,
		OrdID:     resp.OrderID.String(),
		ClOrdID:   resp.ExternalOID,
		State:     state,
		AvgPx:     num(resp.DealAvgPrice),
		AccFillSz: dealVol,
		Sz:        num(resp.Vol),
		// MEXC reports fees as positive charges; the domain's convention is NEGATIVE for a charge
		// (§41 depends on this: realized PnL is exchange gross PLUS fee, and a sign error there
		// would credit the fee and overstate every trade by twice its cost).
		Fee: num(resp.Fee).Neg(),
		Pnl: num(resp.Profit),
	}, nil
}

// SetLeverage changes an instrument's leverage.
func (c *Client) SetLeverage(req domain.LeverageChange) error {
	body := map[string]any{
		"symbol":     req.InstID,
		"leverage":   req.Lever.IntPart(),
		"openType":   openTypeFor(req.MgnMode),
		"positionId": 0,
	}
	if req.PosSide != "" {
		// MEXC's positionType: 1 = long, 2 = short.
		if strings.EqualFold(req.PosSide, "long") {
			body["positionType"] = 1
		} else {
			body["positionType"] = 2
		}
	}
	if err := c.do("POST", "/api/v1/private/position/change_leverage", nil, body, nil); err != nil {
		return fmt.Errorf("mexc set leverage %s: %w", req.InstID, err)
	}
	return nil
}

// positionResp is one entry of /api/v1/private/position/open_positions.
type positionResp struct {
	PositionID     json.Number `json:"positionId"`
	Symbol         string      `json:"symbol"`
	PositionType   int         `json:"positionType"` // 1 long, 2 short
	OpenType       int         `json:"openType"`     // 1 isolated, 2 cross
	HoldVol        json.Number `json:"holdVol"`
	HoldAvgPrice   json.Number `json:"holdAvgPrice"`
	LiquidatePrice json.Number `json:"liquidatePrice"`
	Leverage       json.Number `json:"leverage"`
	// Im is initial margin, used to derive UplRatio below — MEXC reports no unrealized-PnL ratio
	// of its own, where OKX reports uplRatio directly.
	Im json.Number `json:"im"`
	// Unrealized is this position's OPEN PnL. MEXC also reports `realised`, deliberately not read
	// here: domain.Position carries Upl (UNrealized) and has no realized field, so mapping the
	// wrong one would put a settled figure where every caller expects a live one.
	Unrealized json.Number `json:"unrealized"`
}

// GetPositions lists open positions.
//
// instType is accepted and ignored — MEXC has one futures product per symbol, with no analogue to
// OKX's SWAP/FUTURES split (§33.2). Honouring it would mean inventing a MEXC concept that does not
// exist.
func (c *Client) GetPositions(instType string) ([]domain.Position, error) {
	var resp []positionResp
	if err := c.do("GET", "/api/v1/private/position/open_positions", nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("mexc get positions: %w", err)
	}
	out := make([]domain.Position, 0, len(resp))
	for _, p := range resp {
		vol := num(p.HoldVol)
		if !vol.IsPositive() {
			continue // a flat entry is not a position
		}
		posSide := "long"
		signed := vol
		if p.PositionType == 2 {
			posSide = "short"
			// The domain carries a SIGNED position size (negative = short); MEXC reports magnitude
			// plus a direction field. §14 records a sign-unaware comparison here placing every
			// requested short as a long, so the sign is applied at this boundary rather than left
			// for callers to reconstruct.
			signed = vol.Neg()
		}
		mgnMode := "isolated"
		if p.OpenType == openTypeCross {
			mgnMode = "cross"
		}
		out = append(out, domain.Position{
			InstID:  p.Symbol,
			PosSide: posSide,
			Pos:     signed,
			AvgPx:   num(p.HoldAvgPrice),
			LiqPx:   num(p.LiquidatePrice),
			Lever:   num(p.Leverage),
			MgnMode: mgnMode,
			Upl:     num(p.Unrealized),
			// UplRatio is unrealized PnL over initial margin — the same quantity OKX reports
			// directly and which §19.1 established must be leverage-adjusted. Derived rather than
			// left zero, because the live observation path reads it and zero reads as a flat
			// position rather than as missing data.
			UplRatio: safeRatio(num(p.Unrealized), num(p.Im)),
		})
	}
	return out, nil
}

// assetResp is one entry of /api/v1/private/account/assets.
type assetResp struct {
	Currency     string      `json:"currency"`
	AvailableBal json.Number `json:"availableBalance"`
	Equity       json.Number `json:"equity"`
	FrozenBal    json.Number `json:"frozenBalance"`
}

// GetBalance lists account balances, optionally filtered to one currency.
func (c *Client) GetBalance(ccy string) ([]domain.Balance, error) {
	var resp []assetResp
	if err := c.do("GET", "/api/v1/private/account/assets", nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("mexc get balance: %w", err)
	}
	out := make([]domain.Balance, 0, len(resp))
	for _, a := range resp {
		if ccy != "" && !strings.EqualFold(a.Currency, ccy) {
			continue
		}
		out = append(out, domain.Balance{
			Ccy:     a.Currency,
			Eq:      num(a.Equity),
			AvailEq: num(a.AvailableBal),
		})
	}
	return out, nil
}

// safeRatio divides a by b, returning zero when b is not positive rather than panicking. An
// unreported initial margin must not take down a position poll.
func safeRatio(a, b decimal.Decimal) decimal.Decimal {
	if !b.IsPositive() {
		return decimal.Zero
	}
	return a.Div(b)
}
