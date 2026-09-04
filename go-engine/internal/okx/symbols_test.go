package okx

import "testing"

func TestSymbolMap_ResolveKnownSymbol(t *testing.T) {
	m := SymbolMap{"BTC": "BTC-USD_UM_XPERP-310404"}
	got, err := m.Resolve("BTC")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if got != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("Resolve(BTC) = %q, want BTC-USD_UM_XPERP-310404", got)
	}
}

func TestSymbolMap_ResolveUnknownSymbolErrors(t *testing.T) {
	m := SymbolMap{"BTC": "BTC-USD_UM_XPERP-310404"}
	if _, err := m.Resolve("DOGE"); err == nil {
		t.Fatal("expected an error resolving an unmapped symbol, got nil")
	}
}

func TestSymbolMap_ResolveEmptyValueErrors(t *testing.T) {
	// A symbol present in the map but mapped to "" must fail the same as an absent one — never
	// silently subscribe to/call an empty instId.
	m := SymbolMap{"BTC": ""}
	if _, err := m.Resolve("BTC"); err == nil {
		t.Fatal("expected an error resolving a symbol mapped to an empty instId, got nil")
	}
}

func TestSymbolMap_ResolveAllStopsOnFirstError(t *testing.T) {
	m := SymbolMap{"BTC": "BTC-USD_UM_XPERP-310404"}
	_, err := m.ResolveAll([]string{"BTC", "DOGE", "ETH"})
	if err == nil {
		t.Fatal("expected an error for the unresolvable DOGE symbol, got nil")
	}
}

func TestSymbolMap_ResolveAllPreservesOrder(t *testing.T) {
	m := SymbolMap{
		"BTC": "BTC-USD_UM_XPERP-310404",
		"ETH": "ETH-USD_UM_XPERP-310404",
	}
	got, err := m.ResolveAll([]string{"ETH", "BTC"})
	if err != nil {
		t.Fatalf("ResolveAll failed: %v", err)
	}
	want := []string{"ETH-USD_UM_XPERP-310404", "BTC-USD_UM_XPERP-310404"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ResolveAll = %v, want %v", got, want)
	}
}
