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
	// Bound the target against the stop the position actually carries, using the RATCHETED stop
	// rather than the original: newSL is what the trade is now risking, so measuring reward against
	// the old one would let a tightened stop silently justify a target matched to risk no longer
	// being taken — the same ordering conductor.Clamps.Apply uses on the open path.
	newTP = clampTPToRatio(newTP, newSL, o.EntryPx, direction)
	return newSL, newTP
}

// MaxInTradeTPSLRatio bounds reward:risk on an IN-TRADE target move, mirroring
// conductor.Clamps.MaxTPSLRatio on the open path.
//
// It exists because that open-path cap turned out to bound only where a target STARTS, not where it
// ends up (found 2026-09-14 on order 3356: a LINK short that opened with a correct 0.75% target and
// was walked by the model to 33.5% away over three adjustments — a 152:1 reward:risk on a 5m scalp,
// and 13 of 19 open positions past the 3:1 cap at the time, the worst at 298:1).
//
// MaxTPDistPct (50% of entry) did not catch it and was never meant to: §31.1 chose that number
// purely to stop an unbounded WALK to infinity, deliberately far wider than any realistic target so
// it constrains only the runaway case. A ratio bound is what makes a target reachable, for the same
// reason §45 gives on the open path: it scales with the instrument's own volatility exactly as the
// stop does, so one bound is right for BTC and PEPE alike.
//
// Deliberately LOOSER than the open path's 3:1. A position that has run in profit has usually had
// its stop ratcheted tighter, which mechanically raises the ratio without the target having moved at
// all — bounding an in-trade move as tightly as an opening one would drag targets in every time the
// stop tightened, which is the opposite of letting a winner run.
const MaxInTradeTPSLRatio = 6.0

// clampTPToRatio pulls a target back to at most MaxInTradeTPSLRatio times the stop distance.
//
// Clamped rather than rejected, matching moveTP's own price guard: rejecting would keep whatever
// over-wide target is already on the order, which is exactly the state being corrected. Returns the
// target untouched when there is no stop to measure against — with no risk leg there is no ratio,
// and inventing an absolute distance here would be a different rule than the one this states.
func clampTPToRatio(tp, sl *decimal.Decimal, entry, direction decimal.Decimal) *decimal.Decimal {
	if tp == nil || sl == nil || !entry.IsPositive() {
		return tp
	}
	slDist := sl.Sub(entry).Abs()
	if !slDist.IsPositive() {
		return tp
	}
	maxDist := slDist.Mul(decimal.NewFromFloat(MaxInTradeTPSLRatio))
	if tp.Sub(entry).Abs().LessThanOrEqual(maxDist) {
		return tp
	}
	// Rebuild on the profitable side of entry: above for a long, below for a short.
	clamped := entry.Add(direction.Mul(maxDist))
	return &clamped
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

// TPPriceGapPct is how far beyond the live price a clamped take-profit is placed, as a fraction of
// price. Mirrors SLPriceGapPct and exists for the same reason: a target sitting exactly at price is
// already touched, so clamping to the boundary itself would still close on the very next tick.
const TPPriceGapPct = 0.001 // 0.1%

// moveTP applies the model's proposed TP move with NO ratchet and no per-step size cap (removed
// 2026-09-04, explicit operator decision): the model may move the target closer (locking in a
// nearer profit) or further away (letting a winning trade run for more) in one step.
//
// Three guards remain, answering different questions.
//
// The result must stay on the profitable side of ENTRY — a "take-profit" that crosses entry
// realizes a LOSS on touch, which isn't a take-profit at all (the 2026-08-29 order 100/110
// incident this function's predecessor was built to fix).
//
// It must stay within MaxTPDistPct of entry, which is what stops repeated calls from walking the
// target to infinity (the §31.1 order 1851 runaway).
//
// And it must stay on the unreached side of the LIVE PRICE. This is the SL guard's exact mirror
// (ratchetSL's clamp, added 2026-09-05) and was missing here until 2026-09-07: entry and price
// answer different questions once a trade is in profit. For a long that has run up, everything
// between entry and price is "above entry" and passes the entry check, yet a target placed there
// is already behind the market — the next tick closes at whatever fraction of the move it happens
// to sit on. Found on order 2595 (PUMP long, entry 0.004315): at +6.5% unrealized the model pulled
// the target from 0.004664 to 0.004322 while price was 0.004349, and the trade closed 39 seconds
// after opening for a fraction of what it had reached. A scan of the trade log found 27 orders with
// the same shape, several peaking near +10% and realizing a few cents.
//
// Clamped rather than rejected, matching ratchetSL's own revision: rejecting throws away every
// proposal the model makes near price and costs it the ability to pull a target in at all, which is
// legitimate behaviour — only landing it BEHIND price is not.
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

	// Keep the target strictly beyond the live price, so it can still be reached rather than being
	// already behind the market. Applied before the entry check below, which then rejects anything
	// this clamp could not place profitably (a long whose price has fallen below entry, say).
	if price.IsPositive() {
		gap := price.Mul(decimal.NewFromFloat(TPPriceGapPct))
		if direction.IsPositive() {
			if limit := price.Add(gap); proposed.LessThan(limit) {
				proposed = limit
			}
		} else if limit := price.Sub(gap); proposed.GreaterThan(limit) {
			proposed = limit
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
