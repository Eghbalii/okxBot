package usecase

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

func ptr(d decimal.Decimal) *decimal.Decimal { return &d }

func TestRatchetSLTP_LongTighteningAllowed(t *testing.T) {
	// Long, entry 100, SL 95, TP 110. Price now 105. Agent proposes tightening SL up (locking
	// profit) and TP closer (2% adjust each).
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("110"))}
	price := dec("105")
	newSL, newTP := RatchetSLTP(o, price, dec("0.02"), dec("0.02"))

	// SL should move up by 2% of price (105*0.02=2.1) -> 97.1
	if !newSL.Equal(dec("97.1")) {
		t.Errorf("expected SL 97.1, got %s", newSL)
	}
	// TP should move down (closer) by 2% of price -> 110 - 2.1 = 107.9
	if !newTP.Equal(dec("107.9")) {
		t.Errorf("expected TP 107.9, got %s", newTP)
	}
}

func TestRatchetSLTP_LongWideningRejected(t *testing.T) {
	// Agent proposes a negative slAdjustPct, which for a long would move SL DOWN (widening risk) —
	// must be rejected, SL stays put.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("110"))}
	price := dec("105")
	newSL, newTP := RatchetSLTP(o, price, dec("-0.02"), dec("-0.02"))

	if !newSL.Equal(dec("95")) {
		t.Errorf("expected SL unchanged at 95 (widening rejected), got %s", newSL)
	}
	if !newTP.Equal(dec("110")) {
		t.Errorf("expected TP unchanged at 110 (widening rejected), got %s", newTP)
	}
}

func TestRatchetSLTP_ShortTighteningAllowed(t *testing.T) {
	// Short, entry 100, SL 105, TP 90. Price now 95. Tightening SL down, TP closer (up).
	o := port.PaperOrder{Side: "sell", EntryPx: dec("100"), SLPx: ptr(dec("105")), TPPx: ptr(dec("90"))}
	price := dec("95")
	newSL, newTP := RatchetSLTP(o, price, dec("0.02"), dec("0.02"))

	// SL moves down by 2% of price (95*0.02=1.9) -> 103.1
	if !newSL.Equal(dec("103.1")) {
		t.Errorf("expected SL 103.1, got %s", newSL)
	}
	// TP moves up (closer to price) by 1.9 -> 91.9
	if !newTP.Equal(dec("91.9")) {
		t.Errorf("expected TP 91.9, got %s", newTP)
	}
}

func TestRatchetSLTP_ShortWideningRejected(t *testing.T) {
	o := port.PaperOrder{Side: "sell", EntryPx: dec("100"), SLPx: ptr(dec("105")), TPPx: ptr(dec("90"))}
	price := dec("95")
	newSL, newTP := RatchetSLTP(o, price, dec("-0.02"), dec("-0.02"))

	if !newSL.Equal(dec("105")) {
		t.Errorf("expected SL unchanged at 105, got %s", newSL)
	}
	if !newTP.Equal(dec("90")) {
		t.Errorf("expected TP unchanged at 90, got %s", newTP)
	}
}

func TestRatchetSLTP_CannotUndoPriorTightening(t *testing.T) {
	// SL already tightened to 99 (from an earlier ratchet step) on a long. A new proposal to move
	// it back down to 96 must be rejected even though 96 > original 95 — the ratchet only allows
	// forward progress relative to the CURRENT stored SL, not the original.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("99")), TPPx: ptr(dec("110"))}
	price := dec("105")
	// -3% of 105 = -3.15, would propose 99 - 3.15 = 95.85, which is less than current 99 -> reject.
	newSL, _ := RatchetSLTP(o, price, dec("-0.03"), dec("0"))
	if !newSL.Equal(dec("99")) {
		t.Errorf("expected SL to stay at ratcheted 99, got %s", newSL)
	}
}

func TestRatchetSLTP_ClampsOversizedAdjustment(t *testing.T) {
	// Proposed adjust of 10% must be clamped to MaxSLTPAdjustPct (2%) before applying.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("110"))}
	price := dec("100")
	newSL, _ := RatchetSLTP(o, price, dec("0.10"), dec("0"))
	// clamped to 2% of 100 = 2 -> 95+2=97
	if !newSL.Equal(dec("97")) {
		t.Errorf("expected SL clamped-adjustment result 97, got %s", newSL)
	}
}

func TestRatchetSLTP_NilSLTPStaysNil(t *testing.T) {
	// An order with no SL/TP set (nil) is not something the ratchet creates — it only tightens an
	// existing one.
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100")}
	price := dec("105")
	newSL, newTP := RatchetSLTP(o, price, dec("0.02"), dec("0.02"))
	if newSL != nil {
		t.Errorf("expected nil SL to stay nil, got %v", newSL)
	}
	if newTP != nil {
		t.Errorf("expected nil TP to stay nil, got %v", newTP)
	}
}

func TestRatchetSLTP_TPCannotCrossPrice(t *testing.T) {
	// An oversized tightening proposal that would push TP past the current price must be rejected
	// (kept at current) rather than producing a TP that's already been "hit".
	o := port.PaperOrder{Side: "buy", EntryPx: dec("100"), SLPx: ptr(dec("95")), TPPx: ptr(dec("100.5"))}
	price := dec("100")
	// 2% of 100 = 2, proposed TP = 100.5 - 2 = 98.5, which is below current price 100 -> reject.
	_, newTP := RatchetSLTP(o, price, dec("0"), dec("0.02"))
	if !newTP.Equal(dec("100.5")) {
		t.Errorf("expected TP unchanged at 100.5 (would cross price), got %s", newTP)
	}
}
