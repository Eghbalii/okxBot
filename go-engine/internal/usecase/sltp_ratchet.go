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

// ratchetSL only allows moving SL in the risk-reducing direction: for a long, that's up (toward
// or past entry, locking in profit); for a short, down. A proposal that would move SL the other
// way (widening risk, or undoing a previous tightening) is rejected and the existing SL is kept.
func ratchetSL(current *decimal.Decimal, direction, price, adjustPct decimal.Decimal) *decimal.Decimal {
	if current == nil {
		return nil // no SL set; the ratchet only tightens an existing one, it doesn't create one
	}
	// direction=1 (long): risk-reducing SL move is upward (positive delta). direction=-1 (short):
	// risk-reducing move is downward, so a positive adjustPct must produce a negative delta.
	delta := direction.Mul(adjustPct).Mul(price)
	proposed := current.Add(delta)

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

// moveTP applies the model's proposed TP move with NO ratchet and NO per-step size cap (removed
// 2026-09-04, explicit operator decision): the model may move the target closer (locking in a
// nearer profit) or further away (letting a winning trade run for more) by any amount in one step.
// The only guard left is that the result stays on the profitable side of entry — a "take-profit"
// that crosses entry would realize a LOSS on touch, which isn't a take-profit at all. Guarding only
// against the current price is not enough on its own (see the 2026-08-29 order 100/110 incident
// this function's predecessor was built to fix), so entry is what's actually checked here.
func moveTP(current *decimal.Decimal, direction, price, entry, adjustPct decimal.Decimal) *decimal.Decimal {
	if current == nil {
		return nil
	}
	delta := direction.Mul(adjustPct).Mul(price)
	proposed := current.Add(delta)

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
