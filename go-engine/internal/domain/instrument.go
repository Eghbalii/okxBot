package domain

import "github.com/shopspring/decimal"

// Instrument is an exchange's contract-shape metadata for one tradeable instId — required to
// convert a desired notional/base-unit size into the contract count an order actually needs
// (CLAUDE.md §14's long-standing known gap: "order sizing assumes a contract multiplier of 1").
// Found load-bearing 2026-09-04: OKX's real-trading-eligible instrument for this account
// (BTC-USD_UM_XPERP-<date>, not the classic BTC-USDT-SWAP the rest of this codebase collects
// market data from) has CtVal=0.0001 and LotSz=1 — a contract multiplier assumption of 1 would
// size every real order roughly 10,000x too large.
type Instrument struct {
	InstID   string
	CtVal    decimal.Decimal // contract value, in CtValCcy units, per 1 contract
	LotSz    decimal.Decimal // order size must be a multiple of this many contracts
	MinSz    decimal.Decimal // minimum order size, in contracts
	CtValCcy string          // currency CtVal is denominated in (e.g. "BTC")
}
