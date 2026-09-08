package conductor

import (
	"testing"

	"github.com/shopspring/decimal"
)

func px(s string) *decimal.Decimal {
	d := dec(s)
	return &d
}

// standard is a representative clamp set: stop between 0.5% and 5% of entry, target at least 1.5x
// the stop distance.
var standard = Clamps{
	MinSLDistPct: dec("0.005"),
	MaxSLDistPct: dec("0.05"),
	MinTPSLRatio: dec("1.5"),
}

func TestClamps_LongWithinBounds(t *testing.T) {
	// Entry 100, stop at 98 (2%), target at 106 (6% = 3x the stop). Everything is already legal,
	// so nothing should move — a clamp that adjusts compliant values would silently override the
	// model's real decision.
	got := standard.Apply("buy", dec("100"), decimal.Zero, Levels{SLPx: px("98"), TPPx: px("106")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("98")) {
		t.Errorf("SL = %v, want unchanged 98", got.SLPx)
	}
	if got.TPPx == nil || !got.TPPx.Equal(dec("106")) {
		t.Errorf("TP = %v, want unchanged 106", got.TPPx)
	}
}

func TestClamps_TooTightStopIsWidened(t *testing.T) {
	// A stop 0.1% from entry stops out on ordinary noise before the trade can do anything. Pushed
	// out to the 0.5% floor.
	got := standard.Apply("buy", dec("100"), decimal.Zero, Levels{SLPx: px("99.9")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("99.5")) {
		t.Errorf("SL = %v, want widened to the 0.5%% floor (99.5)", got.SLPx)
	}
}

func TestClamps_TooWideStopIsTightened(t *testing.T) {
	// A 40% stop turns a bounded loss into an account event, especially at high leverage. Pulled
	// in to the 5% ceiling.
	got := standard.Apply("buy", dec("100"), decimal.Zero, Levels{SLPx: px("60")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("95")) {
		t.Errorf("SL = %v, want tightened to the 5%% ceiling (95)", got.SLPx)
	}
}

func TestClamps_ShortIsMirrored(t *testing.T) {
	// A short's stop sits ABOVE entry and its target below. The same 0.1% too-tight stop must be
	// widened upward, not downward.
	got := standard.Apply("sell", dec("100"), decimal.Zero, Levels{SLPx: px("100.1"), TPPx: px("94")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("100.5")) {
		t.Errorf("short SL = %v, want 100.5", got.SLPx)
	}
	if got.TPPx == nil || !got.TPPx.Equal(dec("94")) {
		t.Errorf("short TP = %v, want unchanged 94", got.TPPx)
	}
}

func TestClamps_TargetWidenedToMeetRatio(t *testing.T) {
	// Stop 2% away, target only 1% away: risking 2 to make 1 is negative expectancy by
	// construction, no matter how good the entry. The target is pushed out to 1.5x the stop.
	got := standard.Apply("buy", dec("100"), decimal.Zero, Levels{SLPx: px("98"), TPPx: px("101")})
	if got.TPPx == nil || !got.TPPx.Equal(dec("103")) {
		t.Errorf("TP = %v, want widened to 1.5x the 2%% stop (103)", got.TPPx)
	}
}

func TestClamps_RatioUsesTheClampedStop(t *testing.T) {
	// The model asked for a 40% stop (clamped to 5%) and a 10% target. The ratio must be checked
	// against the stop actually being used, not the rejected one: against the original 40% stop a
	// 10% target looks far too near and would be wrongly widened, when in truth 10% is already 2x
	// the real 5% risk and needs no change.
	got := standard.Apply("buy", dec("100"), decimal.Zero, Levels{SLPx: px("60"), TPPx: px("110")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("95")) {
		t.Fatalf("SL = %v, want 95", got.SLPx)
	}
	if got.TPPx == nil || !got.TPPx.Equal(dec("110")) {
		t.Errorf("TP = %v, want unchanged 110 (already 2x the clamped 5%% stop)", got.TPPx)
	}
}

func TestClamps_InvertedLevelsAreDropped(t *testing.T) {
	// A long whose "stop" sits above entry and whose "target" sits below is incoherent. Guessing
	// the intent would invent a decision the model never made, so both are dropped and the caller
	// falls back to the strategy's own levels.
	got := standard.Apply("buy", dec("100"), decimal.Zero, Levels{SLPx: px("105"), TPPx: px("95")})
	if got.SLPx != nil {
		t.Errorf("inverted SL should be dropped, got %v", got.SLPx)
	}
	if got.TPPx != nil {
		t.Errorf("inverted TP should be dropped, got %v", got.TPPx)
	}
}

func TestClamps_NilLevelsPassThrough(t *testing.T) {
	// A model that set no levels is a real case, not an error — the caller keeps the strategy's.
	got := standard.Apply("buy", dec("100"), decimal.Zero, Levels{})
	if got.SLPx != nil || got.TPPx != nil {
		t.Errorf("nil levels should stay nil, got SL=%v TP=%v", got.SLPx, got.TPPx)
	}
}

func TestClamps_ZeroConfigDisablesEverything(t *testing.T) {
	// An unconfigured Clamps must be a no-op rather than clamping to zero, so a deployment that
	// hasn't set the keys yet behaves exactly as it did before this existed.
	got := Clamps{}.Apply("buy", dec("100"), decimal.Zero, Levels{SLPx: px("60"), TPPx: px("100.5")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("60")) {
		t.Errorf("SL = %v, want untouched 60", got.SLPx)
	}
	if got.TPPx == nil || !got.TPPx.Equal(dec("100.5")) {
		t.Errorf("TP = %v, want untouched 100.5", got.TPPx)
	}
}

func TestClamps_PartialConfig(t *testing.T) {
	// Only a minimum stop distance configured: it applies, and the unset ceiling and ratio do not
	// silently clamp to zero.
	c := Clamps{MinSLDistPct: dec("0.005")}
	got := c.Apply("buy", dec("100"), decimal.Zero, Levels{SLPx: px("99.9"), TPPx: px("100.1")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("99.5")) {
		t.Errorf("SL = %v, want 99.5", got.SLPx)
	}
	if got.TPPx == nil || !got.TPPx.Equal(dec("100.1")) {
		t.Errorf("TP = %v, want untouched 100.1 (no ratio configured)", got.TPPx)
	}
}

func TestClamps_NonPositiveEntryIsPassthrough(t *testing.T) {
	// Every clamp is relative to entry price; without a usable entry there is nothing to clamp
	// against, and inventing one would be worse than leaving the levels alone.
	in := Levels{SLPx: px("98"), TPPx: px("106")}
	got := Clamps{MinSLDistPct: dec("0.5")}.Apply("buy", decimal.Zero, decimal.Zero, in)
	if got.SLPx != in.SLPx || got.TPPx != in.TPPx {
		t.Error("a non-positive entry price should pass levels through untouched")
	}
}

// A strategy that proposes only a target must still get a bounded stop (CLAUDE.md §16.9): order 80
// opened with unbounded downside because nothing filled the gap a missing stop leaves.
func TestEnsureStop_FillsAMissingStopAtTheWidestBound(t *testing.T) {
	cl := Clamps{MinSLDistPct: dec("0.005"), MaxSLDistPct: dec("0.05"), MinTPSLRatio: dec("1.5")}

	long := cl.EnsureStop("buy", dec("100"), decimal.Zero, Levels{})
	if long.SLPx == nil || !long.SLPx.Equal(dec("95")) {
		t.Errorf("long stop should sit MaxSLDistPct below entry (95), got %v", long.SLPx)
	}

	short := cl.EnsureStop("sell", dec("100"), decimal.Zero, Levels{})
	if short.SLPx == nil || !short.SLPx.Equal(dec("105")) {
		t.Errorf("short stop should sit MaxSLDistPct above entry (105), got %v", short.SLPx)
	}
}

// An existing stop is the strategy's own decision and must not be overwritten.
func TestEnsureStop_LeavesAnExistingStopAlone(t *testing.T) {
	cl := Clamps{MaxSLDistPct: dec("0.05")}
	own := dec("99")
	got := cl.EnsureStop("buy", dec("100"), decimal.Zero, Levels{SLPx: &own})
	if got.SLPx == nil || !got.SLPx.Equal(dec("99")) {
		t.Errorf("expected the strategy's own stop to survive, got %v", got.SLPx)
	}
}

// MaxLossPct caps the REALIZED loss a stop can produce once leverage is applied — a distinct
// question from MaxSLDistPct, which only bounds the raw price distance (2026-08-31 request: "SL
// should never allow more than 15% loss, at any leverage, no cap on profit").
var withLossCap = Clamps{MinSLDistPct: dec("0.001"), MaxSLDistPct: dec("0.5"), MaxLossPct: dec("0.15")}

func TestClamps_MaxLossPctTightensStopAtHighLeverage(t *testing.T) {
	// At 20x leverage, a 15% loss cap means the price can only move 0.75% before the stop must
	// bind — far tighter than the raw 50% MaxSLDistPct alone would allow.
	got := withLossCap.Apply("buy", dec("100"), dec("20"), Levels{SLPx: px("90")}) // 10% price move requested
	if got.SLPx == nil || !got.SLPx.Equal(dec("99.25")) {
		t.Errorf("SL = %v, want tightened to 0.75%% price distance (99.25) so 20x loss caps at 15%%", got.SLPx)
	}
}

func TestClamps_MaxLossPctDoesNotTightenAt1x(t *testing.T) {
	// At 1x, a 15% loss cap allows a full 15% price move — well within the 50% MaxSLDistPct, so
	// a 10% stop request is unaffected.
	got := withLossCap.Apply("buy", dec("100"), dec("1"), Levels{SLPx: px("90")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("90")) {
		t.Errorf("SL = %v, want unchanged 90 at 1x leverage (10%% loss is within the 15%% cap)", got.SLPx)
	}
}

func TestClamps_MaxLossPctZeroLeverageTreatedAs1x(t *testing.T) {
	// A zero/unset leverage must not be treated as "no leverage limit" (which would disable the
	// cap entirely) or divide-by-zero — it falls back to 1x, matching
	// usecase.unrealizedPnLPct's own defensive fallback.
	got := withLossCap.Apply("buy", dec("100"), decimal.Zero, Levels{SLPx: px("90")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("90")) {
		t.Errorf("SL = %v, want unchanged 90 (zero leverage treated as 1x)", got.SLPx)
	}
}

func TestClamps_MaxLossPctHasNoEffectOnTakeProfit(t *testing.T) {
	// No profit cap exists at any leverage — only the stop side is bounded by MaxLossPct.
	got := withLossCap.Apply("buy", dec("100"), dec("50"), Levels{SLPx: px("99"), TPPx: px("200")})
	if got.TPPx == nil || !got.TPPx.Equal(dec("200")) {
		t.Errorf("TP = %v, want unchanged 200 — profit is never capped regardless of leverage", got.TPPx)
	}
}

func TestEnsureStop_MaxLossPctBoundsTheFilledStop(t *testing.T) {
	// The missing-stop fallback (EnsureStop) must also respect the leverage-aware cap, not just
	// the already-set-stop path (Apply) — a strategy that emits only a target must still get a
	// stop that cannot blow past 15% loss at whatever leverage the position opens with.
	cl := Clamps{MaxSLDistPct: dec("0.5"), MaxLossPct: dec("0.15")}
	got := cl.EnsureStop("buy", dec("100"), dec("20"), Levels{})
	if got.SLPx == nil || !got.SLPx.Equal(dec("99.25")) {
		t.Errorf("filled SL = %v, want 99.25 (0.75%% price distance at 20x, capping loss at 15%%)", got.SLPx)
	}
}

// The first real order (id 3, SOL short) opened with a stop from the model and NO take-profit,
// because the model emitted SLPx but a zero TPPx and nothing downstream re-supplied a target the
// way EnsureStop re-supplies a stop. RealTrader watches SL/TP in-process, so that position could
// only ever end at its stop, at the timeout, or by hand.
func TestEnsureTarget_FillsMissingTargetFromStopDistance(t *testing.T) {
	cl := Clamps{MinTPSLRatio: decimal.NewFromFloat(1.5)}
	entry := decimal.NewFromFloat(104.16)
	sl := decimal.NewFromFloat(104.62) // short: stop above entry

	out := cl.EnsureTarget("sell", entry, Levels{SLPx: &sl})
	if out.TPPx == nil {
		t.Fatal("expected a target to be filled in")
	}
	// stop distance 0.46 * 1.5 = 0.69 below entry for a short.
	want := decimal.NewFromFloat(104.16 - 0.69)
	if out.TPPx.Sub(want).Abs().GreaterThan(decimal.NewFromFloat(0.001)) {
		t.Fatalf("target: want ~%s, got %s", want, out.TPPx)
	}
	if !out.TPPx.LessThan(entry) {
		t.Fatalf("a short's target must sit below entry, got %s vs entry %s", out.TPPx, entry)
	}
}

func TestEnsureTarget_LongSideFillsAboveEntry(t *testing.T) {
	cl := Clamps{MinTPSLRatio: decimal.NewFromFloat(2)}
	entry := decimal.NewFromFloat(100)
	sl := decimal.NewFromFloat(98) // long: stop below entry

	out := cl.EnsureTarget("buy", entry, Levels{SLPx: &sl})
	if out.TPPx == nil || !out.TPPx.Equal(decimal.NewFromFloat(104)) {
		t.Fatalf("long target: want 104 (2*2 above entry), got %v", out.TPPx)
	}
}

// An existing target is never overwritten — this only fills a gap, it does not re-price.
func TestEnsureTarget_LeavesExistingTargetAlone(t *testing.T) {
	cl := Clamps{MinTPSLRatio: decimal.NewFromFloat(1.5)}
	entry := decimal.NewFromFloat(100)
	sl := decimal.NewFromFloat(98)
	tp := decimal.NewFromFloat(101)

	out := cl.EnsureTarget("buy", entry, Levels{SLPx: &sl, TPPx: &tp})
	if out.TPPx == nil || !out.TPPx.Equal(decimal.NewFromFloat(101)) {
		t.Fatalf("existing target must be preserved, got %v", out.TPPx)
	}
}

// With nothing to derive from, inventing a number would be worse than leaving the gap visible.
func TestEnsureTarget_NoStopOrNoRatioLeavesGap(t *testing.T) {
	entry := decimal.NewFromFloat(100)
	sl := decimal.NewFromFloat(98)

	if out := (Clamps{MinTPSLRatio: decimal.NewFromFloat(1.5)}).EnsureTarget("buy", entry, Levels{}); out.TPPx != nil {
		t.Fatalf("no stop to derive from: want nil target, got %v", out.TPPx)
	}
	if out := (Clamps{}).EnsureTarget("buy", entry, Levels{SLPx: &sl}); out.TPPx != nil {
		t.Fatalf("no ratio configured: want nil target, got %v", out.TPPx)
	}
}
