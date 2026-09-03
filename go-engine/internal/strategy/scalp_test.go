package strategy

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// The strategies added for 5m scalping (CLAUDE.md §30) need fixtures the package's existing
// `oscillating` helper cannot provide. That helper builds candles with Open == Close (zero-height
// bodies), constant volume, and no Timestamp — so every strategy keying off candle body direction
// or a volume surge returns Hold on all of them, and any test driven purely by that fixture passes
// while exercising nothing. That is exactly how the ICT re-arm bugs these tests now cover reached
// production: TestWithParams_DoesNotCarryAccumulatedState reported PASS for ict_fvg and
// ict_order_block while comparing Hold against Hold for all 400 candles.

// barTime returns the open time of bar i on a 5m series, so fixtures carry the real Timestamp the
// engine always populates (tickfeed.go's parseCandleFields) and which the ICT strategies use to
// identify a setup across calls.
//
// Timestamps are REQUIRED for ict_fvg and ict_order_block to signal at all: they identify a setup
// by the timestamp of the candle that formed it, so a fixture leaving Timestamp at its zero value
// makes every candle look older than "nothing handled yet" and the strategies return Hold forever.
// That is the safe direction to fail, but a new fixture built without timestamps would look like a
// broken strategy rather than a broken fixture — which is why these helpers set it centrally.
func barTime(i int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * 5 * time.Minute)
}

// bar builds one timestamped candle. Volume defaults to a flat 10 where a test doesn't care.
func bar(i int, o, h, l, c float64) Candle {
	return barVol(i, o, h, l, c, 10)
}

func barVol(i int, o, h, l, c, v float64) Candle {
	return Candle{
		Timestamp: barTime(i),
		Open:      decimal.NewFromFloat(o),
		High:      decimal.NewFromFloat(h),
		Low:       decimal.NewFromFloat(l),
		Close:     decimal.NewFromFloat(c),
		Volume:    decimal.NewFromFloat(v),
	}
}

// bodiedCandles builds a timestamped series with real bodies (Open != Close), varying volume and
// noise — the shape a body-pattern or volume-surge strategy actually needs to be exercised at all.
func bodiedCandles(n int, seed int64) []Candle {
	r := rand.New(rand.NewSource(seed))
	out := make([]Candle, n)
	price := 100.0
	for i := range out {
		trend := 3 * math.Sin(float64(i)/20)
		open := price
		close := price + trend*0.3 + (r.Float64()-0.5)*2
		high := math.Max(open, close) + r.Float64()*0.8
		low := math.Min(open, close) - r.Float64()*0.8
		vol := 10.0
		if r.Float64() < 0.1 {
			vol = 50.0 // occasional surge, so volume_breakout can trigger
		}
		out[i] = barVol(i, open, high, low, close, vol)
		price = close
	}
	return out
}

// scalpKinds is every kind added in CLAUDE.md §30.
var scalpKinds = []string{
	"vwap_reversion", "bb_squeeze_breakout", "range_breakout", "keltner_trend_scalp",
	"ict_fvg", "ict_order_block", "ict_liquidity_sweep", "engulfing_reversal",
	"inside_bar_breakout", "macd_momentum", "volume_breakout", "ema_ribbon_pullback",
}

// Every new strategy must actually produce signals on realistic data. A strategy that silently
// never fires looks identical to a working one in every aggregate metric — that is precisely how
// pmax stayed permanently silent while registered and assignable (CLAUDE.md §16.8).
func TestScalpStrategies_FireOnRealisticData(t *testing.T) {
	candles := bodiedCandles(800, 42)
	for _, kind := range scalpKinds {
		t.Run(kind, func(t *testing.T) {
			factory, ok := Factories[kind]
			if !ok {
				t.Fatalf("kind %q is not registered in Factories", kind)
			}
			s := factory()
			fired := 0
			for i := 60; i <= len(candles); i++ {
				sig, err := s.Evaluate(candles[:i])
				if err != nil {
					t.Fatalf("evaluate at i=%d: %v", i, err)
				}
				if sig.Side != Hold {
					fired++
				}
			}
			if fired == 0 {
				t.Errorf("never fired across %d candles — a permanently silent strategy is "+
					"indistinguishable from a working one in production metrics", len(candles))
			}
		})
	}
}

// Whatever a strategy emits must be a coherent trade: a stop on the losing side of entry and a
// target on the winning side. A stop on the wrong side would be touched instantly, and a target
// past entry realizes a loss under close_reason='tp' — the exact defect CLAUDE.md §16.9 found in
// the ratchet.
func TestScalpStrategies_EmitCoherentLevels(t *testing.T) {
	candles := bodiedCandles(800, 7)
	for _, kind := range scalpKinds {
		t.Run(kind, func(t *testing.T) {
			s := Factories[kind]()
			for i := 60; i <= len(candles); i++ {
				sig, err := s.Evaluate(candles[:i])
				if err != nil {
					t.Fatalf("evaluate at i=%d: %v", i, err)
				}
				if sig.Side == Hold {
					continue
				}
				r := sig.ResolveLevels(candles[i-1].Close)
				if !r.SLPx.IsPositive() {
					t.Fatalf("at i=%d: %s signal has no stop at all", i, r.Side)
				}
				if r.Side == Buy {
					if r.SLPx.GreaterThanOrEqual(r.EntryPx) {
						t.Fatalf("at i=%d: buy stop %s is not below entry %s", i, r.SLPx, r.EntryPx)
					}
					if r.TPPx.IsPositive() && r.TPPx.LessThanOrEqual(r.EntryPx) {
						t.Fatalf("at i=%d: buy target %s is not above entry %s", i, r.TPPx, r.EntryPx)
					}
				} else {
					if r.SLPx.LessThanOrEqual(r.EntryPx) {
						t.Fatalf("at i=%d: sell stop %s is not above entry %s", i, r.SLPx, r.EntryPx)
					}
					if r.TPPx.IsPositive() && r.TPPx.GreaterThanOrEqual(r.EntryPx) {
						t.Fatalf("at i=%d: sell target %s is not below entry %s", i, r.TPPx, r.EntryPx)
					}
				}
			}
		})
	}
}

// The real WithParams state-isolation contract, driven by a fixture the stateful strategies
// actually respond to. TestWithParams_DoesNotCarryAccumulatedState covers every kind but runs on
// `oscillating`, where these return Hold on every bar — so it compares Hold to Hold and proves
// nothing about them. This asserts the same contract AND that the comparison was non-trivial.
func TestScalpStrategies_WithParamsDoesNotCarryState(t *testing.T) {
	candles := bodiedCandles(500, 11)
	for _, kind := range scalpKinds {
		t.Run(kind, func(t *testing.T) {
			factory := Factories[kind]
			warmed := factory()
			for i := 60; i <= len(candles); i++ {
				if _, err := warmed.Evaluate(candles[:i]); err != nil {
					t.Fatalf("warm-up: %v", err)
				}
			}
			params := map[string]decimal.Decimal{}
			for _, spec := range warmed.Params() {
				params[spec.Name] = spec.Default
			}
			fromWarmed := warmed.WithParams(params)
			fromFresh := factory().WithParams(params)

			fired := 0
			for i := 60; i <= len(candles); i++ {
				a, errA := fromWarmed.Evaluate(candles[:i])
				b, errB := fromFresh.Evaluate(candles[:i])
				if (errA == nil) != (errB == nil) {
					t.Fatalf("at i=%d: error mismatch %v vs %v", i, errA, errB)
				}
				if a.Side != b.Side {
					t.Fatalf("at i=%d: copy from a warmed instance gave %v, copy from a fresh one "+
						"gave %v — accumulated state leaked through WithParams", i, a.Side, b.Side)
				}
				if a.Side != Hold {
					fired++
				}
			}
			if fired == 0 {
				t.Errorf("comparison was vacuous: never fired on this fixture, so state isolation " +
					"is untested for this kind")
			}
		})
	}
}

// --- ict_fvg -------------------------------------------------------------------------------

// bullishFVG builds the minimal 3-candle bullish imbalance: c1.High (100) < c3.Low (105), so the
// gap zone is [100, 105].
func bullishFVG() []Candle {
	return []Candle{
		bar(0, 99, 100, 98, 99),
		bar(1, 101, 108, 101, 107),
		bar(2, 107, 110, 105, 109),
	}
}

// driveCandleByCandle feeds a strategy one closed candle at a time, the way the engine does
// (papertrade.go's handleCandle), and returns each call's signal. Evaluating a whole sequence in a
// single call instead would skip the intermediate states these strategies build up, and would not
// exercise the re-arm behavior at all.
func driveCandleByCandle(t *testing.T, s Strategy, candles []Candle, from int) []Signal {
	t.Helper()
	var out []Signal
	for i := from; i <= len(candles); i++ {
		sig, err := s.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate at i=%d: %v", i, err)
		}
		out = append(out, sig)
	}
	return out
}

// A gap is only tradeable once price RETURNS to it. Entering on the candle that formed the gap
// takes the top of the impulse — the worst price in the setup — and the test is trivially true
// there (a bullish gap's upper edge IS that candle's own low).
func TestICTFVG_DoesNotEnterOnTheGapFormingCandle(t *testing.T) {
	sigs := driveCandleByCandle(t, NewICTFairValueGap(), bullishFVG(), 3)
	if sigs[len(sigs)-1].Side != Hold {
		t.Errorf("fired %q on the candle that created the gap; price has not returned to it yet",
			sigs[len(sigs)-1].Side)
	}
}

// One imbalance is one trade. The scan re-detects the same triple on every call for as long as it
// stays inside LookbackBars, so without setup identity the consumed state is re-armed and the same
// gap fires an entry on every subsequent candle.
func TestICTFVG_TradesOneGapOnlyOnce(t *testing.T) {
	seq := bullishFVG()
	for i := 0; i < 10; i++ {
		// Each candle dips to 103, inside the [100, 105] zone.
		seq = append(seq, bar(3+i, 106, 107, 103, 106))
	}
	fires := 0
	for _, sig := range driveCandleByCandle(t, NewICTFairValueGap(), seq, 3) {
		if sig.Side != Hold {
			fires++
		}
	}
	if fires != 1 {
		t.Errorf("one gap produced %d entries, want exactly 1 — the rescan is re-arming a setup "+
			"that was already traded", fires)
	}
}

// The entry itself must be the pullback: stop at the gap's far edge, target above.
func TestICTFVG_EntersOnTheReturnWithGapEdgeAsStop(t *testing.T) {
	seq := append(bullishFVG(), bar(3, 106, 107, 103, 106))
	sigs := driveCandleByCandle(t, NewICTFairValueGap(), seq, 3)
	sig := sigs[len(sigs)-1]
	if sig.Side != Buy {
		t.Fatalf("want Buy on the return into the zone, got %q", sig.Side)
	}
	if !sig.SLPx.Equal(decimal.NewFromInt(100)) {
		t.Errorf("stop = %s, want the gap's lower edge 100", sig.SLPx)
	}
	if !sig.EntryPx.Equal(decimal.NewFromInt(106)) {
		t.Errorf("entry = %s, want the returning candle's close 106", sig.EntryPx)
	}
}

// A gap price trades straight through is not support — it must be discarded, not entered later.
func TestICTFVG_InvalidatedGapNeverTrades(t *testing.T) {
	seq := append(bullishFVG(),
		bar(3, 106, 107, 95, 96), // low 95 breaks below the zone's floor — not support after all
		bar(4, 96, 104, 96, 103), // back into the old zone; must not resurrect it
	)
	for i, sig := range driveCandleByCandle(t, NewICTFairValueGap(), seq, 3) {
		if sig.Side != Hold {
			t.Errorf("call %d traded an invalidated gap (%q)", i, sig.Side)
		}
	}
}

// --- ict_order_block ------------------------------------------------------------------------

// orderBlockSetup builds a flat warm-up, a bearish candle (the block, range [99, 100.2]) and a
// large bullish impulse, so a bullish order block is armed.
func orderBlockSetup() []Candle {
	var seq []Candle
	for i := 0; i < 8; i++ {
		seq = append(seq, bar(i, 100, 100.5, 99.5, 100))
	}
	seq = append(seq, bar(8, 100, 100.2, 99, 99.2))
	seq = append(seq, bar(9, 99.2, 106, 99.1, 105.5))
	return seq
}

func TestICTOrderBlock_TradesOneBlockOnlyOnce(t *testing.T) {
	s := NewICTOrderBlock()
	s.ATRPeriod = 3
	seq := orderBlockSetup()
	for i := 0; i < 8; i++ {
		seq = append(seq, bar(10+i, 103, 103.5, 100, 103)) // dips into the block's range
	}
	fires := 0
	for _, sig := range driveCandleByCandle(t, s, seq, 10) {
		if sig.Side != Hold {
			fires++
		}
	}
	if fires != 1 {
		t.Errorf("one order block produced %d entries, want exactly 1 — the rescan is re-arming a "+
			"setup that was already traded", fires)
	}
}

func TestICTOrderBlock_EntersOnReturnWithBlockLowAsStop(t *testing.T) {
	s := NewICTOrderBlock()
	s.ATRPeriod = 3
	seq := append(orderBlockSetup(), bar(10, 103, 103.5, 100, 103))
	sigs := driveCandleByCandle(t, s, seq, 10)
	sig := sigs[len(sigs)-1]
	if sig.Side != Buy {
		t.Fatalf("want Buy on the return into the block, got %q", sig.Side)
	}
	if !sig.SLPx.Equal(decimal.NewFromFloat(99)) {
		t.Errorf("stop = %s, want the order block's low 99", sig.SLPx)
	}
}

// --- macd_momentum -------------------------------------------------------------------------

// The histogram's two compared values must come from the candles passed in, not from state carried
// between calls: evaluating the same window twice must give the same answer both times. A
// state-carried "previous" silently compares a value against itself on the second call.
func TestMACDMomentum_IsDeterministicForTheSameWindow(t *testing.T) {
	candles := bodiedCandles(200, 3)
	s := NewMACDMomentum()
	for i := 60; i <= len(candles); i++ {
		first, err := s.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		second, err := s.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("re-evaluate: %v", err)
		}
		if first.Side != second.Side {
			t.Fatalf("at i=%d: evaluating the same window twice gave %q then %q", i, first.Side, second.Side)
		}
	}
}

// A fresh instance handed a full window must agree with one that saw the window grow candle by
// candle — the engine reseeds from Postgres on restart (CLAUDE.md §14), so the two paths have to
// produce the same signal for the same bar.
func TestMACDMomentum_ColdStartMatchesWarmedInstance(t *testing.T) {
	candles := bodiedCandles(200, 5)
	warmed := NewMACDMomentum()
	for i := 60; i < len(candles); i++ {
		if _, err := warmed.Evaluate(candles[:i]); err != nil {
			t.Fatalf("warm-up: %v", err)
		}
	}
	warmSig, err := warmed.Evaluate(candles)
	if err != nil {
		t.Fatalf("warmed evaluate: %v", err)
	}
	coldSig, err := NewMACDMomentum().Evaluate(candles)
	if err != nil {
		t.Fatalf("cold evaluate: %v", err)
	}
	if warmSig.Side != coldSig.Side {
		t.Errorf("warmed instance gave %q, cold instance gave %q for the same final window",
			warmSig.Side, coldSig.Side)
	}
}

// --- ict_liquidity_sweep -------------------------------------------------------------------

// The pattern is a wick THROUGH a swing level that closes back inside — a candle that simply
// closes beyond the level is a genuine breakout and must not be faded.
func TestICTLiquiditySweep_RequiresCloseBackInsideTheRange(t *testing.T) {
	s := NewICTLiquiditySweep()
	s.SwingLookback = 5
	base := []Candle{
		bar(0, 100, 101, 99, 100),
		bar(1, 100, 101, 99, 100),
		bar(2, 100, 101, 99, 100),
		bar(3, 100, 101, 99, 100),
		bar(4, 100, 101, 99, 100),
	}

	swept := append(append([]Candle{}, base...), bar(5, 100, 103, 99.5, 100.5))
	sig, err := s.Evaluate(swept)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if sig.Side != Sell {
		t.Errorf("a wick above the swing high closing back inside should fade the sweep, got %q", sig.Side)
	}

	brokeOut := append(append([]Candle{}, base...), bar(5, 100, 103, 99.5, 102.5))
	sig, err = NewICTLiquiditySweep().WithParams(map[string]decimal.Decimal{
		"swing_lookback": decimal.NewFromInt(5),
	}).Evaluate(brokeOut)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if sig.Side != Hold {
		t.Errorf("a candle closing above the swing high is a real breakout, not a sweep; got %q", sig.Side)
	}
}

// --- inside_bar_breakout -------------------------------------------------------------------

// The inside bar is consumed on the breakout; the same pattern must not fire twice.
func TestInsideBarBreakout_TradesOnePatternOnlyOnce(t *testing.T) {
	s := NewInsideBarBreakout()
	seq := []Candle{
		bar(0, 100, 105, 95, 100), // mother bar
		bar(1, 100, 103, 97, 101), // inside bar, range [97, 103]
	}
	// The engine evaluates once per closed candle, so the inside bar has to be seen (and armed) by
	// its own call before the breakout candle arrives.
	if _, err := s.Evaluate(seq); err != nil {
		t.Fatalf("arming evaluate: %v", err)
	}

	seq = append(seq, bar(2, 101, 106, 100, 105)) // breaks above 103
	sig, err := s.Evaluate(seq)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if sig.Side != Buy {
		t.Fatalf("want Buy on the inside-bar breakout, got %q", sig.Side)
	}
	if !sig.SLPx.Equal(decimal.NewFromInt(97)) {
		t.Errorf("stop = %s, want the inside bar's low 97", sig.SLPx)
	}
	// A further push in the same direction is not a second signal from the same pattern.
	seq = append(seq, bar(3, 105, 110, 104, 109))
	if sig, err = s.Evaluate(seq); err != nil {
		t.Fatalf("evaluate: %v", err)
	} else if sig.Side != Hold {
		t.Errorf("the same inside bar fired a second time (%q)", sig.Side)
	}
}
