package conductor

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestOpenCategory(t *testing.T) {
	for _, tc := range []struct{ side, want string }{
		{"buy", domain.CategoryBuy},
		{"sell", domain.CategorySell},
		{"hold", ""},
		{"", ""},
	} {
		if got := OpenCategory(tc.side); got != tc.want {
			t.Errorf("OpenCategory(%q) = %q, want %q", tc.side, got, tc.want)
		}
	}
}

func TestTerminalCategory(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{"tp", domain.CategoryClosedTP},
		{"sl", domain.CategoryClosedSL},
		{CloseReasonRLEarly, domain.CategoryClosedEarly},
		// A timeout close shares closed_early's category (CLAUDE.md §15.14): PaperTrader force-
		// closed it for running too long, not the model, but the trade still trains something.
		{CloseReasonTimeout, domain.CategoryClosedEarly},
		// A manual close (the panel's Close button, 2026-08-31) shares the same category for the
		// same reason: the model has no closed_manual category, and reporting nothing would leave
		// that trade training nothing at all.
		{CloseReasonManual, domain.CategoryClosedEarly},
		{"nonsense", ""},
	} {
		if got := TerminalCategory(tc.reason); got != tc.want {
			t.Errorf("TerminalCategory(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestShouldUpdate_FirstCallAlwaysFires(t *testing.T) {
	c := New(Config{})
	if !c.ShouldUpdate(1, dec("0"), time.Now()) {
		t.Fatal("first update for an unseen order should fire: there is no baseline to compare against yet")
	}
}

func TestShouldUpdate_PnLThreshold(t *testing.T) {
	c := New(Config{UpdatePnLThresholdPct: dec("0.01"), UpdateMaxInterval: time.Hour})
	now := time.Now()
	c.ShouldUpdate(1, dec("0"), now)

	if c.ShouldUpdate(1, dec("0.005"), now.Add(time.Second)) {
		t.Error("0.5% move is below the 1% threshold; should not fire")
	}
	if !c.ShouldUpdate(1, dec("0.01"), now.Add(2*time.Second)) {
		t.Error("a move of exactly the threshold should fire")
	}
	// The baseline must have advanced to the firing PnL, so the next comparison is against the
	// new level rather than the original one — otherwise every subsequent tick would fire.
	if c.ShouldUpdate(1, dec("0.012"), now.Add(3*time.Second)) {
		t.Error("baseline did not advance after firing; 0.2% past the new baseline should not fire")
	}
}

func TestShouldUpdate_FiresOnLossesToo(t *testing.T) {
	c := New(Config{UpdatePnLThresholdPct: dec("0.01"), UpdateMaxInterval: time.Hour})
	now := time.Now()
	c.ShouldUpdate(1, dec("0"), now)
	// The trigger is on magnitude of movement, not direction: a position moving 2% AGAINST the
	// trade is at least as important to observe as one moving into profit.
	if !c.ShouldUpdate(1, dec("-0.02"), now.Add(time.Second)) {
		t.Error("a 2% adverse move should fire")
	}
}

func TestShouldUpdate_TimeCeiling(t *testing.T) {
	c := New(Config{UpdatePnLThresholdPct: dec("0.01"), UpdateMaxInterval: 15 * time.Minute})
	now := time.Now()
	c.ShouldUpdate(1, dec("0"), now)

	if c.ShouldUpdate(1, dec("0"), now.Add(14*time.Minute)) {
		t.Error("no PnL movement and the ceiling not yet reached; should not fire")
	}
	// A position grinding sideways must still be observed: funding accrues, setups decay, and
	// "time passed" is itself information (CLAUDE.md §15.12).
	if !c.ShouldUpdate(1, dec("0"), now.Add(15*time.Minute)) {
		t.Error("the time ceiling should fire even with zero PnL movement")
	}
}

func TestShouldUpdate_IsPerOrder(t *testing.T) {
	c := New(Config{UpdatePnLThresholdPct: dec("0.01"), UpdateMaxInterval: time.Hour})
	now := time.Now()
	c.ShouldUpdate(1, dec("0"), now)

	// A second order has its own baseline; order 1's recent update must not suppress it.
	if !c.ShouldUpdate(2, dec("0"), now) {
		t.Error("a different order should have its own cadence state")
	}
}

func TestShouldUpdate_Defaults(t *testing.T) {
	// The zero Config must be usable: every field falls back to a documented default rather than
	// producing a zero threshold, which would fire on every single tick.
	c := New(Config{})
	now := time.Now()
	c.ShouldUpdate(1, dec("0"), now)
	if c.ShouldUpdate(1, dec("0.0001"), now.Add(time.Second)) {
		t.Error("zero Config should fall back to the 1% default, not a zero threshold")
	}
	if !c.ShouldUpdate(1, dec("0.01"), now.Add(2*time.Second)) {
		t.Error("default threshold should fire at 1%")
	}
}

func TestNoteSignalUpdate_ResetsCadence(t *testing.T) {
	c := New(Config{UpdatePnLThresholdPct: dec("0.01"), UpdateMaxInterval: time.Hour})
	now := time.Now()
	c.ShouldUpdate(1, dec("0"), now)

	// A strategy fired at 2% PnL and was sent to the model immediately (signals are never
	// filtered by cadence). Recording it must advance the baseline, or the very next tick would
	// fire a redundant PnL-triggered update for a move already reported.
	c.NoteSignalUpdate(1, dec("0.02"), now.Add(time.Second))
	if c.ShouldUpdate(1, dec("0.02"), now.Add(2*time.Second)) {
		t.Error("a signal-driven update should reset the cadence baseline")
	}
}

func TestForget_DropsState(t *testing.T) {
	c := New(Config{UpdatePnLThresholdPct: dec("0.01"), UpdateMaxInterval: time.Hour})
	now := time.Now()
	c.ShouldUpdate(1, dec("0"), now)
	c.Forget(1)

	// After forgetting, the order looks unseen again — which is also why losing this state on a
	// restart is harmless: the next tick simply establishes a fresh baseline.
	if !c.ShouldUpdate(1, dec("0"), now.Add(time.Second)) {
		t.Error("a forgotten order should be treated as unseen")
	}
}

func TestSignalCarryForward(t *testing.T) {
	c := New(Config{})
	if _, ok := c.CarriedSignal("BTC-USDT-SWAP", "1H"); ok {
		t.Fatal("no signal retained yet")
	}

	sig := domain.StrategySignal{Side: "buy", Kind: "rsi_sma", Bar: "1H", Confidence: dec("0.8")}
	c.RetainSignal("BTC-USDT-SWAP", "1H", sig)

	got, ok := c.CarriedSignal("BTC-USDT-SWAP", "1H")
	if !ok || got.Kind != "rsi_sma" || got.Side != "buy" {
		t.Fatalf("retained signal not returned: %+v ok=%v", got, ok)
	}
	// Retention is per (instId, bar): one token's 1H opinion says nothing about another's, and a
	// 1H signal must not leak onto a 5m decision.
	if _, ok := c.CarriedSignal("BTC-USDT-SWAP", "5m"); ok {
		t.Error("a signal must not carry across timeframes")
	}
	if _, ok := c.CarriedSignal("ETH-USDT-SWAP", "1H"); ok {
		t.Error("a signal must not carry across instruments")
	}
}

func TestRetainSignal_Overwrites(t *testing.T) {
	c := New(Config{})
	c.RetainSignal("BTC-USDT-SWAP", "1H", domain.StrategySignal{Side: "buy", Kind: "rsi_sma"})
	c.RetainSignal("BTC-USDT-SWAP", "1H", domain.StrategySignal{Side: "sell", Kind: "macd"})

	got, _ := c.CarriedSignal("BTC-USDT-SWAP", "1H")
	if got.Side != "sell" || got.Kind != "macd" {
		t.Errorf("carry-forward should hold the MOST RECENT signal, got %+v", got)
	}
}

func TestIsTimedOut_DefaultSixHours(t *testing.T) {
	c := New(Config{})
	openedAt := time.Now().Add(-5 * time.Hour)
	if c.IsTimedOut(openedAt, time.Now()) {
		t.Error("5h open should not be timed out against the 6h default")
	}
	openedAt = time.Now().Add(-6*time.Hour - time.Second)
	if !c.IsTimedOut(openedAt, time.Now()) {
		t.Error("just past 6h open should be timed out against the 6h default")
	}
}

func TestIsTimedOut_ConfiguredDuration(t *testing.T) {
	c := New(Config{MaxOpenDuration: 30 * time.Minute})
	now := time.Now()
	if c.IsTimedOut(now.Add(-29*time.Minute), now) {
		t.Error("29m open should not be timed out against a 30m configured limit")
	}
	if !c.IsTimedOut(now.Add(-31*time.Minute), now) {
		t.Error("31m open should be timed out against a 30m configured limit")
	}
}

func TestIsTimedOut_StatelessAcrossCalls(t *testing.T) {
	// Unlike ShouldUpdate, IsTimedOut takes no orderID and tracks no per-order state -- calling it
	// repeatedly for the same order must give the same answer each time, not "fires once".
	c := New(Config{MaxOpenDuration: time.Hour})
	openedAt := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		if !c.IsTimedOut(openedAt, time.Now()) {
			t.Fatalf("call %d: expected timed out every time, stateless", i)
		}
	}
}
