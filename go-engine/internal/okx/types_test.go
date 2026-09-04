package okx

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

// Regression test for the bug found live 2026-09-04 (cmd/okx-apitest's diagnostic against a real
// OKX account): GET /api/v5/trade/order returns avgPx/accFillSz/sz as "" (not "0") for an order
// that has not started filling yet. decimal.Decimal's own UnmarshalJSON rejects "", which made
// every poll of RealTrader.waitForFill's fill-timeout loop fail and retry until the timeout
// elapsed — harmless (the retry loop already tolerated it) but noisy, and a real correctness gap
// waiting to bite the next caller that doesn't retry.
func TestOrderStatus_DecodesBlankDecimalFieldsAsZero(t *testing.T) {
	raw := `{
		"instId": "BTC-USD_UM_XPERP-310404",
		"ordId": "123",
		"clOrdId": "",
		"state": "live",
		"avgPx": "",
		"accFillSz": "",
		"sz": "1"
	}`

	var s OrderStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if !s.AvgPx.Decimal.Equal(decimal.Zero) {
		t.Errorf("AvgPx = %s, want 0", s.AvgPx.Decimal)
	}
	if !s.AccFillSz.Decimal.Equal(decimal.Zero) {
		t.Errorf("AccFillSz = %s, want 0", s.AccFillSz.Decimal)
	}
	if !s.Sz.Decimal.Equal(decimal.NewFromInt(1)) {
		t.Errorf("Sz = %s, want 1", s.Sz.Decimal)
	}

	domainStatus := s.ToDomain()
	if !domainStatus.AvgPx.Equal(decimal.Zero) {
		t.Errorf("domain AvgPx = %s, want 0", domainStatus.AvgPx)
	}
}

func TestOrderStatus_DecodesNonBlankDecimalFieldsNormally(t *testing.T) {
	raw := `{
		"instId": "BTC-USD_UM_XPERP-310404",
		"ordId": "123",
		"state": "filled",
		"avgPx": "81050.5",
		"accFillSz": "1",
		"sz": "1"
	}`

	var s OrderStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	want := decimal.RequireFromString("81050.5")
	if !s.AvgPx.Decimal.Equal(want) {
		t.Errorf("AvgPx = %s, want %s", s.AvgPx.Decimal, want)
	}
}

func TestDecimalOrZero_NullDecodesAsZero(t *testing.T) {
	var d decimalOrZero
	if err := json.Unmarshal([]byte("null"), &d); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !d.Decimal.Equal(decimal.Zero) {
		t.Errorf("got %s, want 0", d.Decimal)
	}
}
