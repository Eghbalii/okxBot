package tester

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

func d(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return v
}

func TestBuildOrder_ResolvesLevelsFromPercentages(t *testing.T) {
	sig := strategy.Signal{Side: strategy.Buy, SLPct: d("0.02"), TPPct: d("0.05")}
	o := BuildOrder("BTC-USDT-SWAP", 7, "5m", d("100"), d("10"), d("10"), decimal.Zero, sig)

	if o.InstID != "BTC-USDT-SWAP" || o.VersionID != 7 || o.Bar != "5m" || o.Side != "buy" {
		t.Fatalf("unexpected order shape: %+v", o)
	}
	if !o.EntryPx.Equal(d("100")) {
		t.Fatalf("entry = %s, want 100", o.EntryPx)
	}
	if o.SLPx == nil || !o.SLPx.Equal(d("98")) {
		t.Fatalf("sl = %v, want 98", o.SLPx)
	}
	if o.TPPx == nil || !o.TPPx.Equal(d("105")) {
		t.Fatalf("tp = %v, want 105", o.TPPx)
	}
	if !o.Size.Equal(d("10")) || !o.Leverage.Equal(d("10")) {
		t.Fatalf("size/leverage not passed through fixed: %+v", o)
	}
}

func TestBuildOrder_SellDirection(t *testing.T) {
	sig := strategy.Signal{Side: strategy.Sell, SLPct: d("0.02"), TPPct: d("0.05")}
	o := BuildOrder("XRP-USDT-SWAP", 1, "5m", d("100"), d("10"), d("10"), decimal.Zero, sig)

	if o.SLPx == nil || !o.SLPx.Equal(d("102")) {
		t.Fatalf("sell sl = %v, want 102 (above entry)", o.SLPx)
	}
	if o.TPPx == nil || !o.TPPx.Equal(d("95")) {
		t.Fatalf("sell tp = %v, want 95 (below entry)", o.TPPx)
	}
}

func TestBuildOrder_NoLevelsWhenSignalHasNone(t *testing.T) {
	sig := strategy.Signal{Side: strategy.Buy}
	o := BuildOrder("BTC-USDT-SWAP", 1, "5m", d("100"), d("10"), d("10"), decimal.Zero, sig)
	if o.SLPx != nil || o.TPPx != nil {
		t.Fatalf("expected nil levels for a signal with no pct/px set, got sl=%v tp=%v", o.SLPx, o.TPPx)
	}
}

// MaxLossPct caps the REALIZED loss a strategy's own SL can produce once leverage is applied
// (2026-08-31 request: "SL should never allow more than 15% loss, at any leverage, no cap on
// profit") — this service has no EnsureStop-style fallback, but an SL that IS set must still
// respect the cap.
func TestBuildOrder_MaxLossPctTightensStopAtHighLeverage(t *testing.T) {
	// 10% price-distance SL at 20x leverage would realize 200% loss; capped to 15% means the
	// price distance must shrink to 0.75%.
	sig := strategy.Signal{Side: strategy.Buy, SLPct: d("0.10")}
	o := BuildOrder("BTC-USDT-SWAP", 1, "5m", d("100"), d("10"), d("20"), d("0.15"), sig)
	if o.SLPx == nil || !o.SLPx.Equal(d("99.25")) {
		t.Fatalf("sl = %v, want tightened to 99.25 (0.75%% distance caps 20x loss at 15%%)", o.SLPx)
	}
}

func TestBuildOrder_MaxLossPctDoesNotWidenAnAlreadyTighterStop(t *testing.T) {
	// A 1% price-distance SL at 20x already realizes only 20% — wait, that's OVER 15%, so it
	// should still tighten. Use a genuinely-safe 0.5% distance (10% loss at 20x) to confirm the
	// clamp leaves an already-compliant stop untouched.
	sig := strategy.Signal{Side: strategy.Buy, SLPct: d("0.005")}
	o := BuildOrder("BTC-USDT-SWAP", 1, "5m", d("100"), d("10"), d("20"), d("0.15"), sig)
	if o.SLPx == nil || !o.SLPx.Equal(d("99.5")) {
		t.Fatalf("sl = %v, want unchanged 99.5 (10%% loss at 20x is within the 15%% cap)", o.SLPx)
	}
}

func TestBuildOrder_MaxLossPctMirroredForSell(t *testing.T) {
	sig := strategy.Signal{Side: strategy.Sell, SLPct: d("0.10")}
	o := BuildOrder("BTC-USDT-SWAP", 1, "5m", d("100"), d("10"), d("20"), d("0.15"), sig)
	if o.SLPx == nil || !o.SLPx.Equal(d("100.75")) {
		t.Fatalf("sell sl = %v, want tightened to 100.75 (above entry, mirrored)", o.SLPx)
	}
}

func TestBuildOrder_MaxLossPctZeroDisablesTheClamp(t *testing.T) {
	sig := strategy.Signal{Side: strategy.Buy, SLPct: d("0.10")}
	o := BuildOrder("BTC-USDT-SWAP", 1, "5m", d("100"), d("10"), d("20"), decimal.Zero, sig)
	if o.SLPx == nil || !o.SLPx.Equal(d("90")) {
		t.Fatalf("sl = %v, want unchanged 90 when maxLossPct is disabled", o.SLPx)
	}
}

func TestBuildOrder_MaxLossPctHasNoEffectOnTakeProfit(t *testing.T) {
	sig := strategy.Signal{Side: strategy.Buy, SLPct: d("0.10"), TPPct: d("2.0")}
	o := BuildOrder("BTC-USDT-SWAP", 1, "5m", d("100"), d("10"), d("20"), d("0.15"), sig)
	if o.TPPx == nil || !o.TPPx.Equal(d("300")) {
		t.Fatalf("tp = %v, want unchanged 300 — profit is never capped regardless of leverage", o.TPPx)
	}
}

func TestRealizedPnL_Buy(t *testing.T) {
	// +5% move, $10 notional, 10x leverage -> +$5
	pnl := RealizedPnL("buy", d("100"), d("105"), d("10"), d("10"))
	if !pnl.Equal(d("5")) {
		t.Fatalf("pnl = %s, want 5", pnl)
	}
}

func TestRealizedPnL_SellProfitsOnDrop(t *testing.T) {
	// price fell 5%, short profits: +$5
	pnl := RealizedPnL("sell", d("100"), d("95"), d("10"), d("10"))
	if !pnl.Equal(d("5")) {
		t.Fatalf("pnl = %s, want 5", pnl)
	}
}

func TestRealizedPnL_LeverageScales(t *testing.T) {
	pnl1x := RealizedPnL("buy", d("100"), d("110"), d("10"), d("1"))
	pnl10x := RealizedPnL("buy", d("100"), d("110"), d("10"), d("10"))
	if !pnl10x.Equal(pnl1x.Mul(d("10"))) {
		t.Fatalf("10x pnl (%s) should be exactly 10x the 1x pnl (%s)", pnl10x, pnl1x)
	}
}

func TestIsTimedOut(t *testing.T) {
	now := time.Now()
	if IsTimedOut(now.Add(-5*time.Hour), now, 6*time.Hour) {
		t.Error("5h open should not be timed out against a 6h limit")
	}
	if !IsTimedOut(now.Add(-6*time.Hour-time.Second), now, 6*time.Hour) {
		t.Error("just past 6h open should be timed out against a 6h limit")
	}
}

func TestIsTimedOut_Stateless(t *testing.T) {
	now := time.Now()
	openedAt := now.Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		if !IsTimedOut(openedAt, now, time.Hour) {
			t.Fatalf("call %d: expected timed out every time, stateless", i)
		}
	}
}
