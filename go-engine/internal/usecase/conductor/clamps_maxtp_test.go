package conductor

import (
	"testing"

	"github.com/shopspring/decimal"
)

func d(f float64) decimal.Decimal { return decimal.NewFromFloat(f) }

func ptr(v decimal.Decimal) *decimal.Decimal { return &v }

// prodClamps mirrors the deployed paper/real configuration.
func prodClamps() Clamps {
	return Clamps{
		MinSLDistPct: d(0.002),
		MaxSLDistPct: d(0.05),
		MaxLossPct:   d(0.15),
		MinTPSLRatio: d(1.0),
		MaxTPSLRatio: d(3),
	}
}

// TestApply_CapsRunawayTPRatio reproduces production order 2549 (ict_fvg, BTC-like entry): a
// structurally tight stop widened to the floor, with a target 6.5% away — an effective 65% of
// margin at 10x, which price on a 5m bar does not reach.
func TestApply_CapsRunawayTPRatio(t *testing.T) {
	cl := prodClamps()
	entry := d(100)
	// stop 0.011% away (the gap edge), target 6.5% away — the real shape from production.
	in := Levels{SLPx: ptr(d(99.989)), TPPx: ptr(d(106.5))}

	out := cl.Apply("buy", entry, d(10), in)

	if out.SLPx == nil || out.TPPx == nil {
		t.Fatalf("expected both levels, got %+v", out)
	}
	slDist := entry.Sub(*out.SLPx)
	tpDist := out.TPPx.Sub(entry)
	ratio := tpDist.Div(slDist)

	if ratio.GreaterThan(d(3.0001)) {
		t.Fatalf("TP:SL ratio %s exceeds the 3:1 cap (tp=%s sl=%s)", ratio, out.TPPx, out.SLPx)
	}
	// And in margin terms it must now be reachable rather than 65%.
	marginPct := tpDist.Div(entry).Mul(d(100)).Mul(d(10))
	if marginPct.GreaterThan(d(20)) {
		t.Fatalf("TP still %s%% of margin away, want <=20%%", marginPct)
	}
}

// TestApply_NeverLetsTPFallBelowSL is the operator's explicit 2026-09-12 requirement: the target's
// own percentage must never be smaller than the stop's, i.e. at least 1:1.
func TestApply_NeverLetsTPFallBelowSL(t *testing.T) {
	cl := prodClamps()
	entry := d(100)
	// A target much NEARER than the stop — must be pushed out to at least 1:1, not capped down.
	in := Levels{SLPx: ptr(d(99)), TPPx: ptr(d(100.1))}

	for _, side := range []string{"buy", "sell"} {
		lv := in
		if side == "sell" {
			lv = Levels{SLPx: ptr(d(101)), TPPx: ptr(d(99.9))}
		}
		out := cl.Apply(side, entry, d(10), lv)
		if out.SLPx == nil || out.TPPx == nil {
			t.Fatalf("%s: expected both levels", side)
		}
		slDist := entry.Sub(*out.SLPx).Abs()
		tpDist := out.TPPx.Sub(entry).Abs()
		if tpDist.LessThan(slDist) {
			t.Fatalf("%s: TP distance %s is below SL distance %s — violates the 1:1 floor", side, tpDist, slDist)
		}
	}
}

// TestApply_LeavesReasonableRatiosUntouched guards against over-correction: a strategy's own 2:1
// is inside both bounds and must pass through unchanged.
func TestApply_LeavesReasonableRatiosUntouched(t *testing.T) {
	cl := prodClamps()
	entry := d(100)
	in := Levels{SLPx: ptr(d(99)), TPPx: ptr(d(102))} // exactly 2:1, stop 1% (inside bounds)

	out := cl.Apply("buy", entry, d(10), in)

	if !out.SLPx.Equal(d(99)) {
		t.Fatalf("SL moved: got %s want 99", out.SLPx)
	}
	if !out.TPPx.Equal(d(102)) {
		t.Fatalf("TP moved: got %s want 102", out.TPPx)
	}
}

// TestApply_ShortSideCapsToo — the asymmetry bugs in this codebase's history (§16.9's inverted TP)
// were all sign errors, so the short path is asserted independently rather than assumed to mirror.
func TestApply_ShortSideCapsToo(t *testing.T) {
	cl := prodClamps()
	entry := d(100)
	in := Levels{SLPx: ptr(d(100.011)), TPPx: ptr(d(93.5))}

	out := cl.Apply("sell", entry, d(10), in)

	slDist := out.SLPx.Sub(entry)
	tpDist := entry.Sub(*out.TPPx)
	if !tpDist.IsPositive() {
		t.Fatalf("short TP must sit below entry, got %s", out.TPPx)
	}
	if ratio := tpDist.Div(slDist); ratio.GreaterThan(d(3.0001)) {
		t.Fatalf("short TP:SL ratio %s exceeds cap", ratio)
	}
}

// TestApply_ZeroMaxTPSLRatioDisablesCap — zero-valued fields disable their clamp, per the struct's
// own documented contract.
func TestApply_ZeroMaxTPSLRatioDisablesCap(t *testing.T) {
	cl := prodClamps()
	cl.MaxTPSLRatio = decimal.Zero
	entry := d(100)
	in := Levels{SLPx: ptr(d(99)), TPPx: ptr(d(150))}

	out := cl.Apply("buy", entry, d(10), in)

	if !out.TPPx.Equal(d(150)) {
		t.Fatalf("cap should be disabled at zero, TP got moved to %s", out.TPPx)
	}
}
