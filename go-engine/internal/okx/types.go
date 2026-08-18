package okx

// Ticker is a normalized OKX v5 "tickers" channel / REST market ticker payload.
type Ticker struct {
	InstID    string `json:"instId"`
	Last      string `json:"last"`
	AskPx     string `json:"askPx"`
	BidPx     string `json:"bidPx"`
	Open24h   string `json:"open24h"`
	High24h   string `json:"high24h"`
	Low24h    string `json:"low24h"`
	Vol24h    string `json:"vol24h"`
	Timestamp string `json:"ts"`
}

// Position mirrors OKX's /api/v5/account/positions entry (fields we care about).
type Position struct {
	InstID      string `json:"instId"`
	PosSide     string `json:"posSide"`
	Pos         string `json:"pos"`
	AvgPx       string `json:"avgPx"`
	Lever       string `json:"lever"`
	Upl         string `json:"upl"`
	UplRatio    string `json:"uplRatio"`
	LiqPx       string `json:"liqPx"`
	MarkPx      string `json:"markPx"`
	NotionalUsd string `json:"notionalUsd"`
	MgnMode     string `json:"mgnMode"`
}

// Balance mirrors a single currency entry from /api/v5/account/balance.
type Balance struct {
	Ccy     string `json:"ccy"`
	Eq      string `json:"eq"`
	AvailEq string `json:"availEq"`
}

// OrderRequest is the payload for POST /api/v5/trade/order.
type OrderRequest struct {
	InstID  string `json:"instId"`
	TdMode  string `json:"tdMode"`            // "cross" or "isolated"
	Side    string `json:"side"`              // "buy" or "sell"
	PosSide string `json:"posSide,omitempty"` // "long" or "short" (hedge mode)
	OrdType string `json:"ordType"`           // "market", "limit", ...
	Sz      string `json:"sz"`                // size in contracts
	Px      string `json:"px,omitempty"`      // required for limit orders
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
	InstID  string `json:"instId"`
	Lever   string `json:"lever"`
	MgnMode string `json:"mgnMode"`           // "cross" or "isolated"
	PosSide string `json:"posSide,omitempty"` // required in hedge mode
}
