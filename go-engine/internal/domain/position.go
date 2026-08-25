package domain

import "github.com/shopspring/decimal"

// Position is an open (or flat) position for an instrument.
type Position struct {
	InstID      string
	PosSide     string // "long", "short", or "" (net mode)
	Pos         decimal.Decimal
	AvgPx       decimal.Decimal
	Lever       decimal.Decimal
	Upl         decimal.Decimal
	UplRatio    decimal.Decimal
	LiqPx       decimal.Decimal
	MarkPx      decimal.Decimal
	NotionalUsd decimal.Decimal
	MgnMode     string // "cross" or "isolated"
}

// Balance is a single currency's account balance.
type Balance struct {
	Ccy     string
	Eq      decimal.Decimal
	AvailEq decimal.Decimal
}
