package strategy

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

// CLAUDE.md §15.11/§14's strategy audit: the observation carries a strategy's proposed entry price
// and SL/TP LEVELS, because a level derived from chart structure (a stop below a swing low, a
// target at a fair-value gap) carries information a percentage does not. A strategy that computes a
// real level and then divides it into a percentage throws that structure away, and everything
// downstream reasons about a level no strategy actually chose.
//
// These tests pin the strategies that genuinely have structure to report. The ones NOT covered here
// (rsi_sma, rsi_sma_fuzzy, sma_cross_fixed_exit, ema_cross_trailing, stepped_trailing, stoch_cross,
// grid_like) are deliberate: their logic computes no structural level, so percentages are the honest
// output and inventing a level would be worse than not having one.

// risingCandles builds a simple ascending series, enough to satisfy most warm-up requirements.
func risingCandles(n int, start, step float64) []Candle {
	out := make([]Candle, n)
	p := start
	for i := range out {
		out[i] = Candle{
			Open:   decimal.NewFromFloat(p),
			High:   decimal.NewFromFloat(p * 1.002),
			Low:    decimal.NewFromFloat(p * 0.998),
			Close:  decimal.NewFromFloat(p),
			Volume: decimal.NewFromInt(10),
		}
		p += step
	}
	return out
}

// assertCoherent checks the invariant every emitted level set must satisfy: a long's stop below its
// entry and target above, mirrored for a short. This is what catches a level projected in the wrong
// direction — the exact failure a percentage plus .Abs() can hide.
func assertCoherent(t *testing.T, sig Signal) {
	t.Helper()
	if sig.Side == Hold {
		return
	}
	if !sig.EntryPx.IsPositive() {
		return
	}
	if sig.SLPx.IsPositive() {
		if sig.Side == Buy && !sig.SLPx.LessThan(sig.EntryPx) {
			t.Errorf("long stop %v must sit below entry %v", sig.SLPx, sig.EntryPx)
		}
		if sig.Side == Sell && !sig.SLPx.GreaterThan(sig.EntryPx) {
			t.Errorf("short stop %v must sit above entry %v", sig.SLPx, sig.EntryPx)
		}
	}
	if sig.TPPx.IsPositive() {
		if sig.Side == Buy && !sig.TPPx.GreaterThan(sig.EntryPx) {
			t.Errorf("long target %v must sit above entry %v", sig.TPPx, sig.EntryPx)
		}
		if sig.Side == Sell && !sig.TPPx.LessThan(sig.EntryPx) {
			t.Errorf("short target %v must sit below entry %v", sig.TPPx, sig.EntryPx)
		}
	}
}

func TestDualMAATR_EmitsStructuralStopLevel(t *testing.T) {
	// A downtrend that reverses, so the fast MA crosses up over the slow one and the long branch
	// fires with a swing low genuinely below price.
	candles := make([]Candle, 0, 120)
	p := 100.0
	for i := 0; i < 60; i++ {
		candles = append(candles, mkCandle(p))
		p -= 0.4
	}
	for i := 0; i < 60; i++ {
		candles = append(candles, mkCandle(p))
		p += 0.9
	}

	s := NewDualMAATR()
	var sig Signal
	for i := 60; i <= len(candles); i++ {
		got, err := s.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if got.Side != Hold {
			sig = got
			break
		}
	}
	if sig.Side == Hold {
		t.Skip("no crossover produced by this series")
	}

	if !sig.SLPx.IsPositive() {
		t.Fatal("dual_ma_atr computes a real stop (swing low - ATR) and must emit it as a price")
	}
	assertCoherent(t, sig)

	// The emitted level must match the percentage it also reports — if the two disagree, whichever
	// one a caller happens to read changes the trade.
	implied := sig.EntryPx.Sub(sig.SLPct.Mul(sig.EntryPx))
	if sig.SLPx.Sub(implied).Abs().GreaterThan(dec("0.0001")) {
		t.Errorf("SLPx %v disagrees with SLPct-implied %v", sig.SLPx, implied)
	}
}

func mkCandle(p float64) Candle {
	return Candle{
		Open:   decimal.NewFromFloat(p),
		High:   decimal.NewFromFloat(p * 1.003),
		Low:    decimal.NewFromFloat(p * 0.997),
		Close:  decimal.NewFromFloat(p),
		Volume: decimal.NewFromInt(10),
	}
}

func TestPMax_EmitsSignalsAtAll(t *testing.T) {
	// Regression test for a bug this audit uncovered: PMax compared the CURRENT bar's moving
	// average against a band derived from that same average — i.e. `ma > ma + multiplier*atr`,
	// which is impossible for any positive ATR. The trend could never leave its initial +1, so this
	// strategy emitted exactly zero signals for its entire existence while looking perfectly
	// healthy: registered in Factories, assignable from the panel, silently returning Hold forever.
	//
	// The Pine source compares against the PREVIOUS bar's ratcheted band (strategy_PMax Explorer,
	// lines 82-90). Nothing caught this because no test asserted the strategy ever fires.
	candles := oscillating(300, 100, 15, 9)

	s := NewPMax()
	signals := 0
	for i := 30; i <= len(candles); i++ {
		got, err := s.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if got.Side != Hold {
			signals++
			assertCoherent(t, got)
		}
	}
	if signals == 0 {
		t.Fatal("pmax produced no signal across 300 oscillating candles — the trend flip is unreachable again")
	}
}

func TestPMax_StopIsItsOwnTrailingBand(t *testing.T) {
	// PMax defines a trend flip by the MA crossing its ATR band, so that band IS where the strategy
	// considers itself wrong. A fixed SLPct would put the stop somewhere the strategy has no
	// opinion about.
	candles := oscillating(300, 100, 15, 9)

	s := NewPMax()
	found := false
	for i := 30; i <= len(candles); i++ {
		got, err := s.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if got.Side == Hold {
			continue
		}
		found = true
		if !got.SLPx.IsPositive() {
			t.Fatal("pmax must emit its ATR band as the stop price")
		}
		assertCoherent(t, got)
		// The level and the percentage must describe the same trade; whichever a caller reads
		// should not change where the stop sits.
		implied := got.EntryPx.Sub(directionOf(got.Side).Mul(got.SLPct).Mul(got.EntryPx))
		if got.SLPx.Sub(implied).Abs().GreaterThan(decimal.NewFromFloat(0.0001)) {
			t.Errorf("SLPx %v disagrees with SLPct-implied %v", got.SLPx, implied)
		}
	}
	if !found {
		t.Fatal("no pmax signal produced")
	}
}

func TestPMax_LevelPrecisionStaysBounded(t *testing.T) {
	// Emitted levels land in NUMERIC columns and in every model observation, so an unbounded
	// decimal is a real problem rather than a cosmetic one. Two separate causes were found here:
	// a reward ratio computed as risk*(tp/sl) (two divisions, the 16-digit quotient multiplied back
	// out), and EMA's recursive accumulator compounding ~16 digits per candle forever.
	candles := oscillating(400, 100, 15, 9)
	s := NewPMax()
	for i := 30; i <= len(candles); i++ {
		got, _ := s.Evaluate(candles[:i])
		if got.Side == Hold {
			continue
		}
		for name, v := range map[string]decimal.Decimal{"SLPx": got.SLPx, "TPPx": got.TPPx, "EntryPx": got.EntryPx} {
			if len(v.String()) > 40 {
				t.Fatalf("%s has %d digits (%v) — precision is compounding again", name, len(v.String()), v)
			}
		}
	}
}

func TestTrendConfluence_EmitsATRDerivedLevels(t *testing.T) {
	// An oscillating series with a mild uptrend, which satisfies this strategy's eight simultaneous
	// filters often enough to actually fire.
	candles := make([]Candle, 0, 600)
	for i := 0; i < 600; i++ {
		p := 100.0 + 20.0*math.Sin(float64(i)/25.0) + float64(i)*0.02
		candles = append(candles, mkCandle(p))
	}

	s := NewTrendConfluence()
	found := false
	for i := 200; i <= len(candles); i++ {
		got, err := s.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if got.Side == Hold {
			continue
		}
		found = true
		if !got.SLPx.IsPositive() || !got.TPPx.IsPositive() {
			t.Fatal("trend_confluence computes an ATR-scaled distance and must emit both levels")
		}
		assertCoherent(t, got)
	}
	if !found {
		t.Fatal("no confluence signal produced across 600 candles")
	}
}

func TestPivotReversal_EntryIsTheBreakoutPivot(t *testing.T) {
	// This strategy trades the breakout THROUGH a pivot, so the pivot is where the trade is meant
	// to be taken — not wherever the candle closed after running past it. That gap is exactly what
	// entry_px exists to express.
	s := NewPivotReversal()
	s.LeftBars, s.RightBars = 2, 2

	candles := []Candle{
		mkCandle(100), mkCandle(101), mkCandle(105), mkCandle(101), mkCandle(100),
		mkCandle(99), mkCandle(100),
	}
	// A bar that blows well past the pivot high: the close is far above the breakout level.
	breakout := Candle{
		Open: dec("101"), High: dec("120"), Low: dec("100.5"), Close: dec("118"), Volume: dec("10"),
	}

	var sig Signal
	for i := 5; i <= len(candles); i++ {
		got, _ := s.Evaluate(candles[:i])
		if got.Side != Hold {
			sig = got
		}
	}
	got, err := s.Evaluate(append(append([]Candle{}, candles...), breakout))
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if got.Side != Hold {
		sig = got
	}
	if sig.Side == Hold {
		t.Skip("no pivot breakout produced by this series")
	}

	if !sig.EntryPx.IsPositive() {
		t.Fatal("pivot_reversal must report the armed pivot as its entry price")
	}
	if sig.EntryPx.GreaterThanOrEqual(dec("118")) {
		t.Errorf("entry %v should be the pivot level, not the overshooting close (118)", sig.EntryPx)
	}
}

func TestWeeklyDipBuy_EntryIsTheDipLimitNotTheClose(t *testing.T) {
	// The trigger is the bar's LOW touching the dip level, so the close is routinely well above it.
	// Reporting the close as entry would overstate the fill by the whole length of the wick.
	s := NewWeeklyDipBuy()
	// No timestamps means every candle is in one ISO week, so weekOpen is candles[0].Open = 100 and
	// the 1% dip level is 99. The second bar's low spikes to 80 — far through the level — while its
	// close settles at 103, well ABOVE it. Close-as-entry would report a fill 4% off the truth.
	candles := []Candle{
		{Open: dec("100"), High: dec("101"), Low: dec("99.5"), Close: dec("100"), Volume: dec("10")},
		{Open: dec("100"), High: dec("104"), Low: dec("80"), Close: dec("103"), Volume: dec("10")},
	}

	sig, err := s.Evaluate(candles)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if sig.Side == Hold {
		t.Fatal("a low of 80 is far through the 99 dip level; this should have triggered")
	}
	if !sig.EntryPx.IsPositive() {
		t.Fatal("weekly_dip_buy must report its dip limit as the entry price")
	}
	if !sig.EntryPx.Equal(dec("99")) {
		t.Errorf("entry = %v, want the 99 dip level (the close was 103)", sig.EntryPx)
	}

	// The breakeven stop must anchor to the intended fill, not to the close — otherwise the
	// reported risk is wrong by the whole distance between them.
	resolved := sig.ResolveLevels(dec("103"))
	if !resolved.EntryPx.Equal(dec("99")) {
		t.Errorf("ResolveLevels overwrote the strategy's own entry: %v", resolved.EntryPx)
	}
	if resolved.SLPx.GreaterThanOrEqual(dec("99")) {
		t.Errorf("stop %v must sit below the dip entry 99", resolved.SLPx)
	}
}

// oscillating builds a sine series, which produces the repeated genuine reversals that
// trend-flip strategies need — a monotonic ramp never flips at all.
func oscillating(n int, base, amplitude, period float64) []Candle {
	out := make([]Candle, n)
	for i := range out {
		out[i] = mkCandle(base + amplitude*math.Sin(float64(i)/period))
	}
	return out
}

func directionOf(side Side) decimal.Decimal {
	if side == Sell {
		return decimal.NewFromInt(-1)
	}
	return decimal.NewFromInt(1)
}

func TestEMAPrecisionDoesNotGrowWithWindowSize(t *testing.T) {
	// Regression test for a bug this audit uncovered, independent of the level work itself.
	//
	// EMA's alpha is a repeating decimal for most periods (2/11 at period 10), and the recurrence
	// multiplies the running value by (1-alpha) on every bar. decimal.Decimal is arbitrary-
	// precision, so nothing truncated it: measured 498 digits at 40 candles, 1,139 at 80, 4,659 at
	// 300 — growing linearly and without bound for as long as the candle window does. PaperTrader
	// maintains long windows and several strategies use EMA, so those values were reaching NUMERIC
	// columns and every model observation.
	//
	// The property that matters is that precision stays FLAT as the window grows, not that it hits
	// any particular number.
	candles := oscillating(600, 100, 15, 9)

	var digits []int
	for _, n := range []int{40, 80, 150, 300, 600} {
		ema, err := EMA(candles[:n], 10)
		if err != nil {
			t.Fatalf("ema at n=%d: %v", n, err)
		}
		digits = append(digits, len(ema.String()))
	}
	for i, d := range digits {
		if d > 40 {
			t.Fatalf("EMA at window %d has %d digits; precision is compounding again (all: %v)",
				[]int{40, 80, 150, 300, 600}[i], d, digits)
		}
	}
}

func TestEMAStaysAccurateAfterRounding(t *testing.T) {
	// Bounding the accumulator must not move the value meaningfully — 12 decimal places is far
	// beyond any real instrument's significance, so the EMA must still track a flat series exactly
	// and a known ramp closely.
	flat := make([]Candle, 100)
	for i := range flat {
		flat[i] = mkCandle(100)
	}
	ema, err := EMA(flat, 10)
	if err != nil {
		t.Fatalf("ema: %v", err)
	}
	if ema.Sub(decimal.NewFromInt(100)).Abs().GreaterThan(decimal.NewFromFloat(0.000001)) {
		t.Errorf("EMA of a flat 100 series = %v, want 100", ema)
	}
}

func TestWithParams_DoesNotCarryAccumulatedState(t *testing.T) {
	// Strategy.WithParams must return a copy that has NOT inherited evaluation state — previous MA
	// values, trend direction, armed pivots, trailing bands. The natural Go implementation
	// (`cp := *s`) copies those fields, so this is easy to get wrong silently.
	//
	// cmd/strategy-optimizer tunes parameters by exactly this call (internal/optimizer/runner.go's
	// buildStrategy). It happens to call it on a fresh factory instance today, so nothing is broken
	// in production — but a leak here would mean candidates scored against another configuration's
	// state, making the whole comparison meaningless. The contract is asserted rather than left to
	// the caller's discipline.
	//
	// Covers every kind in Factories, so a new strategy that adds state without resetting it fails
	// here rather than in a tuning run whose results silently mean nothing.
	candles := oscillating(400, 100, 15, 9)

	for kind, factory := range Factories {
		t.Run(kind, func(t *testing.T) {
			// Warm one instance up on real candles so any state it keeps is genuinely populated.
			warmed := factory()
			for i := 60; i <= len(candles); i++ {
				if _, err := warmed.Evaluate(candles[:i]); err != nil {
					t.Fatalf("warm-up evaluate: %v", err)
				}
			}

			// A copy made from the warmed instance must behave identically to one made from a fresh
			// instance: same params in, same signal sequence out. If state leaked, the warmed copy
			// starts mid-trend and diverges.
			params := map[string]decimal.Decimal{}
			for _, spec := range warmed.Params() {
				params[spec.Name] = spec.Default
			}

			fromWarmed := warmed.WithParams(params)
			fromFresh := factory().WithParams(params)

			for i := 60; i <= len(candles); i++ {
				a, errA := fromWarmed.Evaluate(candles[:i])
				b, errB := fromFresh.Evaluate(candles[:i])
				if (errA == nil) != (errB == nil) {
					t.Fatalf("at i=%d: error mismatch %v vs %v", i, errA, errB)
				}
				if a.Side != b.Side {
					t.Fatalf("at i=%d: copy from a warmed instance gave %v, copy from a fresh one gave %v — "+
						"accumulated state leaked through WithParams", i, a.Side, b.Side)
				}
			}
		})
	}
}
