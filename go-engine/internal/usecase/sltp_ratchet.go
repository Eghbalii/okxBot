package usecase

import (
	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// RatchetSLTP computes the new SL/TP prices for an open order given the RL agent's proposed
// adjustment percentages.
//
// SL is still a hard one-way ratchet (CLAUDE.md §15.4): it may only move to reduce risk (toward
// locking in profit), never widen it past the order's original risk budget or walk back a prior
// tightening. This is a Go-side clamp applied unconditionally — the model's own output is never
// trusted as the safety boundary here, matching the pattern in internal/risk.
//
// TP has no ratchet and no per-step size cap (removed 2026-09-04, explicit operator decision): the
// model is free to move it either closer (locking in a nearer target) or FURTHER away (letting a
// winning trade run for more), and to move it by any amount in one step. The only constraint left
// is that it stays on the profitable side of entry — a "take-profit" that crosses entry would
// realize a loss on touch, which isn't a take-profit at all.
//
// slAdjustPct/tpAdjustPct are fractions of price; positive/negative sign is interpreted relative
// to the position's side so the caller doesn't need to reason about buy/sell asymmetry itself.
func RatchetSLTP(o port.PaperOrder, currentPrice, slAdjustPct, tpAdjustPct decimal.Decimal) (newSL, newTP *decimal.Decimal) {
	direction := decimal.NewFromInt(1)
	if o.Side == "sell" {
		direction = decimal.NewFromInt(-1)
	}

	newSL = ratchetSL(o.SLPx, direction, currentPrice, slAdjustPct)
	newTP = moveTP(o.TPPx, direction, currentPrice, o.EntryPx, tpAdjustPct)
	return newSL, newTP
}

// SLPriceGapPct is how far short of the live price a clamped stop is placed, as a fraction of
// price. Small enough that the model keeps essentially all of its trailing range, but non-zero
// because a stop exactly at price is already touched — the clamp has to land strictly inside it or
// it does not prevent the instant close it exists for.
const SLPriceGapPct = 0.001 // 0.1%

// ratchetSL only allows moving SL in the risk-reducing direction: for a long, that's up (toward
// or past entry, locking in profit); for a short, down. A proposal that would move SL the other
// way (widening risk, or undoing a previous tightening) is rejected and the existing SL is kept.
//
// A tightened stop must still stay on the protective side of the LIVE PRICE. Crossing entry is
// intended — that is what trailing into profit means — but a stop placed beyond current price is
// not protection at all: it is already touched, so the next tick closes the position at market,
// instantly, for whatever that costs. Found live 2026-09-05, minutes after the RL flags were first
// enabled: orders 2043 (TRUMP long, entry 2.388) and 2044 (DOGE short) each had their stop ratcheted
// past price within 30 seconds of opening and closed immediately at a loss, with a stop sitting on
// the far side of their own take-profit. The one-way rule bounded DIRECTION but nothing bounded how
// far, the same shape as the moveTP runaway (MaxTPDistPct) fixed the same day.
//
// An over-reaching proposal is CLAMPED to just short of price, not rejected (revised 2026-09-06
// after measuring the first version's effect: 5 SL adjustments against 306 TP ones, a 1:61 ratio —
// the model was proposing stop moves constantly and nearly every one was being thrown away, so it
// had effectively no control over the stop at all). Clamping keeps the stop dynamic — the model
// still decides where it goes, and can walk it right up behind price to lock in profit — while
// making the instant-close outcome unreachable. Rejecting was the safer first move when the
// failure was fresh and the model's calibration unknown; the data since then shows the cost of
// that caution was the whole capability.
func ratchetSL(current *decimal.Decimal, direction, price, adjustPct decimal.Decimal) *decimal.Decimal {
	if current == nil {
		return nil // no SL set; the ratchet only tightens an existing one, it doesn't create one
	}
	// direction=1 (long): risk-reducing SL move is upward (positive delta). direction=-1 (short):
	// risk-reducing move is downward, so a positive adjustPct must produce a negative delta.
	delta := direction.Mul(adjustPct).Mul(price)
	proposed := current.Add(delta)

	// Clamp to a hair inside the live price rather than to the price itself: a stop sitting exactly
	// at price is still touched by the very next tick, so clamping to the boundary would reproduce
	// the instant close this guard exists to prevent.
	if price.IsPositive() {
		gap := price.Mul(decimal.NewFromFloat(SLPriceGapPct))
		if direction.IsPositive() {
			if limit := price.Sub(gap); proposed.GreaterThan(limit) {
				proposed = limit
			}
		} else if limit := price.Add(gap); proposed.LessThan(limit) {
			proposed = limit
		}
	}

	// Keep whichever is more protective: for a long, the higher of current/proposed; for a short,
	// the lower. This is what makes the ratchet a one-way ratchet — a proposal that would move SL
	// backward (loosening) is silently clamped back to the current value, never applied.
	if direction.IsPositive() {
		if proposed.GreaterThan(*current) {
			return &proposed
		}
		return current
	}
	if proposed.LessThan(*current) {
		return &proposed
	}
	return current
}

// MaxTPDistPct bounds how far a take-profit may sit from entry, as a fraction of entry price.
//
// Restored 2026-09-05 after the §31.1 incident: §29 removed moveTP's per-step size cap (so a
// well-judged move could be made in one step rather than accumulating over many), leaving
// "stays on the profitable side of entry" as the only guard. That guard bounds DIRECTION but not
// DISTANCE, and moveTP runs off the tick stream every RLAdjustInterval — so a model with any
// persistent bias walks the target outward a little on every call, unopposed, forever. Observed
// live: order 1851's TP marched from 936 to -566552 in 19 adjustments over 6 minutes, roughly
// doubling each time.
//
// A distance ceiling fixes that without reintroducing the per-step cap §29 deliberately removed:
// a single large adjustment is still allowed, an unbounded WALK is not. 50% of entry is far wider
// than any realistic target on a 5m/15m/1H scalp, so this constrains only the runaway case.
const MaxTPDistPct = 0.50

// moveTP applies the model's proposed TP move with NO ratchet and no per-step size cap (removed
// 2026-09-04, explicit operator decision): the model may move the target closer (locking in a
// nearer profit) or further away (letting a winning trade run for more) in one step.
//
// Two guards remain, answering different questions. The result must stay on the profitable side of
// ENTRY — a "take-profit" that crosses entry realizes a LOSS on touch, which isn't a take-profit at
// all, and checking against the current price instead is not sufficient (the 2026-08-29 order
// 100/110 incident this function's predecessor was built to fix). And it must stay within
// MaxTPDistPct of entry, which is what stops repeated calls from walking the target to infinity.
func moveTP(current *decimal.Decimal, direction, price, entry, adjustPct decimal.Decimal) *decimal.Decimal {
	if current == nil {
		return nil
	}
	delta := direction.Mul(adjustPct).Mul(price)
	proposed := current.Add(delta)

	// Reject a target further from entry than the ceiling allows, rather than clamping it there:
	// clamping would silently accept every runaway proposal at the boundary, making a
	// persistently-biased model look like it had converged on a 50%-away target.
	if entry.IsPositive() {
		maxDist := entry.Mul(decimal.NewFromFloat(MaxTPDistPct))
		if proposed.Sub(entry).Abs().GreaterThan(maxDist) {
			return current
		}
	}

	// A long's target must stay above entry, a short's below — either direction of movement is
	// otherwise allowed.
	if direction.IsPositive() {
		if proposed.GreaterThan(entry) {
			return &proposed
		}
		return current
	}
	if proposed.LessThan(entry) {
		return &proposed
	}
	return current
}
