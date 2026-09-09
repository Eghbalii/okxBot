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
	// TickSz is the instrument's price increment: every price sent to the exchange must be a
	// multiple of it. Missing this is what made SL/TP amends fail (2026-09-10) — a stop computed
	// from a percentage lands on an arbitrary number of decimals (100.41424 against a 0.01 tick),
	// and OKX rejects the whole request with a bare "code=1" that names nothing.
	TickSz decimal.Decimal
}

// RoundPriceToTick snaps px to the instrument's price increment, which every price sent to the
// exchange must respect. A non-positive tick means the instrument metadata did not report one, and
// the price is returned unchanged rather than being mangled by a guess.
//
// Rounds to NEAREST rather than truncating: the caller's intent is a level, and moving it up or
// down by less than one tick is the smallest possible change that OKX will accept. Which direction
// is safer depends on the side and on whether it is a stop or a target, and a sub-tick difference
// is not worth threading that through — the clamps upstream already bound the level itself.
func (i Instrument) RoundPriceToTick(px decimal.Decimal) decimal.Decimal {
	if !i.TickSz.IsPositive() || !px.IsPositive() {
		return px
	}
	return px.Div(i.TickSz).Round(0).Mul(i.TickSz)
}
