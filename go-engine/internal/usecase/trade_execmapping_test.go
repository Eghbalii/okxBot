package usecase

import (
	"testing"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/risk"
)

// Mirrors realtrader_execmapping_test.go's coverage for the same gap in the older, legacy Trader
// path (CLAUDE.md §27, 2026-09-04: fixed for consistency even though this path is being phased
// out in favor of BotTrader, per explicit operator instruction).

func TestTraderExecInstID_FallsBackToInstIDWhenUnset(t *testing.T) {
	tr := &Trader{InstID: "BTC-USDT-SWAP"}
	if got := tr.execInstID(); got != "BTC-USDT-SWAP" {
		t.Errorf("execInstID() = %q, want BTC-USDT-SWAP", got)
	}
}

func TestTraderExecInstID_UsesExecInstIDWhenSet(t *testing.T) {
	tr := &Trader{InstID: "BTC-USDT-SWAP", ExecInstID: "BTC-USD_UM_XPERP-310404"}
	if got := tr.execInstID(); got != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("execInstID() = %q, want BTC-USD_UM_XPERP-310404", got)
	}
}

func TestTraderExecInstType_DefaultsToSwap(t *testing.T) {
	tr := &Trader{}
	if got := tr.execInstType(); got != "SWAP" {
		t.Errorf("execInstType() = %q, want SWAP", got)
	}
}

func TestTraderSettleCcy_DefaultsToUSDT(t *testing.T) {
	tr := &Trader{}
	if got := tr.settleCcy(); got != "USDT" {
		t.Errorf("settleCcy() = %q, want USDT", got)
	}
}

// TestStep_UsesExecInstIDNotMarketDataInstID confirms PlaceOrder/SetLeverage target ExecInstID,
// not InstID, when the two differ — the concrete bug found live: an order attempted against
// BTC-USDT-SWAP would have been rejected outright (50124) for an account only eligible to trade
// BTC-USD_UM_XPERP-310404.
func TestStep_UsesExecInstIDNotMarketDataInstID(t *testing.T) {
	exchange := &fakeExchangeClient{
		ticker:     domain.Ticker{Last: dec("80000")},
		positions:  []domain.Position{{InstID: "BTC-USD_UM_XPERP-310404", Pos: dec("0"), Lever: dec("1")}},
		balances:   []domain.Balance{{Ccy: "USDC", Eq: dec("1000")}},
		instrument: &domain.Instrument{CtVal: dec("0.0001"), LotSz: dec("1")},
	}
	model := &fakeModelClient{action: domain.Action{
		Action: domain.ActionOpen, Side: "buy", SizePct: dec("1"), LeverageFrac: dec("0.5"),
	}}

	limits := risk.Limits{
		MaxLeverage: dec("5"), MaxPositionNotionalUSD: dec("1000"),
		MaxDailyDrawdownPct: dec("50"), MinLiquidationBufferPct: dec("1"),
	}
	riskManager := risk.NewManager(limits, dec("1000"))

	trader := newTestTrader(exchange, model, riskManager)
	trader.ExecInstID = "BTC-USD_UM_XPERP-310404"
	trader.ExecInstType = "FUTURES"
	trader.SettleCcy = "USDC"

	stepThroughExecute(t, trader, exchange, model.action)

	if len(exchange.placedOrders) != 1 {
		t.Fatalf("expected exactly 1 order placed, got %d", len(exchange.placedOrders))
	}
	if exchange.placedOrders[0].InstID != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("PlaceOrder InstID = %q, want BTC-USD_UM_XPERP-310404", exchange.placedOrders[0].InstID)
	}
	if len(exchange.leverageCalls) != 1 {
		t.Fatalf("expected exactly 1 leverage call, got %d", len(exchange.leverageCalls))
	}
	if exchange.leverageCalls[0].InstID != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("SetLeverage InstID = %q, want BTC-USD_UM_XPERP-310404", exchange.leverageCalls[0].InstID)
	}
}
