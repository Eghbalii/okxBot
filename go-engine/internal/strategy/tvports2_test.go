package strategy

import (
	"testing"

	"github.com/shopspring/decimal"
)

// The 17 TradingView ports requested 2026-09-16 (CLAUDE.md's port task). Per §30.1/§45.3's own
// hard-won lesson, these are tested against REAL committed OKX candles (testdata/*.csv), never a
// synthetic fixture with flat/smooth bodies — a whole suite passed vacuously once already by
// comparing Hold against Hold across a degenerate fixture, and several V2 strategies looked inert
// against synthetic data while firing normally against real data.

// portedKinds is every kind added by this task, derived by name rather than by re-deriving a
// registry filter, since none of them share the V1/V2/scalp suffix conventions the existing helper
// functions (v2Kinds, scalpKinds) filter by.
var portedKinds = []string{
	"philakones_fib", "ut_bot", "scalper_macd_psar_ema200", "ichimoku_tk_cross", "hma_swing",
	"micurobert_ema_cross", "bb_breakout", "hammers_stars", "price_volume_breakout",
	"most_strategy", "zigzag_pa", "open_close_cross", "rsi_divergence", "flawless_victory",
	"hull_suite", "adx_dmi_quality", "anchored_vwap_trend",
}

func TestPortedKinds_AllRegistered(t *testing.T) {
	for _, k := range portedKinds {
		if _, ok := Factories[k]; !ok {
			t.Errorf("kind %q is not registered in Factories", k)
		}
	}
}

// Every strategy must actually fire on at least one of the three real instruments — the anti-
// vacuity guard from CLAUDE.md §16.8 (pmax) and §45.3 (six V2 strategies inert on a synthetic
// fixture). A strategy that never signals cannot have its levels checked and would pass every other
// test here trivially.
func TestPortedKinds_FireOnRealMarketData(t *testing.T) {
	symbols := []string{"BTC", "SOL", "ZEC"}
	markets := make(map[string][]Candle, len(symbols))
	for _, sym := range symbols {
		markets[sym] = loadRealCandles(t, sym)
	}

	for _, kind := range portedKinds {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			total := 0
			for _, sym := range symbols {
				sigs := driveV2(t, Factories[kind](), markets[sym], 250)
				total += len(sigs)
				t.Logf("  %s on %s: %d signals over %d candles", kind, sym, len(sigs), len(markets[sym]))
			}
			if total == 0 {
				t.Errorf("%s never fired across %d real instruments — inert or over-filtered", kind, len(symbols))
			}
		})
	}
}

// Every emitted signal must be a coherent trade: a stop on the losing side of entry, a target on
// the winning side. CLAUDE.md §16.9 records a real production incident where an inverted target
// closed trades at a loss under close_reason='tp' — this is exactly the class of bug that check
// exists to catch, asserted here against real traded prices rather than synthetic ones.
func TestPortedKinds_EmitCoherentLevels(t *testing.T) {
	symbols := []string{"BTC", "SOL", "ZEC"}
	for _, kind := range portedKinds {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			checked := 0
			for _, sym := range symbols {
				candles := loadRealCandles(t, sym)
				s := Factories[kind]()
				for i := 250; i <= len(candles); i++ {
					sig, err := s.Evaluate(candles[:i])
					if err != nil {
						t.Fatalf("%s/%s: evaluate at i=%d: %v", kind, sym, i, err)
					}
					if sig.Side == Hold {
						continue
					}
					r := sig.ResolveLevels(candles[i-1].Close)
					if !r.SLPx.IsPositive() {
						t.Fatalf("%s/%s at i=%d: %s signal has no stop at all", kind, sym, i, r.Side)
					}
					checked++
					if r.Side == Buy {
						if r.SLPx.GreaterThanOrEqual(r.EntryPx) {
							t.Fatalf("%s/%s at i=%d: buy stop %s is not below entry %s", kind, sym, i, r.SLPx, r.EntryPx)
						}
						if r.TPPx.IsPositive() && r.TPPx.LessThanOrEqual(r.EntryPx) {
							t.Fatalf("%s/%s at i=%d: buy target %s is not above entry %s", kind, sym, i, r.TPPx, r.EntryPx)
						}
					} else {
						if r.SLPx.LessThanOrEqual(r.EntryPx) {
							t.Fatalf("%s/%s at i=%d: sell stop %s is not above entry %s", kind, sym, i, r.SLPx, r.EntryPx)
						}
						if r.TPPx.IsPositive() && r.TPPx.GreaterThanOrEqual(r.EntryPx) {
							t.Fatalf("%s/%s at i=%d: sell target %s is not below entry %s", kind, sym, i, r.TPPx, r.EntryPx)
						}
					}
				}
			}
			if checked == 0 {
				t.Errorf("%s emitted no signal across any real instrument — the coherence check passed vacuously", kind)
			}
		})
	}
}

// --- targeted, mutation-checkable tests for the trickiest ports -----------------------------

// ut_bot's whole signal depends on the trailing stop RATCHETING (never retreating against the
// trend) rather than being recomputed fresh every bar — the same structural property pmax.go's own
// bands need (CLAUDE.md §16.8). Without the ratchet, a single down-tick inside an uptrend would
// flip the trend on noise; this drives a clean uptrend and asserts the stop only ever rises.
func TestUTBot_TrailingStopRatchetsUpwardInAnUptrend(t *testing.T) {
	s := NewUTBot()
	var candles []Candle
	px := 100.0
	var stops []decimal.Decimal
	for i := 0; i < 60; i++ {
		// A steady uptrend with a small pullback every 5th bar (never enough to cross the trailing
		// stop and flip the trend) — this is what actually exercises the ratchet: on a strictly
		// monotonic rise, close-nLoss alone would rise every bar with or without decimal.Max, so a
		// missing ratchet would go undetected. The dip is what makes decimal.Max load-bearing.
		if i%5 == 4 {
			px -= 0.3
		} else {
			px += 0.6
		}
		c := decimal.NewFromFloat(px)
		candles = append(candles, Candle{
			Timestamp: barTime(i),
			Open:      c, High: c.Add(decimal.NewFromFloat(0.2)), Low: c.Sub(decimal.NewFromFloat(0.2)), Close: c,
			Volume: decimal.NewFromInt(10),
		})
		if _, err := s.Evaluate(candles); err != nil {
			t.Fatalf("evaluate at %d: %v", i, err)
		}
		stops = append(stops, s.prevStop)
	}
	// Once seeded, the trailing stop in a clean uptrend must never fall from one bar to the next.
	for i := s.ATRPeriod + 3; i < len(stops); i++ {
		if stops[i].LessThan(stops[i-1]) {
			t.Fatalf("trailing stop fell from %s to %s at step %d in a clean uptrend — the ratchet is not holding",
				stops[i-1], stops[i], i)
		}
	}
}

// zigzag_pa must trade a given higher-low/lower-high structural pattern only ONCE, mirroring the
// exact re-arm bug CLAUDE.md §30.1 found in ict_fvg/ict_order_block: a strategy that rescans the
// window on every call and identifies a setup by position rather than identity re-fires on every
// subsequent candle that still satisfies the pattern.
func TestZigZagPA_TradesOneStructureOnlyOnce(t *testing.T) {
	s := NewZigZagPA()
	s.Depth = 2
	var seq []Candle
	px := 100.0
	// Build a clear higher-low structure: down to a low, up, down to a HIGHER low, then break above
	// the intervening swing high and hold there for many bars (which would re-satisfy "close above
	// recent high" repeatedly if setup identity weren't tracked).
	add := func(o, h, l, c float64) {
		i := len(seq)
		seq = append(seq, Candle{
			Timestamp: barTime(i),
			Open:      decimal.NewFromFloat(o), High: decimal.NewFromFloat(h),
			Low: decimal.NewFromFloat(l), Close: decimal.NewFromFloat(c),
			Volume: decimal.NewFromInt(10),
		})
	}
	for i := 0; i < 6; i++ {
		add(px, px+1, px-1, px)
	}
	add(px, px, px-8, px-6) // swing low #1 at ~92
	for i := 0; i < 4; i++ {
		add(px, px+3, px-1, px+2) // rally
	}
	add(px+5, px+9, px+4, px+8) // swing high at ~109
	for i := 0; i < 4; i++ {
		add(px+5, px+7, px+3, px+5)
	}
	add(px+2, px+3, px-3, px-1) // swing low #2 at ~97 — HIGHER than -8's ~92
	// Now push through the recent swing high and hold well above it for a long stretch.
	for i := 0; i < 20; i++ {
		add(px+15, px+16, px+14, px+15)
	}

	fires := 0
	for i := 20; i <= len(seq); i++ {
		sig, err := s.Evaluate(seq[:i])
		if err != nil {
			t.Fatalf("evaluate at i=%d: %v", i, err)
		}
		if sig.Side != Hold {
			fires++
		}
	}
	if fires == 0 {
		t.Skip("this fixture did not produce a higher-low breakout on this Depth — pattern-detection itself untested here")
	}
	if fires > 1 {
		t.Errorf("one higher-low structure produced %d entries, want at most 1 — the same structural "+
			"pattern is re-arming on every subsequent candle that still satisfies it", fires)
	}
}

// The Hull Moving Average must actually respond to a genuine trend reversal — a slope-based
// strategy that never turns is silently broken in a way no other check here would catch (it could
// still "fire" if a bug made it fire on every candle instead of on turns).
func TestHMA_TracksATrendReversal(t *testing.T) {
	var candles []Candle
	px := 100.0
	// Rising for 60 bars, then falling for 60 — HMA must eventually read below its own earlier peak
	// value after the reversal, proving it actually follows price rather than latching.
	for i := 0; i < 60; i++ {
		px += 1
		candles = append(candles, bar(i, px, px+0.5, px-0.5, px))
	}
	peak, err := HMA(candles, 20)
	if err != nil {
		t.Fatalf("hma at peak: %v", err)
	}
	for i := 60; i < 120; i++ {
		px -= 1
		candles = append(candles, bar(i, px, px+0.5, px-0.5, px))
	}
	trough, err := HMA(candles, 20)
	if err != nil {
		t.Fatalf("hma at trough: %v", err)
	}
	if !trough.LessThan(peak) {
		t.Errorf("HMA after a 60-bar downtrend (%s) is not below its value at the prior peak (%s)", trough, peak)
	}
}

// most_strategy's band must ratchet the same way ut_bot's/pmax's do: in a clean uptrend the lower
// band must never retreat once established, or the whole "trailing stop" premise the strategy is
// named for does not hold.
func TestMostStrategy_BandRatchetsUpwardInAnUptrend(t *testing.T) {
	s := NewMostStrategy()
	var candles []Candle
	px := 100.0
	var bands []decimal.Decimal
	for i := 0; i < 60; i++ {
		px += 0.5
		c := decimal.NewFromFloat(px)
		candles = append(candles, Candle{
			Timestamp: barTime(i),
			Open:      c, High: c.Add(decimal.NewFromFloat(0.2)), Low: c.Sub(decimal.NewFromFloat(0.2)), Close: c,
			Volume: decimal.NewFromInt(10),
		})
		if _, err := s.Evaluate(candles); err != nil {
			t.Fatalf("evaluate at %d: %v", i, err)
		}
		bands = append(bands, s.prevMOST)
	}
	for i := s.EMALen + 3; i < len(bands); i++ {
		if bands[i].LessThan(bands[i-1]) {
			t.Fatalf("MOST band fell from %s to %s at step %d in a clean uptrend — the ratchet is not holding",
				bands[i-1], bands[i], i)
		}
	}
}

// flawless_victory's majority vote must actually require agreement — requiring all 3 of 3
// conditions to agree (MinAgree=3, unanimity) must produce strictly fewer signals than requiring
// only 1, which is the check that the vote count is real rather than any single member's opinion
// passed straight through. (MinAgree is clamped to the 1..3 range Params() declares — the strategy
// only has 3 conditions to vote, so an "unreachable" threshold isn't expressible; unanimity is the
// strictest real one.)
func TestFlawlessVictory_RequiresAgreement(t *testing.T) {
	candles := loadRealCandles(t, "BTC")

	loose := NewFlawlessVictory()
	loose.MinAgree = 1
	looseFires := 0
	for i := 250; i <= len(candles); i++ {
		sig, err := loose.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate at i=%d: %v", i, err)
		}
		if sig.Side != Hold {
			looseFires++
		}
	}

	strict := NewFlawlessVictory()
	strict.MinAgree = 3
	strictFires := 0
	for i := 250; i <= len(candles); i++ {
		sig, err := strict.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate at i=%d: %v", i, err)
		}
		if sig.Side != Hold {
			strictFires++
		}
	}

	if looseFires == 0 {
		t.Skip("MinAgree=1 never fired on this fixture — nothing to compare against")
	}
	if strictFires >= looseFires {
		t.Errorf("requiring unanimity (MinAgree=3, %d fires) did not reduce signals below requiring "+
			"just 1 vote (%d fires) — the agreement count is not being enforced", strictFires, looseFires)
	}
}

// MinAgree must be clamped into Params()' declared [1,3] range, since the strategy only ever
// produces 3 votes — a value above 3 would otherwise silently make the strategy permanently unable
// to fire, which is not what an out-of-range proposal should do (ClampParam's whole contract).
func TestFlawlessVictory_ClampsMinAgreeIntoRange(t *testing.T) {
	f := NewFlawlessVictory()
	f.MinAgree = 4
	// Rebuild through WithParams, the real path a proposed value arrives through.
	out := f.WithParams(map[string]decimal.Decimal{
		"min_agree": decimal.NewFromInt(4),
	}).(*FlawlessVictory)
	if out.MinAgree > 3 {
		t.Errorf("MinAgree %d was not clamped to the declared max of 3", out.MinAgree)
	}
}

// OpenCloseCross.DelayBars is the source page's "Delay Open/Close MA" input (found on a second,
// targeted re-check of the listing page, 2026-09-18, after the operator noticed the first port had
// dropped it): "To enable non-Repainting mode set 'Delay Open/Close MA' to 1 or more, but expect
// the reported performance to drop dramatically." Zero (repainting, the original's own default)
// evaluates the cross on the just-closed bar; a positive value re-checks it DelayBars candles back
// instead, so a signal already reported can never be revised by data that arrives afterward.
//
// Asserted here as a real behavior change against real candles, not merely that the field exists:
// shifting which bar the cross is read from must shift WHEN the strategy signals, matching every
// other kind's "the port was actually applied, not merely accepted" test in this file.
func TestOpenCloseCross_DelayBarsShiftsTheEvaluatedBar(t *testing.T) {
	candles := loadRealCandles(t, "ZEC")

	noDelay := NewOpenCloseCross()
	delayed := NewOpenCloseCross()
	delayed.DelayBars = 3

	// First index (from 250 onward) where each variant produces a real signal.
	firstSignal := func(s Strategy) int {
		for i := 250; i <= len(candles); i++ {
			sig, err := s.Evaluate(candles[:i])
			if err != nil {
				t.Fatalf("evaluate at i=%d: %v", i, err)
			}
			if sig.Side != Hold {
				return i
			}
		}
		return -1
	}

	noDelayAt := firstSignal(noDelay)
	delayedAt := firstSignal(delayed)
	if noDelayAt == -1 || delayedAt == -1 {
		t.Skip("one variant never fired on this fixture — nothing to compare timing against")
	}
	if noDelayAt == delayedAt {
		t.Errorf("DelayBars=3 fired at the SAME candle index (%d) as DelayBars=0 — the delay is "+
			"declared but not actually shifting which bar the cross is evaluated against", noDelayAt)
	}
}

// A strategy needing MORE history for a larger delay must return Hold rather than index out of
// range or evaluate against a too-short window — this is the exact "need" calculation DelayBars
// changes (Period+1+DelayBars, not just Period+1).
func TestOpenCloseCross_DelayBarsExtendsTheWarmupRequirement(t *testing.T) {
	s := NewOpenCloseCross()
	s.Period = 14
	s.DelayBars = 5
	// Enough candles for the undelayed case (Period+1=15) but short of Period+1+DelayBars=20.
	short := make([]Candle, 18)
	for i := range short {
		short[i] = Candle{Open: decimal.NewFromInt(100), Close: decimal.NewFromInt(100)}
	}
	sig, err := s.Evaluate(short)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if sig.Side != Hold {
		t.Error("evaluated with fewer candles than Period+1+DelayBars requires — should Hold, not read past the delayed window")
	}
}

// delay_bars must be clamped into its declared [0,20] range through WithParams, the real path a
// proposed value arrives through from the panel/optimizer.
func TestOpenCloseCross_ClampsDelayBarsIntoRange(t *testing.T) {
	s := NewOpenCloseCross()
	out := s.WithParams(map[string]decimal.Decimal{
		"delay_bars": decimal.NewFromInt(999),
	}).(*OpenCloseCross)
	if out.DelayBars > 20 {
		t.Errorf("DelayBars %d was not clamped to the declared max of 20", out.DelayBars)
	}

	out2 := s.WithParams(map[string]decimal.Decimal{
		"delay_bars": decimal.NewFromInt(-5),
	}).(*OpenCloseCross)
	if out2.DelayBars < 0 {
		t.Errorf("DelayBars %d was not clamped to the declared min of 0", out2.DelayBars)
	}
}
