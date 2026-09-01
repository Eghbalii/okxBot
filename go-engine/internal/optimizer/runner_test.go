package optimizer

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

func TestSignalPrices_BuySide(t *testing.T) {
	entry := decimal.NewFromInt(100)
	signal := strategy.Signal{Side: strategy.Buy, SLPct: decimal.NewFromFloat(0.01), TPPct: decimal.NewFromFloat(0.02)}

	sl, tp := signalPrices(entry, signal, decimal.Zero)
	if sl == nil || !sl.Equal(decimal.NewFromInt(99)) {
		t.Errorf("expected SL 99 for a buy 1%% below entry, got %v", sl)
	}
	if tp == nil || !tp.Equal(decimal.NewFromInt(102)) {
		t.Errorf("expected TP 102 for a buy 2%% above entry, got %v", tp)
	}
}

func TestSignalPrices_SellSide(t *testing.T) {
	entry := decimal.NewFromInt(100)
	signal := strategy.Signal{Side: strategy.Sell, SLPct: decimal.NewFromFloat(0.01), TPPct: decimal.NewFromFloat(0.02)}

	sl, tp := signalPrices(entry, signal, decimal.Zero)
	if sl == nil || !sl.Equal(decimal.NewFromInt(101)) {
		t.Errorf("expected SL 101 for a sell 1%% above entry, got %v", sl)
	}
	if tp == nil || !tp.Equal(decimal.NewFromInt(98)) {
		t.Errorf("expected TP 98 for a sell 2%% below entry, got %v", tp)
	}
}

func TestSignalPrices_NoSLOrTPWhenPctZero(t *testing.T) {
	entry := decimal.NewFromInt(100)
	signal := strategy.Signal{Side: strategy.Buy}

	sl, tp := signalPrices(entry, signal, decimal.Zero)
	if sl != nil {
		t.Errorf("expected nil SL when SLPct is zero, got %v", sl)
	}
	if tp != nil {
		t.Errorf("expected nil TP when TPPct is zero, got %v", tp)
	}
}

func TestSignalPrices_CapsSLAtMaxLossPct_BuySide(t *testing.T) {
	entry := decimal.NewFromInt(100)
	// SLPct of 30% would place SL at 70, far past the 15% cap.
	signal := strategy.Signal{Side: strategy.Buy, SLPct: decimal.NewFromFloat(0.30), TPPct: decimal.NewFromFloat(0.02)}

	sl, tp := signalPrices(entry, signal, decimal.NewFromFloat(0.15))
	if sl == nil || !sl.Equal(decimal.NewFromInt(85)) {
		t.Errorf("expected SL clamped to 85 (15%% below entry), got %v", sl)
	}
	if tp == nil || !tp.Equal(decimal.NewFromInt(102)) {
		t.Errorf("expected TP unaffected by the loss cap, got %v", tp)
	}
}

func TestSignalPrices_CapsSLAtMaxLossPct_SellSide(t *testing.T) {
	entry := decimal.NewFromInt(100)
	signal := strategy.Signal{Side: strategy.Sell, SLPct: decimal.NewFromFloat(0.30)}

	sl, _ := signalPrices(entry, signal, decimal.NewFromFloat(0.15))
	if sl == nil || !sl.Equal(decimal.NewFromInt(115)) {
		t.Errorf("expected SL clamped to 115 (15%% above entry), got %v", sl)
	}
}

func TestSignalPrices_MaxLossPctNeverWidensATighterSL(t *testing.T) {
	entry := decimal.NewFromInt(100)
	signal := strategy.Signal{Side: strategy.Buy, SLPct: decimal.NewFromFloat(0.01)}

	sl, _ := signalPrices(entry, signal, decimal.NewFromFloat(0.15))
	if sl == nil || !sl.Equal(decimal.NewFromInt(99)) {
		t.Errorf("expected SL to stay at 99 (already inside the cap), got %v", sl)
	}
}

func TestSignalPrices_ZeroMaxLossPctDisablesCap(t *testing.T) {
	entry := decimal.NewFromInt(100)
	signal := strategy.Signal{Side: strategy.Buy, SLPct: decimal.NewFromFloat(0.30)}

	sl, _ := signalPrices(entry, signal, decimal.Zero)
	if sl == nil || !sl.Equal(decimal.NewFromInt(70)) {
		t.Errorf("expected uncapped SL of 70 when maxLossPct is zero, got %v", sl)
	}
}

func TestCandidateStrategy_BuildsFromFactory(t *testing.T) {
	params := map[string]decimal.Decimal{
		"rsi_period": decimal.NewFromInt(21),
		"sma_period": decimal.NewFromInt(60),
	}
	s, err := candidateStrategy("rsi_sma", params)
	if err != nil {
		t.Fatalf("candidateStrategy failed: %v", err)
	}
	if s.Name() != "rsi_sma" {
		t.Errorf("expected strategy name rsi_sma, got %s", s.Name())
	}
}

func TestCandidateStrategy_UnknownKindErrors(t *testing.T) {
	_, err := candidateStrategy("not_a_real_kind", nil)
	if err == nil {
		t.Fatal("expected an error for an unknown strategy kind")
	}
}

func TestNewRun_InitializesStatus(t *testing.T) {
	origin := strategy.NewRSISMA(14, 50)
	cfg := RunConfig{
		RunDuration:           0, // deadline math only; not actually waited on in this test
		MinTradesPerCandidate: 15,
		Bar:                   "15m",
		BatchSize:             5,
	}
	run := NewRun("run-1", "BTC-USDT-SWAP", "rsi_sma", origin, cfg, nil, nil, nil, nil)

	st := run.Status()
	if st.RunID != "run-1" || st.InstID != "BTC-USDT-SWAP" || st.Kind != "rsi_sma" {
		t.Errorf("unexpected initial status: %+v", st)
	}
	if st.Done {
		t.Error("a freshly-created run should not be Done")
	}
	if st.CandidateCount != 0 {
		t.Errorf("expected 0 candidates initially, got %d", st.CandidateCount)
	}
}

func TestRun_Expired(t *testing.T) {
	origin := strategy.NewRSISMA(14, 50)
	cfg := RunConfig{RunDuration: 0, Bar: "15m"} // 0 duration => deadline is "now", so Expired() should be true almost immediately
	run := NewRun("run-2", "BTC-USDT-SWAP", "rsi_sma", origin, cfg, nil, nil, nil, nil)

	if !run.Expired() {
		t.Error("expected a zero-duration run to be immediately expired")
	}
}
