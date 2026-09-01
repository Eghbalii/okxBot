package api

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestRealizedPnLOverWindow_SumsOnlyTradeReasonsInWindow(t *testing.T) {
	now := time.Now().UTC()
	history := []port.EquityPoint{
		{CreatedAt: now.Add(-48 * time.Hour), EquityUSD: dec("100"), DeltaUSD: dec("100"), Reason: "seed"},
		{CreatedAt: now.Add(-20 * time.Hour), EquityUSD: dec("110"), DeltaUSD: dec("10"), Reason: "trade"},
		{CreatedAt: now.Add(-10 * time.Hour), EquityUSD: dec("105"), DeltaUSD: dec("-5"), Reason: "trade"},
	}

	usd, pct := realizedPnLOverWindow(history, now.Add(-24*time.Hour))
	if !usd.Equal(dec("5")) {
		t.Fatalf("expected usd=5 (10 - 5), got %s", usd)
	}
	// Window started at 105 - 5 = 100.
	if !pct.Equal(dec("5")) {
		t.Fatalf("expected pct=5 (5/100*100), got %s", pct)
	}
}

func TestRealizedPnLOverWindow_ExcludesPointsBeforeSince(t *testing.T) {
	now := time.Now().UTC()
	history := []port.EquityPoint{
		{CreatedAt: now.Add(-72 * time.Hour), EquityUSD: dec("200"), DeltaUSD: dec("100"), Reason: "trade"},
		{CreatedAt: now.Add(-1 * time.Hour), EquityUSD: dec("210"), DeltaUSD: dec("10"), Reason: "trade"},
	}

	usd, _ := realizedPnLOverWindow(history, now.Add(-24*time.Hour))
	if !usd.Equal(dec("10")) {
		t.Fatalf("expected only the in-window trade counted (10), got %s", usd)
	}
}

func TestRealizedPnLOverWindow_EmptyHistoryReturnsZero(t *testing.T) {
	usd, pct := realizedPnLOverWindow(nil, time.Now())
	if !usd.IsZero() || !pct.IsZero() {
		t.Fatalf("expected zero usd/pct for empty history, got usd=%s pct=%s", usd, pct)
	}
}

func TestRealizedPnLOverWindow_ResetsCountTowardDeltaButPctHandlesZeroStart(t *testing.T) {
	now := time.Now().UTC()
	// A reset-to-zero-then-topped-up scenario: window start equity computed as 0, guarding against
	// a division by zero rather than propagating a decimal panic/Inf.
	history := []port.EquityPoint{
		{CreatedAt: now.Add(-1 * time.Hour), EquityUSD: dec("100"), DeltaUSD: dec("100"), Reason: "reset"},
	}
	usd, pct := realizedPnLOverWindow(history, now.Add(-24*time.Hour))
	if !usd.IsZero() {
		t.Fatalf("expected usd=0 (reset isn't a trade), got %s", usd)
	}
	if !pct.IsZero() {
		t.Fatalf("expected pct=0 when window start equity is 0 (no division by zero), got %s", pct)
	}
}
