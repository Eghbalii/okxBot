package okx

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
)

// Regression coverage for the sizing bug found live 2026-09-04: OKX's real-trading-eligible
// instrument for this project's account (BTC-USD_UM_XPERP-<date>) has CtVal=0.0001 and LotSz=1,
// sharply different from the classic SWAP instruments this codebase's order sizing implicitly
// assumed a multiplier of 1 for (CLAUDE.md §14/§27).
func TestInstrument_DecodesRealXPerpShape(t *testing.T) {
	raw := `{
		"instId": "BTC-USD_UM_XPERP-310404",
		"ctVal": "0.0001",
		"ctValCcy": "BTC",
		"lotSz": "1",
		"minSz": "1"
	}`

	var i Instrument
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	d := i.ToDomain()
	if !d.CtVal.Equal(decimal.RequireFromString("0.0001")) {
		t.Errorf("CtVal = %s, want 0.0001", d.CtVal)
	}
	if !d.LotSz.Equal(decimal.NewFromInt(1)) {
		t.Errorf("LotSz = %s, want 1", d.LotSz)
	}
	if !d.MinSz.Equal(decimal.NewFromInt(1)) {
		t.Errorf("MinSz = %s, want 1", d.MinSz)
	}
	if d.CtValCcy != "BTC" {
		t.Errorf("CtValCcy = %q, want BTC", d.CtValCcy)
	}
}

func TestInstrument_DecodesClassicSwapShape(t *testing.T) {
	raw := `{
		"instId": "BTC-USDT-SWAP",
		"ctVal": "0.01",
		"ctValCcy": "BTC",
		"lotSz": "0.01",
		"minSz": "0.01"
	}`

	var i Instrument
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	d := i.ToDomain()
	if !d.CtVal.Equal(decimal.RequireFromString("0.01")) {
		t.Errorf("CtVal = %s, want 0.01", d.CtVal)
	}
}
