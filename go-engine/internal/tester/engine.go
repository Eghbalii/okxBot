package tester

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// BuildOrder turns a fired signal into an Order ready to insert — fixed notional/leverage (no RL
// sizing here, per the operator's own "size/leverage doesn't matter, start flat" instruction),
// levels resolved from the signal's own percentages/prices via strategy.Signal.ResolveLevels so
// this reuses exactly the same price-resolution logic production strategies already rely on.
func BuildOrder(instID string, versionID int64, bar string, price, notionalUSD, leverage decimal.Decimal, signal strategy.Signal) Order {
	resolved := signal.ResolveLevels(price)
	var slPx, tpPx *decimal.Decimal
	if resolved.SLPx.IsPositive() {
		v := resolved.SLPx
		slPx = &v
	}
	if resolved.TPPx.IsPositive() {
		v := resolved.TPPx
		tpPx = &v
	}
	return Order{
		InstID:    instID,
		VersionID: versionID,
		Bar:       bar,
		Side:      string(signal.Side),
		EntryPx:   price,
		SLPx:      slPx,
		TPPx:      tpPx,
		Size:      notionalUSD,
		Leverage:  leverage,
		OpenedAt:  time.Now().UTC(),
	}
}

// RealizedPnL computes a closed position's PnL in USD from entry/close/size/leverage — identical
// math to usecase.realizedPnL, duplicated here (not imported) because that function is
// package-private to usecase and this service is deliberately independent of it.
func RealizedPnL(side string, entryPx, closePx, size, leverage decimal.Decimal) decimal.Decimal {
	direction := decimal.NewFromInt(1)
	if side == "sell" {
		direction = decimal.NewFromInt(-1)
	}
	return direction.Mul(closePx.Sub(entryPx)).Div(entryPx).Mul(size).Mul(leverage)
}
