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
	//
	// Priced with room above the resulting stop on purpose. This test originally ran at price 100
	// and asserted the stop moved to 105 — i.e. 5% ABOVE the live price, which is not a stop at all
	// but an instant market close, and is precisely the defect that closed orders 2043/2044 seconds
	// after they opened on 2026-09-05. The no-per-step-cap property it exists to check is
	// independent of that, so it is asserted here at a price where a full-size move is still a
	// legitimate stop (see TestRatchetSL_NeverCrossesLivePrice for the boundary itself).
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("130"))}
	price := dec("120")
	newSL, _ := RatchetSLTP(o, price, dec("0.10"), dec("0"))
	// 10% of 120 = 12 -> 95+12=107, applied in full and still well below the live price.
	if !newSL.Equal(dec("107")) {
		t.Errorf("expected SL moved in full to 107, got %s", newSL)
	}
}

func TestRatchetSLTP_LargeTPAdjustmentAppliesInFull(t *testing.T) {
	// Same for TP: a large proposed move that stays on the profitable side of entry applies in
	// full, with no per-step cap. The target chosen here lands inside MaxTPDistPct — this test is
	// about the absence of a PER-STEP cap (CLAUDE.md §29), which is a different constraint from the
	// distance-from-entry ceiling covered by TestMoveTP_RejectsTargetBeyondMaxDistance below.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("110"))}
	price := dec("100")
	newSL, newTP := RatchetSLTP(o, price, dec("0"), dec("0.30"))
	if newSL == nil || !newSL.Equal(dec("95")) {
		t.Errorf("expected SL unchanged at 95, got %v", newSL)
	}
	// 30% of 100 = 30 -> 110+30=140, which is 40% from entry and so within the ceiling.
	if !newTP.Equal(dec("140")) {
		t.Errorf("expected TP moved in full to 140, got %s", newTP)
	}
}

// The distance ceiling itself: a proposal landing beyond MaxTPDistPct of entry is REJECTED (the
// existing target is kept) rather than clamped to the boundary. Clamping would accept every
// runaway proposal at the limit, which reads as a model that converged on a 50%-away target
// instead of one whose output is being discarded — see moveTP's comment.
func TestMoveTP_RejectsTargetBeyondMaxDistance(t *testing.T) {
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("110"))}
	price := dec("100")
	// 110 + 50 = 160, i.e. 60% from entry — past the 50% ceiling.
	_, newTP := RatchetSLTP(o, price, dec("0"), dec("0.50"))
	if newTP == nil || !newTP.Equal(dec("110")) {
		t.Errorf("a target beyond the ceiling must leave TP untouched at 110, got %v", newTP)
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

// A target may be pulled in toward the live price, but never PAST it. Between 2026-09-04 and
// 2026-09-07 this test asserted the opposite — that landing TP between entry and the live price was
// "a perfectly coherent (if close-to-being-hit) take-profit" — and order 2595 showed what that
// costs in practice: a target parked behind the market is not close to being hit, it is already
// hit, and the next tick closes the trade there regardless of how far the move had run.
func TestRatchetSLTP_TPClampedToLivePriceNotPastIt(t *testing.T) {
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("105"))}
	price := dec("102")

	// -4% of 102 = -4.08, so the raw proposal is 105 - 4.08 = 100.92 — above entry (100) but BELOW
	// the live price (102), i.e. already passed. It is clamped up to just beyond price instead.
	_, newTP := RatchetSLTP(o, price, dec("0"), dec("-0.04"))

	if newTP == nil {
		t.Fatal("expected a TP, got nil")
	}
	if !newTP.GreaterThan(price) {
		t.Errorf("long's TP must stay above the live price %s, got %s (already touched)", price, newTP)
	}
	// Clamped to price + TPPriceGapPct, not to price itself: a target exactly at price is still
	// touched by the next tick.
	want := dec("102.102") // 102 * 1.001
	if !newTP.Equal(want) {
		t.Errorf("expected TP clamped to %s, got %s", want, newTP)
	}
}

// The short-side mirror of the above.
func TestRatchetSLTP_ShortTPClampedToLivePriceNotPastIt(t *testing.T) {
	o := port.PaperOrder{Side: "sell", EntryPx: dec("100"), SLPx: ptr(dec("105")), TPPx: ptr(dec("95"))}
	price := dec("98")

	// For a short, "pulling in" raises the target. Raw proposal: 95 + 0.04*98 = 98.92, which is
	// above the live price (98) and therefore already passed.
	_, newTP := RatchetSLTP(o, price, dec("0"), dec("-0.04"))

	if newTP == nil {
		t.Fatal("expected a TP, got nil")
	}
	if !newTP.LessThan(price) {
		t.Errorf("short's TP must stay below the live price %s, got %s (already touched)", price, newTP)
	}
	want := dec("97.902") // 98 * 0.999
	if !newTP.Equal(want) {
		t.Errorf("expected TP clamped to %s, got %s", want, newTP)
	}
}

// The exact production incident, reproduced with order 2595's real numbers (PUMP long, 2026-09-07).
// The model pulled the target from 0.004664 to 0.004322 while price was 0.004349 and the trade was
// +6.5% unrealized; the trade closed 39 seconds after opening for a fraction of what it reached.
// Every guard that existed at the time passed it: the target was above entry, and well inside
// MaxTPDistPct. Only the live price ruled it out.
func TestRatchetSLTP_Order2595_TargetNotParkedBehindMarket(t *testing.T) {
	entry := dec("0.004315")
	tp := dec("0.004663795655012131")
	price := dec("0.004349")
	o := port.PaperOrder{Side: "buy", EntryPx: entry, TPPx: &tp}

	// The adjustment that actually ran: enough to land the target at 0.0043224, under price.
	adjustPct := dec("0.004322391895651816975").Sub(tp).Div(price)

	_, newTP := RatchetSLTP(o, price, decimal.Zero, adjustPct)

	if newTP == nil {
		t.Fatal("expected a TP, got nil")
	}
	if newTP.LessThanOrEqual(price) {
		t.Errorf("TP %s is at or below the live price %s — this is the order-2595 bug: the target "+
			"is already touched and the position closes on the next tick", newTP, price)
	}
	if newTP.LessThanOrEqual(entry) {
		t.Errorf("TP %s must stay above entry %s", newTP, entry)
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

// TestMoveTP_CannotWalkTargetAwayForever reproduces the CLAUDE.md §31.1 incident: order 1851
// (ZEC, sell, entry 955.96) had its TP marched from 936 to -566552 across 19 adjustments in six
// minutes, roughly doubling each step. Each individual move was legal — every one left the target
// on the profitable side of entry — because the only guard bounded direction, not distance, while
// moveTP runs off the tick stream and re-applies whatever bias the model has every couple of
// seconds. MaxTPDistPct bounds the distance so the walk terminates.
func TestMoveTP_CannotWalkTargetAwayForever(t *testing.T) {
	entry := decimal.RequireFromString("955.96")
	price := decimal.RequireFromString("955.96")
	direction := decimal.NewFromInt(-1) // short
	tp := decimal.RequireFromString("936.0")

	current := &tp
	for i := 0; i < 50; i++ {
		// A persistently-biased model asking to push the target further away, every single call.
		current = moveTP(current, direction, price, entry, decimal.RequireFromString("0.02"))
	}

	dist := current.Sub(entry).Abs()
	maxDist := entry.Mul(decimal.NewFromFloat(MaxTPDistPct))
	if dist.GreaterThan(maxDist) {
		t.Errorf("TP walked to %s, %s from entry — beyond the %s ceiling; the §31.1 runaway is back",
			current, dist, maxDist)
	}
	if current.GreaterThanOrEqual(entry) {
		t.Errorf("short TP %s must stay below entry %s", current, entry)
	}
}

// A single large, legitimate move must still be allowed — the point of §29's change was that the
// model can reprice a target decisively in one step. Only the unbounded walk is prohibited.
func TestMoveTP_AllowsOneLargeLegitimateMove(t *testing.T) {
	entry := decimal.NewFromInt(100)
	price := decimal.NewFromInt(100)
	tp := decimal.NewFromInt(102)

	got := moveTP(&tp, decimal.NewFromInt(1), price, entry, decimal.RequireFromString("0.10"))
	if got == nil || !got.Equal(decimal.NewFromInt(112)) {
		t.Fatalf("want a single 10%% move to 112, got %v", got)
	}
}

// TestRatchetSL_NeverCrossesLivePrice reproduces the 2026-09-05 incident: order 2043 (TRUMP long,
// entry 2.388) had its stop ratcheted from 2.3737 up to 2.5846 — past entry AND past the live
// price AND past its own take-profit — and closed at market 0 seconds later for a loss. Trailing
// past entry is intended; landing beyond the live price is not, because such a stop is already
// touched the moment it is written.
func TestRatchetSL_NeverCrossesLivePrice(t *testing.T) {
	// An over-reaching proposal is clamped to just inside price, not rejected (2026-09-06): the
	// stop still MOVES — the model keeps control of it — it just cannot land somewhere that is
	// already touched. Rejecting outright left the model with no influence over the stop at all
	// (measured: 5 SL adjustments vs 306 TP ones).
	long := port.PaperOrder{Side: "buy", EntryPx: dec("2.388"), SLPx: ptr(dec("2.3737")), TPPx: ptr(dec("2.4095"))}
	price := dec("2.388")
	newSL, _ := RatchetSLTP(long, price, dec("0.10"), dec("0"))
	if newSL == nil {
		t.Fatal("long stop should have moved, got nil")
	}
	if newSL.GreaterThanOrEqual(price) {
		t.Errorf("long stop %s is at/above live price %s — it would trigger instantly", newSL, price)
	}
	if !newSL.GreaterThan(dec("2.3737")) {
		t.Errorf("long stop should still have tightened from 2.3737 toward price, got %s", newSL)
	}

	// Short: mirrored — clamped to just above price, never at or below it.
	short := port.PaperOrder{Side: "sell", EntryPx: dec("0.09308"), SLPx: ptr(dec("0.09449")), TPPx: ptr(dec("0.09097"))}
	sPrice := dec("0.09308")
	newSL2, _ := RatchetSLTP(short, sPrice, dec("0.10"), dec("0"))
	if newSL2 == nil {
		t.Fatal("short stop should have moved, got nil")
	}
	if newSL2.LessThanOrEqual(sPrice) {
		t.Errorf("short stop %s is at/below live price %s — it would trigger instantly", newSL2, sPrice)
	}
	if !newSL2.LessThan(dec("0.09449")) {
		t.Errorf("short stop should still have tightened from 0.09449 toward price, got %s", newSL2)
	}
}

// The legitimate case must still work: a modest tightening that stays on the protective side of
// price is applied in full, including when it crosses entry to lock in profit.
func TestRatchetSL_StillTrailsIntoProfitBelowPrice(t *testing.T) {
	// Long entered at 100, price has run to 120; moving the stop to 110 locks in profit and is
	// still safely below price.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("130"))}
	newSL, _ := RatchetSLTP(o, dec("120"), dec("0.125"), dec("0")) // 95 + 0.125*120 = 110
	if newSL == nil || !newSL.Equal(dec("110")) {
		t.Fatalf("want stop trailed to 110 (past entry, below price), got %v", newSL)
	}
}
