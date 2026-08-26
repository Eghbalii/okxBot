package usecase

import (
	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// MaxSLTPAdjustPct is the largest single-step SL/TP adjustment (as a fraction of price) the RL
// agent's SLAdjustPct/TPAdjustPct is allowed to request, independent of what the model itself
// proposes — CLAUDE.md §15.4's "±2% per decision step" starting point. Applied before the ratchet
// direction check below.
var MaxSLTPAdjustPct = decimal.NewFromFloat(0.02)

// RatchetSLTP computes the new SL/TP prices for an open order given the RL agent's proposed
// adjustment percentages, enforcing the CLAUDE.md §15.4 hard constraint: an adjustment may only
// tighten the position's risk (move SL toward locking in profit / reduce the TP distance so it's
// easier to hit sooner), never widen it past the order's original risk budget or walk back a prior
// tightening. This is a Go-side clamp applied unconditionally — the model's own output is never
// trusted as the safety boundary here, matching the pattern in internal/risk.
//
// slAdjustPct/tpAdjustPct are fractions of price (already clamped to +/-MaxSLTPAdjustPct by the
// caller or here); positive/negative sign is interpreted relative to the position's side so the
// caller doesn't need to reason about buy/sell asymmetry itself.
func RatchetSLTP(o port.PaperOrder, currentPrice, slAdjustPct, tpAdjustPct decimal.Decimal) (newSL, newTP *decimal.Decimal) {
	slAdjustPct = clampAbs(slAdjustPct, MaxSLTPAdjustPct)
	tpAdjustPct = clampAbs(tpAdjustPct, MaxSLTPAdjustPct)

	direction := decimal.NewFromInt(1)
	if o.Side == "sell" {
		direction = decimal.NewFromInt(-1)
	}

	newSL = ratchetSL(o.SLPx, direction, currentPrice, slAdjustPct)
	newTP = ratchetTP(o.TPPx, direction, currentPrice, tpAdjustPct)
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

// ratchetTP only allows moving TP closer to the current price (reducing the distance still needed
// to hit it, i.e. "locking in" a nearer target) — never further away, which would loosen the
// original risk/reward budget the order was opened with.
func ratchetTP(current *decimal.Decimal, direction, price, adjustPct decimal.Decimal) *decimal.Decimal {
	if current == nil {
		return nil
	}
	delta := direction.Mul(adjustPct).Mul(price)
	proposed := current.Sub(delta) // subtract: shrinking the TP distance moves TP toward price

	// "Closer to current price" for a long means TP moving down (but never below price); for a
	// short, TP moving up (but never below... i.e. never past price in the wrong direction).
	if direction.IsPositive() {
		if proposed.LessThan(*current) && proposed.GreaterThan(price) {
			return &proposed
		}
		return current
	}
	if proposed.GreaterThan(*current) && proposed.LessThan(price) {
		return &proposed
	}
	return current
}

func clampAbs(v, max decimal.Decimal) decimal.Decimal {
	if v.GreaterThan(max) {
		return max
	}
	if v.LessThan(max.Neg()) {
		return max.Neg()
	}
	return v
}
