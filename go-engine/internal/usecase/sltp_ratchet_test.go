package usecase

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

func ptr(d decimal.Decimal) *decimal.Decimal { return &d }

func TestRatchetSLTP_LongTighteningAllowed(t *testing.T) {
	// Long, entry 100, SL 95, TP 110. Price now 105. Agent proposes tightening SL up (locking
	// profit) and moving TP further away (more ambitious target, 2% adjust each).
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("110"))}
	price := dec("105")
	newSL, newTP := RatchetSLTP(o, price, dec("0.02"), dec("0.02"))

	// SL should move up by 2% of price (105*0.02=2.1) -> 97.1
	if !newSL.Equal(dec("97.1")) {
		t.Errorf("expected SL 97.1, got %s", newSL)
	}
	// TP has no ratchet/cap (2026-09-04): a positive tpAdjustPct moves a long's target further away
	// (more profit) -> 110 + 2.1 = 112.1
	if !newTP.Equal(dec("112.1")) {
		t.Errorf("expected TP 112.1, got %s", newTP)
	}
}

func TestRatchetSLTP_LongWideningRejected(t *testing.T) {
	// Agent proposes a negative slAdjustPct, which for a long would move SL DOWN (widening risk) —
	// must be rejected, SL stays put. TP has no such rejection any more (2026-09-04): a negative
	// tpAdjustPct is a legitimate "move the target closer" proposal and applies in full.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("110"))}
	price := dec("105")
	newSL, newTP := RatchetSLTP(o, price, dec("-0.02"), dec("-0.02"))

	if !newSL.Equal(dec("95")) {
		t.Errorf("expected SL unchanged at 95 (widening rejected), got %s", newSL)
	}
	// -2% of 105 = -2.1 -> 110 - 2.1 = 107.9, still above entry so it applies.
	if !newTP.Equal(dec("107.9")) {
		t.Errorf("expected TP moved to 107.9 (TP moves freely now), got %s", newTP)
	}
}

func TestRatchetSLTP_ShortTighteningAllowed(t *testing.T) {
	// Short, entry 100, SL 105, TP 90. Price now 95. Tightening SL down, TP further away (down,
	// more ambitious target for a short).
	o := port.PaperOrder{Side: "sell", EntryPx: dec("100"), SLPx: ptr(dec("105")), TPPx: ptr(dec("90"))}
	price := dec("95")
	newSL, newTP := RatchetSLTP(o, price, dec("0.02"), dec("0.02"))

	// SL moves down by 2% of price (95*0.02=1.9) -> 103.1
	if !newSL.Equal(dec("103.1")) {
		t.Errorf("expected SL 103.1, got %s", newSL)
	}
	// A positive tpAdjustPct moves a short's target further away (down): 90 - 1.9 = 88.1
	if !newTP.Equal(dec("88.1")) {
		t.Errorf("expected TP 88.1, got %s", newTP)
	}
}

func TestRatchetSLTP_ShortWideningRejected(t *testing.T) {
	o := port.PaperOrder{Side: "sell", EntryPx: dec("100"), SLPx: ptr(dec("105")), TPPx: ptr(dec("90"))}
	price := dec("95")
	newSL, newTP := RatchetSLTP(o, price, dec("-0.02"), dec("-0.02"))

	if !newSL.Equal(dec("105")) {
		t.Errorf("expected SL unchanged at 105, got %s", newSL)
	}
	// -2% moves a short's TP closer (up): 90 + 1.9 = 91.9, still below entry so it applies.
	if !newTP.Equal(dec("91.9")) {
		t.Errorf("expected TP moved to 91.9 (TP moves freely now), got %s", newTP)
	}
}

func TestRatchetSLTP_CannotUndoPriorTightening(t *testing.T) {
	// SL already tightened to 99 (from an earlier ratchet step) on a long. A new proposal to move
	// it back down to 96 must be rejected even though 96 > original 95 — the ratchet only allows
	// forward progress relative to the CURRENT stored SL, not the original.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("99")), TPPx: ptr(dec("110"))}
	price := dec("105")
	// -3% of 105 = -3.15, would propose 99 - 3.15 = 95.85, which is less than current 99 -> reject.
	newSL, _ := RatchetSLTP(o, price, dec("-0.03"), dec("0"))
	if !newSL.Equal(dec("99")) {
		t.Errorf("expected SL to stay at ratcheted 99, got %s", newSL)
	}
}

func TestRatchetSLTP_LargeSLAdjustmentAppliesInFull(t *testing.T) {
	// No per-step size cap any more (removed 2026-09-04, explicit operator decision): a proposed
	// 10% adjust applies in full rather than being clamped to a fixed percentage.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("110"))}
	price := dec("100")
	newSL, _ := RatchetSLTP(o, price, dec("0.10"), dec("0"))
	// 10% of 100 = 10 -> 95+10=105
	if !newSL.Equal(dec("105")) {
		t.Errorf("expected SL moved in full to 105, got %s", newSL)
	}
}

func TestRatchetSLTP_LargeTPAdjustmentAppliesInFull(t *testing.T) {
	// Same for TP: a large proposed move that stays on the profitable side of entry applies in
	// full, with no per-step cap.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("110"))}
	price := dec("100")
	newSL, newTP := RatchetSLTP(o, price, dec("0"), dec("0.50"))
	if newSL == nil || !newSL.Equal(dec("95")) {
		t.Errorf("expected SL unchanged at 95, got %v", newSL)
	}
	// 50% of 100 = 50 -> 110+50=160
	if !newTP.Equal(dec("160")) {
		t.Errorf("expected TP moved in full to 160, got %s", newTP)
	}
}

func TestRatchetSLTP_NilSLTPStaysNil(t *testing.T) {
	// An order with no SL/TP set (nil) is not something the ratchet creates — it only tightens an
	// existing one.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100")}
	price := dec("105")
	newSL, newTP := RatchetSLTP(o, price, dec("0.02"), dec("0.02"))
	if newSL != nil {
		t.Errorf("expected nil SL to stay nil, got %v", newSL)
	}
	if newTP != nil {
		t.Errorf("expected nil TP to stay nil, got %v", newTP)
	}
}

func TestRatchetSLTP_TPMayCrossCurrentPrice(t *testing.T) {
	// TP crossing the CURRENT price is allowed (2026-09-04): only entry bounds it now. A proposal
	// that pushes TP below the live price but still above entry applies in full — this used to be
	// rejected, but a target between entry and the live price is a perfectly coherent (if
	// close-to-being-hit) take-profit, not a "loosening" the way crossing entry would be.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("105"))}
	price := dec("102")
	// -4% of 102 = -4.08, proposed TP = 105 - 4.08 = 100.92 — below the live price (102) but still
	// above entry (100), so it applies even though it now sits between entry and the live price.
	_, newTP := RatchetSLTP(o, price, dec("0"), dec("-0.04"))
	if !newTP.Equal(dec("100.92")) {
		t.Errorf("expected TP moved to 100.92 (crossing current price is now allowed), got %s", newTP)
	}
}

// A take-profit must never move past ENTRY, in either direction: touching it would realize a
// loss, so it would not be a take-profit any more. This is the one guard moveTP still enforces,
// independent of the current price (CLAUDE.md's original 2026-08-29 incident on orders 100/110,
// stoch_cross short, entry 2.649: a proposal moved TP toward price and past entry, closing at a
// realized loss under close_reason='tp' — the ratchet-based predecessor of moveTP was built to fix
// this, and the entry check carries over unchanged even though the rest of the ratchet is gone).
func TestRatchetSLTP_ShortTPCannotCrossEntry(t *testing.T) {
	entry := dec("2.649")
	tp := dec("2.62251")
	o := port.PaperOrder{Side: "sell", EntryPx: entry, TPPx: &tp}

	// A large proposal moving the short's TP toward/past entry (negative adjustPct = closer).
	_, newTP := RatchetSLTP(o, dec("2.68"), decimal.Zero, dec("-0.30"))

	if newTP == nil {
		t.Fatal("expected the existing TP to be kept, got nil")
	}
	if newTP.GreaterThanOrEqual(entry) {
		t.Errorf("short's TP must stay below entry %s, got %s (hitting it realizes a loss)", entry, newTP)
	}
}

func TestRatchetSLTP_LongTPCannotCrossEntry(t *testing.T) {
	entry := dec("100")
	tp := dec("110")
	o := port.PaperOrder{Side: "buy", EntryPx: entry, TPPx: &tp}

	// A large proposal moving the long's TP toward/past entry (negative adjustPct = closer).
	_, newTP := RatchetSLTP(o, dec("96"), decimal.Zero, dec("-0.20"))

	if newTP == nil {
		t.Fatal("expected the existing TP to be kept, got nil")
	}
	if newTP.LessThanOrEqual(entry) {
		t.Errorf("long's TP must stay above entry %s, got %s (hitting it realizes a loss)", entry, newTP)
	}
}
