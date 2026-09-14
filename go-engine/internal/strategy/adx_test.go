package strategy

import (
	"testing"

	"github.com/shopspring/decimal"
)

// ADX must read HIGH on a clean trend and LOW on chop.
//
// That property is the whole reason it was added: screening 41 strategies found none of them asking
// "is the market going anywhere at all", and a breakout pattern in a chopping market is noise no
// matter how cleanly it forms. An ADX that cannot separate the two would make every regime-adaptive
// strategy built on it a pass-through wearing a filter's name.
func TestADX_SeparatesTrendFromChop(t *testing.T) {
	trend := make([]Candle, 80)
	px := 100.0
	for i := range trend {
		px *= 1.004
		c := decimal.NewFromFloat(px)
		trend[i] = Candle{
			Open:  c.Mul(decimal.NewFromFloat(0.999)),
			High:  c.Mul(decimal.NewFromFloat(1.002)),
			Low:   c.Mul(decimal.NewFromFloat(0.998)),
			Close: c, Volume: decimal.NewFromInt(100),
		}
	}
	trendADX, err := ADX(trend, 14)
	if err != nil {
		t.Fatalf("trend: %v", err)
	}

	chop := make([]Candle, 80)
	for i := range chop {
		v := 100.0
		if i%2 == 0 {
			v = 100.5
		}
		c := decimal.NewFromFloat(v)
		chop[i] = Candle{
			Open:  c,
			High:  c.Mul(decimal.NewFromFloat(1.002)),
			Low:   c.Mul(decimal.NewFromFloat(0.998)),
			Close: c, Volume: decimal.NewFromInt(100),
		}
	}
	chopADX, err := ADX(chop, 14)
	if err != nil {
		t.Fatalf("chop: %v", err)
	}

	if !trendADX.GreaterThan(chopADX) {
		t.Errorf("a clean trend must read higher than chop: trend=%s chop=%s", trendADX, chopADX)
	}
	// The conventional reading is >25 trending, <20 ranging. A sustained one-directional move should
	// clear the first comfortably, or the scale is wrong rather than merely the ordering.
	if trendADX.LessThan(decimal.NewFromInt(25)) {
		t.Errorf("a sustained one-directional move should read well above 25, got %s", trendADX)
	}
	if chopADX.GreaterThan(decimal.NewFromInt(25)) {
		t.Errorf("a two-bar oscillation should not read as trending, got %s", chopADX)
	}
}

// ADX is direction-agnostic: a downtrend is as much a trend as an uptrend, and a strategy that only
// sees strength in one direction would silently refuse to short.
func TestADX_IsDirectionAgnostic(t *testing.T) {
	up := make([]Candle, 80)
	down := make([]Candle, 80)
	pu, pd := 100.0, 100.0
	for i := range up {
		pu *= 1.004
		pd *= 0.996
		cu, cd := decimal.NewFromFloat(pu), decimal.NewFromFloat(pd)
		up[i] = Candle{Open: cu, High: cu.Mul(decimal.NewFromFloat(1.002)), Low: cu.Mul(decimal.NewFromFloat(0.998)), Close: cu, Volume: decimal.NewFromInt(100)}
		down[i] = Candle{Open: cd, High: cd.Mul(decimal.NewFromFloat(1.002)), Low: cd.Mul(decimal.NewFromFloat(0.998)), Close: cd, Volume: decimal.NewFromInt(100)}
	}
	a, _ := ADX(up, 14)
	b, _ := ADX(down, 14)
	if a.Sub(b).Abs().GreaterThan(decimal.NewFromInt(5)) {
		t.Errorf("up and down trends of equal strength must read alike: %s vs %s", a, b)
	}
}

// +DI and -DI must point the right way, or a strategy taking direction from them trades backwards.
func TestDirectionalIndex_PointsTheRightWay(t *testing.T) {
	up := make([]Candle, 60)
	px := 100.0
	for i := range up {
		px *= 1.005
		c := decimal.NewFromFloat(px)
		up[i] = Candle{Open: c, High: c.Mul(decimal.NewFromFloat(1.002)), Low: c.Mul(decimal.NewFromFloat(0.998)), Close: c, Volume: decimal.NewFromInt(100)}
	}
	plus, minus, err := DirectionalIndex(up, 14)
	if err != nil {
		t.Fatalf("DirectionalIndex: %v", err)
	}
	if !plus.GreaterThan(minus) {
		t.Errorf("in an uptrend +DI must exceed -DI, got +%s -%s", plus, minus)
	}
}

// A window too short to warm BOTH smoothing passes must error rather than return a number computed
// from a partial warm-up — the same discipline v8's observation builders follow.
func TestADX_RefusesAShortWindow(t *testing.T) {
	short := make([]Candle, 10)
	for i := range short {
		c := decimal.NewFromInt(100)
		short[i] = Candle{Open: c, High: c, Low: c, Close: c, Volume: decimal.NewFromInt(1)}
	}
	if _, err := ADX(short, 14); err == nil {
		t.Error("expected an error on a window too short for two smoothing passes")
	}
}
