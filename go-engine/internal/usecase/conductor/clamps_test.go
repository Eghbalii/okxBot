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
	got := standard.Apply("buy", dec("100"), Levels{SLPx: px("98"), TPPx: px("106")})
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
	got := standard.Apply("buy", dec("100"), Levels{SLPx: px("99.9")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("99.5")) {
		t.Errorf("SL = %v, want widened to the 0.5%% floor (99.5)", got.SLPx)
	}
}

func TestClamps_TooWideStopIsTightened(t *testing.T) {
	// A 40% stop turns a bounded loss into an account event, especially at high leverage. Pulled
	// in to the 5% ceiling.
	got := standard.Apply("buy", dec("100"), Levels{SLPx: px("60")})
	if got.SLPx == nil || !got.SLPx.Equal(dec("95")) {
		t.Errorf("SL = %v, want tightened to the 5%% ceiling (95)", got.SLPx)
	}
}

func TestClamps_ShortIsMirrored(t *testing.T) {
	// A short's stop sits ABOVE entry and its target below. The same 0.1% too-tight stop must be
	// widened upward, not downward.
	got := standard.Apply("sell", dec("100"), Levels{SLPx: px("100.1"), TPPx: px("94")})
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
	got := standard.Apply("buy", dec("100"), Levels{SLPx: px("98"), TPPx: px("101")})
	if got.TPPx == nil || !got.TPPx.Equal(dec("103")) {
		t.Errorf("TP = %v, want widened to 1.5x the 2%% stop (103)", got.TPPx)
	}
}

func TestClamps_RatioUsesTheClampedStop(t *testing.T) {
	// The model asked for a 40% stop (clamped to 5%) and a 10% target. The ratio must be checked
	// against the stop actually being used, not the rejected one: against the original 40% stop a
	// 10% target looks far too near and would be wrongly widened, when in truth 10% is already 2x
	// the real 5% risk and needs no change.
	got := standard.Apply("buy", dec("100"), Levels{SLPx: px("60"), TPPx: px("110")})
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
	got := standard.Apply("buy", dec("100"), Levels{SLPx: px("105"), TPPx: px("95")})
	if got.SLPx != nil {
		t.Errorf("inverted SL should be dropped, got %v", got.SLPx)
	}
	if got.TPPx != nil {
		t.Errorf("inverted TP should be dropped, got %v", got.TPPx)
	}
}

func TestClamps_NilLevelsPassThrough(t *testing.T) {
	// A model that set no levels is a real case, not an error — the caller keeps the strategy's.
	got := standard.Apply("buy", dec("100"), Levels{})
	if got.SLPx != nil || got.TPPx != nil {
		t.Errorf("nil levels should stay nil, got SL=%v TP=%v", got.SLPx, got.TPPx)
	}
}

func TestClamps_ZeroConfigDisablesEverything(t *testing.T) {
	// An unconfigured Clamps must be a no-op rather than clamping to zero, so a deployment that
	// hasn't set the keys yet behaves exactly as it did before this existed.
	got := Clamps{}.Apply("buy", dec("100"), Levels{SLPx: px("60"), TPPx: px("100.5")})
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
	got := c.Apply("buy", dec("100"), Levels{SLPx: px("99.9"), TPPx: px("100.1")})
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
	got := Clamps{MinSLDistPct: dec("0.5")}.Apply("buy", decimal.Zero, in)
	if got.SLPx != in.SLPx || got.TPPx != in.TPPx {
		t.Error("a non-positive entry price should pass levels through untouched")
	}
}
