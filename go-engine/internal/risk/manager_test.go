package risk

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestHalt_TripsCircuitBreakerWithReason(t *testing.T) {
	m := NewManager(Limits{}, decimal.NewFromInt(1000))
	if halted, _ := m.Halted(); halted {
		t.Fatal("expected not halted initially")
	}
	m.Halt("margin mode mismatch")
	halted, reason := m.Halted()
	if !halted {
		t.Fatal("expected halted after Halt()")
	}
	if reason != "margin mode mismatch" {
		t.Fatalf("expected reason %q, got %q", "margin mode mismatch", reason)
	}
}

func TestHalt_ApproveRejectsWhileHalted(t *testing.T) {
	m := NewManager(Limits{MaxLeverage: decimal.NewFromInt(10)}, decimal.NewFromInt(1000))
	m.Halt("test halt")
	_, err := m.Approve(ProposedAction{Leverage: decimal.NewFromInt(5)})
	if err == nil {
		t.Fatal("expected Approve to reject while halted")
	}
}

func TestReset_ClearsHaltSetByHaltMethod(t *testing.T) {
	m := NewManager(Limits{}, decimal.NewFromInt(1000))
	m.Halt("test halt")
	m.Reset(decimal.NewFromInt(2000))
	if halted, _ := m.Halted(); halted {
		t.Fatal("expected Reset to clear a halt set via Halt(), not just CheckDrawdown")
	}
}
