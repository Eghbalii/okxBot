package usecase

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// candleSeries builds domain.ReturnsWindow+1 candles from a list of closes (enough for exactly one
// BuildReturns call), one minute apart, open==close so each candle's own body is degenerate but its
// close-to-close return is exactly what the test names.
func candleSeries(closes ...float64) []domain.Candle {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]domain.Candle, len(closes))
	for i, c := range closes {
		d := decimal.NewFromFloat(c)
		out[i] = domain.Candle{Timestamp: base.Add(time.Duration(i) * time.Minute), Open: d, High: d, Low: d, Close: d, Volume: decimal.NewFromInt(1)}
	}
	return out
}

func risingCloses() []float64 {
	closes := make([]float64, domain.ReturnsWindow+1)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	return closes
}

func fallingCloses() []float64 {
	closes := make([]float64, domain.ReturnsWindow+1)
	for i := range closes {
		closes[i] = 100 - float64(i)
	}
	return closes
}

// btcCorrelationReturns is the 2026-09-21 fix: correlation is measured one timeframe up from the
// decision bar, resolved dynamically (nextHigherBar) rather than against a hardcoded "15m" — so
// these tests exercise the actual selection/fallback logic, not just that a number comes back.

func TestBTCCorrelationReturns_UsesHigherBarWhenAvailable(t *testing.T) {
	decisionToken := []decimal.Decimal{decimal.NewFromFloat(0.01)} // any non-empty placeholder — must NOT be what's returned
	decisionBTC := []decimal.Decimal{decimal.NewFromFloat(0.02)}

	fifteenMTokenWindow := candleSeries(risingCloses()...)
	fifteenMBTCWindow := candleSeries(fallingCloses()...)

	tokenBars := map[string][]domain.Candle{
		"5m":  candleSeries(risingCloses()...), // present but must be ignored — 5m is the decision bar itself
		"15m": fifteenMTokenWindow,
	}
	btcCandles := func(bar string) ([]domain.Candle, bool) {
		if bar == "15m" {
			return fifteenMBTCWindow, true
		}
		return nil, false
	}

	tokenRets, btcRets := btcCorrelationReturns("5m", decisionToken, decisionBTC, tokenBars, btcCandles)

	wantToken, err := BuildReturns(fifteenMTokenWindow)
	if err != nil {
		t.Fatalf("BuildReturns(15m token window): %v", err)
	}
	wantBTC, err := BuildReturns(fifteenMBTCWindow)
	if err != nil {
		t.Fatalf("BuildReturns(15m btc window): %v", err)
	}
	if len(tokenRets) != len(wantToken) || !tokenRets[0].Equal(wantToken[0]) {
		t.Errorf("token returns = %v, want the 15m series %v (not the decision-bar fallback)", tokenRets, wantToken)
	}
	if len(btcRets) != len(wantBTC) || !btcRets[0].Equal(wantBTC[0]) {
		t.Errorf("btc returns = %v, want the 15m series %v (not the decision-bar fallback)", btcRets, wantBTC)
	}
}

// The whole point of resolving "one step up" dynamically: moving the decision bar to 15m must
// automatically correlate against 1H next time, with no code change.
func TestBTCCorrelationReturns_TracksDecisionBarMovingUp(t *testing.T) {
	oneHTokenWindow := candleSeries(risingCloses()...)
	oneHBTCWindow := candleSeries(fallingCloses()...)

	tokenBars := map[string][]domain.Candle{
		"5m":  candleSeries(risingCloses()...),
		"15m": candleSeries(risingCloses()...), // now the decision bar — must be ignored, not correlated against
		"1H":  oneHTokenWindow,
	}
	btcCandles := func(bar string) ([]domain.Candle, bool) {
		if bar == "1H" {
			return oneHBTCWindow, true
		}
		return nil, false
	}

	tokenRets, _ := btcCorrelationReturns("15m", nil, nil, tokenBars, btcCandles)
	want, err := BuildReturns(oneHTokenWindow)
	if err != nil {
		t.Fatalf("BuildReturns(1H window): %v", err)
	}
	if len(tokenRets) != len(want) || !tokenRets[0].Equal(want[0]) {
		t.Errorf("expected correlation to follow the decision bar up to 1H's own series, got %v want %v", tokenRets, want)
	}
}

func TestBTCCorrelationReturns_FallsBackWhenNoHigherBarConfigured(t *testing.T) {
	decisionToken := []decimal.Decimal{decimal.NewFromFloat(0.01)}
	decisionBTC := []decimal.Decimal{decimal.NewFromFloat(0.02)}

	tokenBars := map[string][]domain.Candle{"5m": candleSeries(risingCloses()...)}
	btcCandles := func(string) ([]domain.Candle, bool) { return nil, false }

	tokenRets, btcRets := btcCorrelationReturns("5m", decisionToken, decisionBTC, tokenBars, btcCandles)
	if len(tokenRets) != 1 || !tokenRets[0].Equal(decisionToken[0]) {
		t.Errorf("expected the decision-bar fallback series, got %v", tokenRets)
	}
	if len(btcRets) != 1 || !btcRets[0].Equal(decisionBTC[0]) {
		t.Errorf("expected the decision-bar fallback series, got %v", btcRets)
	}
}

func TestBTCCorrelationReturns_FallsBackWhenHigherBarWindowNotFilledYet(t *testing.T) {
	decisionToken := []decimal.Decimal{decimal.NewFromFloat(0.01)}
	decisionBTC := []decimal.Decimal{decimal.NewFromFloat(0.02)}

	// 15m is configured (present as a key) but its window hasn't filled to ReturnsWindow+1 yet —
	// the warm-up case CLAUDE.md §9 already describes for other multi-timeframe reads.
	tokenBars := map[string][]domain.Candle{
		"5m":  candleSeries(risingCloses()...),
		"15m": candleSeries(100, 101, 102), // too short for BuildReturns
	}
	btcCandles := func(bar string) ([]domain.Candle, bool) {
		if bar == "15m" {
			return candleSeries(100, 99, 98), true // also too short
		}
		return nil, false
	}

	tokenRets, btcRets := btcCorrelationReturns("5m", decisionToken, decisionBTC, tokenBars, btcCandles)
	if len(tokenRets) != 1 || !tokenRets[0].Equal(decisionToken[0]) {
		t.Errorf("expected the decision-bar fallback during warm-up, got %v", tokenRets)
	}
	if len(btcRets) != 1 || !btcRets[0].Equal(decisionBTC[0]) {
		t.Errorf("expected the decision-bar fallback during warm-up, got %v", btcRets)
	}
}

func TestBTCCorrelationReturns_NilBTCCandlesFuncFallsBack(t *testing.T) {
	decisionToken := []decimal.Decimal{decimal.NewFromFloat(0.01)}
	decisionBTC := []decimal.Decimal{decimal.NewFromFloat(0.02)}
	tokenBars := map[string][]domain.Candle{"15m": candleSeries(risingCloses()...)}

	tokenRets, btcRets := btcCorrelationReturns("5m", decisionToken, decisionBTC, tokenBars, nil)
	if len(tokenRets) != 1 || !tokenRets[0].Equal(decisionToken[0]) {
		t.Errorf("expected the decision-bar fallback with a nil btcCandles func, got %v", tokenRets)
	}
	if len(btcRets) != 1 || !btcRets[0].Equal(decisionBTC[0]) {
		t.Errorf("expected the decision-bar fallback with a nil btcCandles func, got %v", btcRets)
	}
}

// End-to-end through BuildBTCContext itself: correlating a rising token series against a rising
// BTC series (both computed from the SAME higher-bar windows) must read close to +1, proving the
// higher-bar series actually reaches Correlation, not just that btcCorrelationReturns resolves
// them correctly in isolation.
func TestBuildBTCContext_CorrelationUsesTheResolvedHigherBarSeries(t *testing.T) {
	decisionWindow := candleSeries(risingCloses()...) // BTC's own decision-bar window (for OHLC/swing)
	higherToken, err := BuildReturns(candleSeries(risingCloses()...))
	if err != nil {
		t.Fatalf("BuildReturns: %v", err)
	}
	higherBTC, err := BuildReturns(candleSeries(risingCloses()...))
	if err != nil {
		t.Fatalf("BuildReturns: %v", err)
	}

	ctx, err := BuildBTCContext(decisionWindow, higherToken, higherBTC)
	if err != nil {
		t.Fatalf("BuildBTCContext: %v", err)
	}
	if ctx.Correlation.LessThan(decimal.NewFromFloat(0.99)) {
		t.Errorf("correlation = %s, want close to 1 (two identical rising series)", ctx.Correlation)
	}
}
