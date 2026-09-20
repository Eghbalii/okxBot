package conductor

import (
	"testing"

	"github.com/shopspring/decimal"
)

// The exact production scenario the operator described (2026-09-20): a $20 cross-margin trading
// cap, a stop distance that MaxLossPct's own margin-relative math would call safe, but which — at
// this position's actual margin and leverage — would realize far more than the $20 cap if touched.
// $20 margin at 10x, stop 10% away: loss = 20 * 10 * 0.10 = $20, exactly the cap. A stop any wider
// must be tightened; this one is already exactly at the boundary and must NOT be touched (a clamp
// that fires exactly at its own boundary would be off-by-one in the wrong, unsafe direction if it
// fired past it instead of at it — this asserts the boundary itself is inclusive of "safe").
func TestClampSLToCapUSD_ExactlyAtCapIsLeftAlone(t *testing.T) {
	sl := px("90") // 10% below a 100 entry, long
	clamped, tightened := ClampSLToCapUSD("buy", dec("100"), dec("20"), dec("10"), dec("20"), sl)
	if tightened {
		t.Fatalf("a stop exactly at the cap boundary was tightened; want it left alone: %v", clamped)
	}
	if !clamped.Equal(dec("90")) {
		t.Fatalf("SL = %s, want unchanged 90", clamped)
	}
}

// A stop wider than the cap allows must be pulled in to exactly the cap-safe distance — this is the
// real production scenario: $20 cap, cross margin, a stop that alone realizes the whole account.
func TestClampSLToCapUSD_TightensAStopThatWouldExceedTheCap(t *testing.T) {
	// $20 margin at 20x leverage: a naive 50%-of-price stop would realize 20*20*0.5 = $200 — ten
	// times the $20 cap. The cap-safe distance is capUSD/(marginUSD*leverage) = 20/(20*20) = 0.05 =
	// 5% of entry, so a long's stop must land at 95, not the proposed 50.
	sl := px("50")
	clamped, tightened := ClampSLToCapUSD("buy", dec("100"), dec("20"), dec("20"), dec("20"), sl)
	if !tightened {
		t.Fatal("expected the stop to be tightened, it was left alone")
	}
	if !clamped.Equal(dec("95")) {
		t.Fatalf("clamped SL = %s, want 95 (5%% of entry, the cap-safe distance)", clamped)
	}
}

// Mirror of the above for a short: the stop sits ABOVE entry, and tightening moves it DOWN toward
// entry (less room for price to move against the position before the cap is reached).
func TestClampSLToCapUSD_TightensAShortStop(t *testing.T) {
	sl := px("150") // 50% above entry
	clamped, tightened := ClampSLToCapUSD("sell", dec("100"), dec("20"), dec("20"), dec("20"), sl)
	if !tightened {
		t.Fatal("expected the short's stop to be tightened")
	}
	if !clamped.Equal(dec("105")) {
		t.Fatalf("clamped SL = %s, want 105 (5%% above entry, the cap-safe distance for a short)", clamped)
	}
}

// A stop already tighter than the cap requires must never be WIDENED by this guard — it only ever
// tightens, matching every other clamp's own "never make a proposal more dangerous" rule.
func TestClampSLToCapUSD_NeverWidensAnAlreadySafeStop(t *testing.T) {
	sl := px("99") // 1% away — far tighter than the ~10% the cap alone would allow
	clamped, tightened := ClampSLToCapUSD("buy", dec("100"), dec("20"), dec("10"), dec("20"), sl)
	if tightened {
		t.Fatalf("an already-tight stop was moved: %v", clamped)
	}
	if !clamped.Equal(dec("99")) {
		t.Fatalf("SL = %s, want unchanged 99", clamped)
	}
}

// No cap configured (capUSD zero — the "disabled" convention every other Clamps field also uses)
// must leave the stop untouched, not treat zero as "zero dollars of allowed loss".
func TestClampSLToCapUSD_ZeroCapDisablesTheGuard(t *testing.T) {
	sl := px("50")
	clamped, tightened := ClampSLToCapUSD("buy", dec("100"), dec("20"), dec("20"), decimal.Zero, sl)
	if tightened {
		t.Fatal("a zero cap (disabled) must not tighten anything")
	}
	if !clamped.Equal(dec("50")) {
		t.Fatalf("SL = %s, want unchanged 50", clamped)
	}
}

// A nil stop (none proposed) must pass through unchanged and unflagged — this function only
// tightens a level that already exists; supplying a missing one is EnsureStop's job.
func TestClampSLToCapUSD_NilStopPassesThrough(t *testing.T) {
	clamped, tightened := ClampSLToCapUSD("buy", dec("100"), dec("20"), dec("10"), dec("20"), nil)
	if tightened {
		t.Fatal("nil stop must never report tightened=true")
	}
	if clamped != nil {
		t.Fatalf("clamped = %v, want nil", clamped)
	}
}

// A stop already on the wrong side of entry (Apply's own "wrong side of entry" case) is not this
// function's job to fix — signedDist returns non-positive, and the function must leave it alone
// rather than attempting cap math against a level that was never coherent to begin with.
func TestClampSLToCapUSD_WrongSideOfEntryLeftAlone(t *testing.T) {
	sl := px("110") // above entry, wrong side for a long's stop
	clamped, tightened := ClampSLToCapUSD("buy", dec("100"), dec("20"), dec("10"), dec("20"), sl)
	if tightened {
		t.Fatal("a wrong-side stop must not be reported as tightened")
	}
	if !clamped.Equal(dec("110")) {
		t.Fatalf("SL = %s, want unchanged 110 (not this function's job to fix)", clamped)
	}
}

// Non-positive margin/leverage must disable the guard rather than divide by zero or produce a
// nonsensical clamp — the same defensive posture maxSLDistPctFor takes for leverage, but here
// leverage/margin being unknown means the dollar-loss math cannot be trusted at all, so this
// function refuses rather than guessing 1x the way maxSLDistPctFor does.
func TestClampSLToCapUSD_NonPositiveMarginOrLeverageDisablesTheGuard(t *testing.T) {
	sl := px("50")
	if clamped, tightened := ClampSLToCapUSD("buy", dec("100"), decimal.Zero, dec("10"), dec("20"), sl); tightened || !clamped.Equal(dec("50")) {
		t.Fatalf("zero margin: got clamped=%v tightened=%v, want unchanged/false", clamped, tightened)
	}
	if clamped, tightened := ClampSLToCapUSD("buy", dec("100"), dec("20"), decimal.Zero, dec("20"), sl); tightened || !clamped.Equal(dec("50")) {
		t.Fatalf("zero leverage: got clamped=%v tightened=%v, want unchanged/false", clamped, tightened)
	}
}
