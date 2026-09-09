package usecase

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// Regression coverage for the real-trading gaps found live 2026-09-04 (CLAUDE.md §27): this
// project's market data (InstID) is collected against the classic, deeply liquid SWAP instrument
// (e.g. BTC-USDT-SWAP), but a real account may only have usable margin on a DIFFERENT OKX product
// for the same underlying (this account's is BTC-USD_UM_XPERP-<date>, instType FUTURES, settled
// in USDC — not USDT). Every exchange call must target that execution instrument/instType/
// currency, never the market-data InstID directly, and sizing must convert through the
// instrument's own CtVal/LotSz rather than assuming a 1:1 base-unit-to-contract multiplier.

func TestSizeToContracts_ConvertsThroughCtValAndRoundsToLotSize(t *testing.T) {
	// BTC-USD_UM_XPERP-310404's real shape, found live: CtVal=0.0001 BTC, LotSz=1 contract.
	inst := domain.Instrument{CtVal: dec("0.0001"), LotSz: dec("1")}

	// $40 notional at $80,000/BTC = 0.0005 BTC = 5 contracts exactly.
	got := sizeToContracts(dec("40"), dec("1"), dec("80000"), inst)
	want := dec("5")
	if !got.Equal(want) {
		t.Errorf("sizeToContracts = %s, want %s", got, want)
	}
}

func TestSizeToContracts_RoundsDownToLotSizeNeverUp(t *testing.T) {
	// $44 notional at $80,000/BTC = 0.00055 BTC = 5.5 contracts — must round DOWN to 5, never up
	// to 6 (which would exceed the notional the risk manager actually approved).
	inst := domain.Instrument{CtVal: dec("0.0001"), LotSz: dec("1")}
	got := sizeToContracts(dec("44"), dec("1"), dec("80000"), inst)
	want := dec("5")
	if !got.Equal(want) {
		t.Errorf("sizeToContracts = %s, want %s (must round down)", got, want)
	}
}

func TestSizeToContracts_FractionalLotSize(t *testing.T) {
	// Classic SWAP shape: CtVal=0.01 BTC, LotSz=0.01 contracts.
	inst := domain.Instrument{CtVal: dec("0.01"), LotSz: dec("0.01")}
	got := sizeToContracts(dec("800"), dec("1"), dec("80000"), inst)
	want := dec("1") // 800/80000 = 0.01 BTC / 0.01 CtVal = 1 contract
	if !got.Equal(want) {
		t.Errorf("sizeToContracts = %s, want %s", got, want)
	}
}

func TestSizeToContracts_ZeroCtValTreatedAsOne(t *testing.T) {
	// Defensive: an unset/misconfigured instrument must not divide by zero.
	inst := domain.Instrument{CtVal: decimal.Zero, LotSz: decimal.Zero}
	got := sizeToContracts(dec("100"), dec("1"), dec("50"), inst)
	want := dec("2") // 100/50 / 1 = 2, no lot rounding since LotSz is not positive
	if !got.Equal(want) {
		t.Errorf("sizeToContracts = %s, want %s", got, want)
	}
}

func TestExecInstID_FallsBackToInstIDWhenUnset(t *testing.T) {
	rt := &RealTrader{InstID: "BTC-USDT-SWAP"}
	if got := rt.execInstID(); got != "BTC-USDT-SWAP" {
		t.Errorf("execInstID() = %q, want BTC-USDT-SWAP", got)
	}
}

func TestExecInstID_UsesExecInstIDWhenSet(t *testing.T) {
	rt := &RealTrader{InstID: "BTC-USDT-SWAP", ExecInstID: "BTC-USD_UM_XPERP-310404"}
	if got := rt.execInstID(); got != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("execInstID() = %q, want BTC-USD_UM_XPERP-310404", got)
	}
}

func TestExecInstType_DefaultsToSwap(t *testing.T) {
	rt := &RealTrader{}
	if got := rt.execInstType(); got != "SWAP" {
		t.Errorf("execInstType() = %q, want SWAP", got)
	}
}

func TestExecInstType_UsesConfiguredValue(t *testing.T) {
	rt := &RealTrader{ExecInstType: "FUTURES"}
	if got := rt.execInstType(); got != "FUTURES" {
		t.Errorf("execInstType() = %q, want FUTURES", got)
	}
}

func TestSettleCcy_DefaultsToUSDT(t *testing.T) {
	rt := &RealTrader{}
	if got := rt.settleCcy(); got != "USDT" {
		t.Errorf("settleCcy() = %q, want USDT", got)
	}
}

func TestSettleCcy_UsesConfiguredValue(t *testing.T) {
	rt := &RealTrader{SettleCcy: "USDC"}
	if got := rt.settleCcy(); got != "USDC" {
		t.Errorf("settleCcy() = %q, want USDC", got)
	}
}

// TestOpenReal_UsesExecInstIDNotMarketDataInstID confirms the actual PlaceOrder call targets
// ExecInstID, not InstID, when the two differ — the concrete bug found live: an order attempted
// against BTC-USDT-SWAP would have been rejected outright (50124) for an account only eligible to
// trade BTC-USD_UM_XPERP-310404.
func TestOpenReal_UsesExecInstIDNotMarketDataInstID(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: dec("0.0001"), LotSz: dec("1")},
	}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("0.5"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.ExecInstID = "BTC-USD_UM_XPERP-310404"
	rt.ExecInstType = "FUTURES"
	rt.SettleCcy = "USDC"
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("80000")}}
	exchange.balances = []domain.Balance{{Ccy: "USDC", Eq: dec("1000")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("80000"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	if len(exchange.placedOrders) != 1 {
		t.Fatalf("expected exactly 1 real order placed, got %d", len(exchange.placedOrders))
	}
	if exchange.placedOrders[0].InstID != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("PlaceOrder InstID = %q, want BTC-USD_UM_XPERP-310404 (the execution instrument, not the market-data one)",
			exchange.placedOrders[0].InstID)
	}
}

// TestOpenReal_ConvertsSizeThroughInstrumentMetadata confirms the placed order's Sz field is a
// real contract count (via CtVal/LotSz), not a raw notional/price division — the bug that would
// have sized an order roughly 10,000x too large against BTC-USD_UM_XPERP-310404's real CtVal.
func TestOpenReal_ConvertsSizeThroughInstrumentMetadata(t *testing.T) {
	repo := newFakeRepository()
	exchange := &fakeExchangeClient{
		instrument: &domain.Instrument{CtVal: dec("0.0001"), LotSz: dec("1")},
	}
	// SizePct=1, LeverageFrac chosen so the approved notional lands on a clean contract count.
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, SizePct: dec("1"), LeverageFrac: dec("0.5"),
	}}
	strategies := []StrategyAssignment{{Bar: "1m", Strategy: &stubStrategy{signal: buySignal()}, StrategyID: 1, Kind: "stub"}}
	rt := newTestRealTrader(repo, exchange, model, strategies)
	rt.ExecInstID = "BTC-USD_UM_XPERP-310404"
	rt.candles = map[string][]domain.Candle{"1m": {realTraderCandle("80000")}}
	exchange.balances = []domain.Balance{{Ccy: "USDT", Eq: dec("1000")}}

	if err := rt.evaluateStrategies(context.Background(), "1m", dec("80000"), testLogger()); err != nil {
		t.Fatalf("evaluateStrategies returned error: %v", err)
	}

	if len(exchange.placedOrders) != 1 {
		t.Fatalf("expected exactly 1 real order placed, got %d", len(exchange.placedOrders))
	}
	sz := exchange.placedOrders[0].Sz
	// Whatever the approved notional was, the resulting contract count must be a whole multiple
	// of LotSz=1 — i.e. an integer — which a raw notional/price division would NOT generally be.
	if !sz.Equal(sz.Truncate(0)) {
		t.Errorf("placed order Sz = %s is not a whole contract count (LotSz=1)", sz)
	}
	if sz.IsZero() {
		t.Error("placed order Sz is zero — sizing conversion likely broken")
	}
}

// The bug that made every real position 1/leverage of its intended size (2026-09-09): margin was
// converted straight to contracts with leverage never applied, so $2 of margin at 10x opened a $2
// position instead of the $20 one leverage exists to provide.
//
// These are order 5's real numbers: $1.87 of margin at 9.44x against SOL (CtVal 0.01, LotSz 1) at
// 102.93 is a $17.66 position — 17 contracts. It opened 1.
func TestSizeToContracts_AppliesLeverageToReachNotional(t *testing.T) {
	inst := domain.Instrument{CtVal: dec("0.01"), LotSz: dec("1"), MinSz: dec("1")}
	got := sizeToContracts(dec("1.87"), dec("9.44"), dec("102.93"), inst)
	if !got.Equal(dec("17")) {
		t.Fatalf("sizeToContracts with leverage = %s contracts, want 17 (a $17.66 position, not $1.03)", got)
	}
}

// Leverage multiplies the position, so the same margin at 10x buys ten times what it does at 1x.
func TestSizeToContracts_ScalesWithLeverage(t *testing.T) {
	inst := domain.Instrument{CtVal: dec("1"), LotSz: dec("1")}
	one := sizeToContracts(dec("100"), dec("1"), dec("10"), inst)
	ten := sizeToContracts(dec("100"), dec("10"), dec("10"), inst)
	if !one.Equal(dec("10")) {
		t.Fatalf("1x: want 10 contracts, got %s", one)
	}
	if !ten.Equal(dec("100")) {
		t.Fatalf("10x: want 100 contracts (10x the 1x size), got %s", ten)
	}
}

// A missing/zero leverage must behave as 1x rather than sizing the position to zero — the same
// defensive fallback CtVal already had.
func TestSizeToContracts_ZeroLeverageTreatedAsOne(t *testing.T) {
	inst := domain.Instrument{CtVal: dec("1"), LotSz: dec("1")}
	got := sizeToContracts(dec("100"), decimal.Zero, dec("10"), inst)
	if !got.Equal(dec("10")) {
		t.Fatalf("zero leverage must size as 1x: want 10, got %s", got)
	}
}

// The expensive tokens the affordability service disabled were never actually unaffordable — they
// only looked that way because sizing ignored leverage. ZEC's minimum contract is ~$11.52; a $2
// budget at 10x is $20 of buying power, which clears it comfortably.
func TestSizeToContracts_LeverageClearsTheContractMinimum(t *testing.T) {
	inst := domain.Instrument{CtVal: dec("0.01"), LotSz: dec("1"), MinSz: dec("1")}
	price := dec("1151.78") // ZEC: one contract = 0.01 * 1151.78 = $11.52

	if got := sizeToContracts(dec("2"), dec("1"), price, inst); !got.IsZero() {
		t.Fatalf("at 1x a $2 budget cannot afford one $11.52 contract, got %s", got)
	}
	if got := sizeToContracts(dec("2"), dec("10"), price, inst); !got.Equal(dec("1")) {
		t.Fatalf("at 10x a $2 budget affords one contract, got %s", got)
	}
}
