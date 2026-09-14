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
	newSL, newTP := RatchetSLTP(o, price, decimal.Zero, dec("0.02"), dec("0.02"))

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
	newSL, newTP := RatchetSLTP(o, price, decimal.Zero, dec("-0.02"), dec("-0.02"))

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
	newSL, newTP := RatchetSLTP(o, price, decimal.Zero, dec("0.02"), dec("0.02"))

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
	newSL, newTP := RatchetSLTP(o, price, decimal.Zero, dec("-0.02"), dec("-0.02"))

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
	newSL, _ := RatchetSLTP(o, price, decimal.Zero, dec("-0.03"), dec("0"))
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
	newSL, _ := RatchetSLTP(o, price, decimal.Zero, dec("0.10"), dec("0"))
	// 10% of 120 = 12 -> 95+12=107, applied in full and still well below the live price.
	if !newSL.Equal(dec("107")) {
		t.Errorf("expected SL moved in full to 107, got %s", newSL)
	}
}

func TestRatchetSLTP_LargeTPAdjustmentAppliesInFull(t *testing.T) {
	// Same for TP: a large proposed move that stays on the profitable side of entry applies in
	// full, with no per-step cap. This test is about the absence of a PER-STEP cap (CLAUDE.md §29),
	// which is a different constraint from both the distance-from-entry ceiling
	// (TestMoveTP_RejectsTargetBeyondMaxDistance) and the reward:risk bound
	// (TestRatchetSLTP_BoundsTargetAgainstTheStop).
	//
	// The stop is deliberately wide (10.0 from entry) so the resulting 40.0 target is 4:1 and sits
	// inside MaxInTradeTPSLRatio. The original fixture used a 5.0 stop, making the same target 8:1 —
	// which the ratio bound added 2026-09-14 correctly clamps, so the fixture was measuring the new
	// bound rather than the absence of a per-step cap.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("90")), TPPx: ptr(dec("110"))}
	price := dec("100")
	newSL, newTP := RatchetSLTP(o, price, decimal.Zero, dec("0"), dec("0.30"))
	if newSL == nil || !newSL.Equal(dec("90")) {
		t.Errorf("expected SL unchanged at 90, got %v", newSL)
	}
	// 30% of 100 = 30 -> 110+30=140, which is 40% from entry (inside MaxTPDistPct) and 4:1 against
	// the 10.0 stop (inside the ratio bound), so it must apply in full.
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
	_, newTP := RatchetSLTP(o, price, decimal.Zero, dec("0"), dec("0.50"))
	if newTP == nil || !newTP.Equal(dec("110")) {
		t.Errorf("a target beyond the ceiling must leave TP untouched at 110, got %v", newTP)
	}
}

func TestRatchetSLTP_NilSLTPStaysNil(t *testing.T) {
	// An order with no SL/TP set (nil) is not something the ratchet creates — it only tightens an
	// existing one.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100")}
	price := dec("105")
	newSL, newTP := RatchetSLTP(o, price, decimal.Zero, dec("0.02"), dec("0.02"))
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
	_, newTP := RatchetSLTP(o, price, decimal.Zero, dec("0"), dec("-0.04"))

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
	_, newTP := RatchetSLTP(o, price, decimal.Zero, dec("0"), dec("-0.04"))

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

	_, newTP := RatchetSLTP(o, price, decimal.Zero, decimal.Zero, adjustPct)

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
	_, newTP := RatchetSLTP(o, dec("2.68"), decimal.Zero, decimal.Zero, dec("-0.30"))

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
	_, newTP := RatchetSLTP(o, dec("96"), decimal.Zero, decimal.Zero, dec("-0.20"))

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
	newSL, _ := RatchetSLTP(long, price, decimal.Zero, dec("0.10"), dec("0"))
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
	newSL2, _ := RatchetSLTP(short, sPrice, decimal.Zero, dec("0.10"), dec("0"))
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
	newSL, _ := RatchetSLTP(o, dec("120"), decimal.Zero, dec("0.125"), dec("0")) // 95 + 0.125*120 = 110
	if newSL == nil || !newSL.Equal(dec("110")) {
		t.Fatalf("want stop trailed to 110 (past entry, below price), got %v", newSL)
	}
}

// TestRatchetSLTP_BoundsTargetAgainstTheStop reproduces order 3356 (2026-09-14, LINK short): the
// position opened with a correct 0.75% target and the model walked it to 33.5% away over three
// in-trade adjustments — a 152:1 reward:risk on a 5m scalp, which price never reaches.
//
// The open path's MaxTPSLRatio (3:1) did not apply because this is the ADJUSTMENT path, and
// MaxTPDistPct (50% of entry) did not catch it because that bound exists only to stop an unbounded
// walk to infinity (§31.1) and is deliberately far wider than any realistic target.
func TestRatchetSLTP_BoundsTargetAgainstTheStop(t *testing.T) {
	// The real order's numbers, to the digit.
	o := port.PaperOrder{
		Side:    "sell",
		EntryPx: dec("11.351"),
		SLPx:    ptr(dec("11.376365")),
		TPPx:    ptr(dec("11.2658675")), // the sane target it opened with
	}
	price := dec("11.35")

	// The model asks to push the target far below entry, as it did live. For a short,
	// direction=-1, so a POSITIVE adjustPct is what moves the target down and away — an earlier
	// version of this test used a negative one, which multiplies to a target ABOVE entry that
	// moveTP rejects on its own, so the ratio clamp never ran and the test passed with the fix
	// removed. Caught by mutation testing, not by review.
	_, newTP := RatchetSLTP(o, price, decimal.Zero, decimal.Zero, dec("0.30"))
	if newTP == nil {
		t.Fatal("expected a target")
	}

	slDist := o.SLPx.Sub(o.EntryPx).Abs()
	tpDist := newTP.Sub(o.EntryPx).Abs()
	ratio := tpDist.Div(slDist)
	if ratio.GreaterThan(dec("6.01")) {
		t.Errorf("reward:risk %s (tp %s, %s from entry) — must be bounded to %v",
			ratio.Round(1), newTP, tpDist, MaxInTradeTPSLRatio)
	}
	// A short's target must still sit below entry after clamping.
	if !newTP.LessThan(o.EntryPx) {
		t.Errorf("short's target %s is not below entry %s", newTP, o.EntryPx)
	}
}

// A target already within the bound must be left exactly alone — the clamp corrects over-wide
// targets, it does not reshape every proposal the model makes.
func TestRatchetSLTP_LeavesAReasonableTargetAlone(t *testing.T) {
	o := port.PaperOrder{
		Side:    "buy",
		EntryPx: dec("100"),
		SLPx:    ptr(dec("99")),  // 1.0 away
		TPPx:    ptr(dec("102")), // 2.0 away — a 2:1, well inside the bound
	}
	_, newTP := RatchetSLTP(o, dec("100.5"), decimal.Zero, decimal.Zero, dec("0.005"))
	if newTP == nil {
		t.Fatal("expected a target")
	}
	// The move is small and within bounds, so it must be applied rather than clamped back.
	if !newTP.GreaterThan(dec("102")) {
		t.Errorf("a within-bounds widening was blocked: %s", newTP)
	}
	if newTP.Sub(dec("100")).GreaterThan(dec("6")) {
		t.Errorf("target %s exceeds the ratio bound it should not have reached", newTP)
	}
}

// The bound measures against the RATCHETED stop, not the original: a tightened stop means less risk
// is being taken, so the reward leg it can justify shrinks with it.
func TestRatchetSLTP_RatioUsesTheTightenedStop(t *testing.T) {
	o := port.PaperOrder{
		Side:    "buy",
		EntryPx: dec("100"),
		SLPx:    ptr(dec("98")),  // 2.0 away initially
		TPPx:    ptr(dec("110")), // 10.0 away — 5:1 against the ORIGINAL stop, inside the bound
	}
	// Tighten the stop toward price: risk drops to ~1.0, so a 10.0 target becomes 10:1.
	newSL, newTP := RatchetSLTP(o, dec("101"), decimal.Zero, dec("0.01"), decimal.Zero)
	if newSL == nil || newTP == nil {
		t.Fatal("expected both levels")
	}
	slDist := newSL.Sub(o.EntryPx).Abs()
	if !slDist.IsPositive() {
		t.Skip("stop landed at entry; ratio undefined")
	}
	if ratio := newTP.Sub(o.EntryPx).Abs().Div(slDist); ratio.GreaterThan(dec("6.01")) {
		t.Errorf("ratio %s against the tightened stop (sl %s, tp %s) exceeds the bound",
			ratio.Round(2), newSL, newTP)
	}
}

// With no stop there is no ratio, so the target must pass through untouched rather than being
// clamped against an invented distance.
func TestRatchetSLTP_NoStopLeavesTargetUnbounded(t *testing.T) {
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: nil, TPPx: ptr(dec("130"))}
	_, newTP := RatchetSLTP(o, dec("101"), decimal.Zero, decimal.Zero, decimal.Zero)
	if newTP == nil || !newTP.Equal(dec("130")) {
		t.Errorf("target changed with no stop to measure against: %v", newTP)
	}
}

// TestRatchetSL_KeepsAMinimumDistanceFromEntry reproduces the 2026-09-14 wipeout: the model walked
// stops to within ~0.17% of entry — inside ordinary 5m noise — and 27 of 31 stop closes in one
// 45-minute window landed under 0.3%, averaging -$0.16 after 15 minutes. Every position closed and
// the account reached zero open.
//
// SLPriceGapPct bounds the stop against the LIVE PRICE, which prevents the instant close; nothing
// bounded it against ENTRY. conductor.Clamps.MinSLDistPct (0.5%) forbids exactly this at open, so
// like §54.7's ratio cap it was bounding only where the trade STARTS.
func TestRatchetSL_KeepsAMinimumDistanceFromEntry(t *testing.T) {
	// Order 3551's real numbers: ZEC short, entry 1134.03, stop walked to 1136.90 (0.25% away).
	o := port.PaperOrder{
		Side:    "sell",
		EntryPx: dec("1134.03"),
		SLPx:    ptr(dec("1151.04")), // where it opened, ~1.5% away
	}
	minDist := dec("0.005") // the configured 0.5%

	// The model asks to pull the stop right in toward entry.
	newSL, _ := RatchetSLTP(o, dec("1135"), minDist, dec("0.012"), decimal.Zero)
	if newSL == nil {
		t.Fatal("expected a stop")
	}

	distPct := newSL.Sub(o.EntryPx).Abs().Div(o.EntryPx)
	if distPct.LessThan(dec("0.00499")) {
		t.Errorf("stop %s is %s%% from entry — closer than the %s%% floor, which is inside 5m noise",
			newSL, distPct.Mul(dec("100")).Round(3), minDist.Mul(dec("100")))
	}
	// A short's stop must stay above entry while it is still on the losing side.
	if !newSL.GreaterThan(o.EntryPx) {
		t.Errorf("short's stop %s crossed below entry %s without being a profit-lock", newSL, o.EntryPx)
	}
}

// The floor must NOT apply once the stop has crossed entry into profit. Measured on the same window,
// 8 of 31 closes had trailed past entry — that is the trailing mechanic working as intended (§15.4),
// and a profit-locking stop is SUPPOSED to sit near price. Bounding its distance from entry would
// forbid trailing altogether, which is the capability §15.4 exists to provide.
func TestRatchetSL_AllowsTrailingPastEntryIntoProfit(t *testing.T) {
	o := port.PaperOrder{
		Side:    "buy",
		EntryPx: dec("100"),
		SLPx:    ptr(dec("99")),
	}
	// Price has run to 105; the model trails the stop up to 100.2 — past entry, locking in profit,
	// and only 0.2% from entry, which the floor would otherwise forbid.
	newSL, _ := RatchetSLTP(o, dec("105"), dec("0.005"), dec("0.012"), decimal.Zero)
	if newSL == nil {
		t.Fatal("expected a stop")
	}
	if !newSL.GreaterThan(o.EntryPx) {
		t.Errorf("stop %s did not trail past entry %s — the floor blocked a profit-lock", newSL, o.EntryPx)
	}
}

// Zero disables the check, so callers that have no configured floor behave exactly as before.
func TestRatchetSL_ZeroMinDistanceDisablesTheFloor(t *testing.T) {
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95"))}
	newSL, _ := RatchetSLTP(o, dec("101"), decimal.Zero, dec("0.05"), decimal.Zero)
	if newSL == nil {
		t.Fatal("expected a stop")
	}
	// 5% of 101 = 5.05 -> 95+5.05 = 100.05, within 0.05% of entry and allowed with no floor set.
	if newSL.Sub(dec("100")).Abs().GreaterThan(dec("0.5")) {
		t.Errorf("stop %s — with the floor disabled the proposal should apply as-is", newSL)
	}
}
