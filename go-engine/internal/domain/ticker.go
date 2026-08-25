package domain

import "github.com/shopspring/decimal"

// Ticker is a normalized market ticker snapshot for an instrument.
type Ticker struct {
	InstID  string
	Last    decimal.Decimal
	AskPx   decimal.Decimal
	BidPx   decimal.Decimal
	Open24h decimal.Decimal
	High24h decimal.Decimal
	Low24h  decimal.Decimal
	Vol24h  decimal.Decimal
}
