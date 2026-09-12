package conductor

import "testing"

// TestApply_CapsNonV2StrategiesToo proves the clamp protects strategies that were NOT rewritten.
// stepped_trailing is an original (not one of the twelve V2s) and its open positions carried TPs
// at 79-98% of margin at 10x before this fix — the same unreachable-target failure. The clamp is
// strategy-agnostic by construction, and this asserts it rather than assuming it.
func TestApply_CapsNonV2StrategiesToo(t *testing.T) {
	cl := Clamps{
		MinSLDistPct: d(0.005),
		MaxSLDistPct: d(0.05),
		MaxLossPct:   d(0.15),
		MinTPSLRatio: d(1.5),
		MaxTPSLRatio: d(3),
	}
	entry := d(100)
	// Production order 3126's shape: a 1.5% stop with a target ~9.8% away (98% of margin at 10x).
	in := Levels{SLPx: ptr(d(98.5)), TPPx: ptr(d(109.83))}

	out := cl.Apply("buy", entry, d(10), in)

	slDist := entry.Sub(*out.SLPx)
	tpDist := out.TPPx.Sub(entry)
	if ratio := tpDist.Div(slDist); ratio.GreaterThan(d(3.0001)) {
		t.Fatalf("ratio %s exceeds cap", ratio)
	}
	marginPct := tpDist.Div(entry).Mul(d(100)).Mul(d(10))
	if marginPct.GreaterThan(d(50)) {
		t.Fatalf("target still %s%% of margin — was 98%% before the fix", marginPct.StringFixed(1))
	}
	t.Logf("stepped_trailing-shaped order: 98%% of margin -> %s%%", marginPct.StringFixed(1))
}
