package strategy

import (
	"testing"
	"time"

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
	"zigzag_pa", "open_close_cross", "rsi_divergence", "flawless_victory",
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

// Real production incident (paper order #1127, DOGE, 2026-09-19): zigzag_pa detected a bearish
// harmonic pattern whose D-pivot (0.0913) had already drifted several candles stale by the time the
// pattern confirmed — live price had fallen straight through the computed take-profit (0.09042862)
// to 0.08981 before the order could open. buildPaperOrder fills at the LIVE price, not at the
// pivot, so conductor.Clamps.Apply then validated the target against 0.08981 as entry: for a short,
// a take-profit above entry reads as being on the wrong side and is silently dropped — the position
// opened with a stop but tp_px permanently NULL in paper_orders, confirmed against the live
// database. This replays the exact real candles from that incident (testdata/DOGE5m.csv, pulled
// from the production TimescaleDB) and asserts the strategy no longer emits a target that the live
// price has already passed — the fix strategies must make themselves, since ResolveLevels/Apply
// downstream have no way to know the level was ever coherent relative to a price that has moved on.
func TestZigZagPA_DoesNotEmitATargetThePriceHasAlreadyPassed(t *testing.T) {
	candles := loadRealCandles(t, "DOGE")
	s := NewZigZagPA()

	// The exact candle index where order #1127 opened: 2026-09-19 17:40:00 UTC close (0.08981),
	// the tick immediately after which the real engine opened the order. Located by timestamp
	// rather than hardcoded so a future testdata refresh that shifts row count doesn't silently
	// start checking the wrong candle.
	incidentTS := time.Date(2026, 9, 19, 17, 40, 0, 0, time.UTC)
	incidentIdx := -1
	for i, c := range candles {
		if c.Timestamp.Equal(incidentTS) {
			incidentIdx = i + 1 // Evaluate takes candles[:i], i.e. "up to and including this candle"
			break
		}
	}
	if incidentIdx == -1 {
		t.Fatal("testdata/DOGE5m.csv no longer contains the #1127 incident candle (2026-09-19 17:40:00 UTC) — " +
			"this test can't reproduce the scenario it exists to guard")
	}

	sig, err := s.Evaluate(candles[:incidentIdx])
	if err != nil {
		t.Fatalf("evaluate at the incident candle: %v", err)
	}
	// Before the fix this returned Side=Sell, EntryPx(d)=0.0913, TPPx=0.09042862 — a target the
	// live close (0.08981) had already fallen through, which conductor.Clamps.Apply then silently
	// dropped for being on the wrong side of the LIVE entry, leaving paper_orders.tp_px NULL.
	if sig.Side != Hold {
		t.Errorf("the incident candle produced Side=%s EntryPx(d)=%s TPPx=%s, want Hold — the live "+
			"close %s has already passed this target, so the order this signal becomes would open "+
			"with no take-profit at all (exactly paper order #1127's production bug)",
			sig.Side, sig.EntryPx, sig.TPPx, candles[incidentIdx-1].Close)
	}

	// General invariant, not just the one known incident: replay the whole file and confirm any
	// OTHER signal this strategy does fire (a different D-pivot, a different pattern) also has its
	// target on the correct side of the price the order will actually fill at.
	fired := 0
	s = NewZigZagPA()
	for i := 1; i <= len(candles); i++ {
		sig, err := s.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate at i=%d: %v", i, err)
		}
		if sig.Side == Hold {
			continue
		}
		fired++
		liveClose := candles[i-1].Close
		if sig.Side == Buy && sig.TPPx.LessThanOrEqual(liveClose) {
			t.Errorf("i=%d: buy target %s is already at/behind the live price %s (d=%s)", i, sig.TPPx, liveClose, sig.EntryPx)
		}
		if sig.Side == Sell && sig.TPPx.GreaterThanOrEqual(liveClose) {
			t.Errorf("i=%d: sell target %s is already at/behind the live price %s (d=%s)", i, sig.TPPx, liveClose, sig.EntryPx)
		}
	}
	t.Logf("zigzag_pa fired %d time(s) on the full DOGE fixture (excluding the suppressed incident signal)", fired)
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

// flawless_victory's real source (confirmed 2026-09-18 against the operator-supplied PineScript) is
// a Bollinger Band + RSI confluence with a HIGHER rsi_sell_guard producing FEWER exit/close signals
// — this is the load-bearing threshold in the real algorithm, replacing the first pass's invented
// 3-way majority vote (which had no basis in the actual source and is why MinAgree is gone).
func TestFlawlessVictory_HigherSellGuardProducesFewerCloseSignals(t *testing.T) {
	candles := loadRealCandles(t, "BTC")

	loose := NewFlawlessVictory()
	loose.RSISellGuard = decimal.NewFromInt(55)
	looseFires := 0
	for i := 250; i <= len(candles); i++ {
		sig, err := loose.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate at i=%d: %v", i, err)
		}
		if sig.Side == Sell {
			looseFires++
		}
	}

	strict := NewFlawlessVictory()
	strict.RSISellGuard = decimal.NewFromInt(95)
	strictFires := 0
	for i := 250; i <= len(candles); i++ {
		sig, err := strict.Evaluate(candles[:i])
		if err != nil {
			t.Fatalf("evaluate at i=%d: %v", i, err)
		}
		if sig.Side == Sell {
			strictFires++
		}
	}

	if looseFires == 0 {
		t.Skip("RSISellGuard=55 never fired a close signal on this fixture — nothing to compare against")
	}
	if strictFires >= looseFires {
		t.Errorf("requiring RSI>95 to close (%d fires) did not reduce signals below requiring RSI>55 "+
			"(%d fires) — the sell guard threshold is not being enforced", strictFires, looseFires)
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
