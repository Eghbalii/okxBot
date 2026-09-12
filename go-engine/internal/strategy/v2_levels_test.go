package strategy

import (
	"testing"

	"github.com/shopspring/decimal"
)

func dd(f float64) decimal.Decimal { return decimal.NewFromFloat(f) }

// TestV2Levels_EnforcesOneToOneFloor tests v2Levels directly rather than through WithParams,
// because ParamSpec's own Min already floors risk_reward at 1 — so the parameter path can never
// deliver a sub-1 value and a test driven through it would pass whether or not this floor exists.
// The floor is kept as defence in depth (a future ParamSpec edit, or a direct caller, could
// supply one), and this is the only honest way to assert it.
func TestV2Levels_EnforcesOneToOneFloor(t *testing.T) {
	for _, side := range []Side{Buy, Sell} {
		sl, tp, ok := v2Levels(side, dd(100), dd(1), dd(1), dd(0.4))
		if !ok {
			t.Fatalf("%s: expected levels", side)
		}
		risk := dd(100).Sub(sl).Abs()
		reward := dd(100).Sub(tp).Abs()
		if reward.LessThan(risk) {
			t.Fatalf("%s: reward %s below risk %s despite the 1:1 floor", side, reward, risk)
		}
	}
}

// TestV2Levels_CapsAbsoluteDistance covers the ZEC case that real data exposed: the RATIO can be
// perfectly sound while the absolute distance is still unreachable, because ATR scales with the
// instrument. A 1.3-ATR stop at 2:1 is ~8% of margin on BTC and was 32% on ZEC at the same 10x.
func TestV2Levels_CapsAbsoluteDistance(t *testing.T) {
	// ATR of 3 on a price of 100 — a very volatile instrument.
	sl, tp, ok := v2Levels(Buy, dd(100), dd(3), dd(1.3), dd(2))
	if !ok {
		t.Fatal("expected levels")
	}
	tpFrac := tp.Sub(dd(100)).Div(dd(100))
	if tpFrac.GreaterThan(dd(0.0201)) {
		t.Fatalf("target %s of price exceeds the 2%% cap", tpFrac)
	}
	// And the cap must not have inverted the trade.
	risk := dd(100).Sub(sl)
	reward := tp.Sub(dd(100))
	if reward.LessThan(risk) {
		t.Fatalf("cap pushed reward %s below risk %s", reward, risk)
	}
}

// TestV2Levels_RejectsImpossibleInputs — a non-positive ATR or entry means the indicator has not
// warmed up, which must produce no signal rather than a nonsense level.
func TestV2Levels_RejectsImpossibleInputs(t *testing.T) {
	cases := []struct {
		name             string
		entry, atr, stop decimal.Decimal
	}{
		{"zero atr", dd(100), dd(0), dd(1)},
		{"zero entry", dd(0), dd(1), dd(1)},
		{"zero stop", dd(100), dd(1), dd(0)},
	}
	for _, c := range cases {
		if _, _, ok := v2Levels(Buy, c.entry, c.atr, c.stop, dd(2)); ok {
			t.Fatalf("%s: expected rejection", c.name)
		}
	}
}

// TestV2Levels_ShortNeverTargetsNegativePrice guards the sign class of bug this codebase has hit
// before (§16.9's TP that ratcheted past entry and closed trades at a loss under close_reason='tp').
func TestV2Levels_ShortNeverTargetsNegativePrice(t *testing.T) {
	// A reward wider than the price itself, on a tiny-priced token.
	_, tp, ok := v2Levels(Sell, dd(0.00001), dd(0.00001), dd(3), dd(4))
	if ok && !tp.IsPositive() {
		t.Fatalf("short target %s is not a real price", tp)
	}
}
