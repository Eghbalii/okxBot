package main

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
	"github.com/eghbalii/okxBot/go-engine/internal/optimizer"
)

// fakeExchange is a minimal port.ExchangeClient covering only what seedWindow needs — this
// binary's only real OKX call.
type fakeExchange struct {
	candles        []domain.Candle
	getCandlesCall struct {
		instID string
		bar    string
		limit  int
	}
}

func (f *fakeExchange) GetTicker(instID string) (domain.Ticker, error)     { return domain.Ticker{}, nil }
func (f *fakeExchange) GetPositions(instType string) ([]domain.Position, error) {
	return nil, nil
}
func (f *fakeExchange) GetBalance(ccy string) ([]domain.Balance, error) { return nil, nil }
func (f *fakeExchange) GetCandles(instID, bar string, limit int) ([]domain.Candle, error) {
	f.getCandlesCall.instID = instID
	f.getCandlesCall.bar = bar
	f.getCandlesCall.limit = limit
	return f.candles, nil
}
func (f *fakeExchange) PlaceOrder(req domain.OrderRequest) (*domain.OrderResult, error) {
	return nil, nil
}
func (f *fakeExchange) SetLeverage(req domain.LeverageChange) error { return nil }
func (f *fakeExchange) CancelOrder(instID, ordID string) error      { return nil }
func (f *fakeExchange) GetOrder(instID, ordID string) (domain.OrderStatus, error) {
	return domain.OrderStatus{}, nil
}
func (f *fakeExchange) GetInstrument(instType, instID string) (domain.Instrument, error) {
	return domain.Instrument{}, nil
}

func (f *fakeExchange) GetFundingRateHistory(instID string, limit int) ([]domain.FundingRate, error) {
	return nil, nil
}

// TestSeedWindow_ResolvesSymbolBeforeCallingExchange confirms GetCandles is called with the real
// OKX instId (via symbolMap), not the short internal symbol this service otherwise uses to key
// its candle windows/trials/targets (CLAUDE.md §27, 2026-09-04 design).
func TestSeedWindow_ResolvesSymbolBeforeCallingExchange(t *testing.T) {
	fx := &fakeExchange{candles: []domain.Candle{{Close: decimal.RequireFromString("100")}}}
	s := &service{
		cfg:       &config.Config{},
		runCfg:    optimizer.RunConfig{CandleWindow: 10},
		exchange:  fx,
		symbolMap: okx.SymbolMap{"BTC": "BTC-USD_UM_XPERP-310404"},
		windows:   make(map[targetKey]*candleWindow),
	}
	s.cfg.Optimizer.Bar = "5m"

	if err := s.seedWindow("BTC"); err != nil {
		t.Fatalf("seedWindow failed: %v", err)
	}

	if fx.getCandlesCall.instID != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("GetCandles called with instID %q, want BTC-USD_UM_XPERP-310404", fx.getCandlesCall.instID)
	}

	w := s.instWindow("BTC")
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.candles) != 1 {
		t.Errorf("expected the window to be seeded with 1 candle, got %d", len(w.candles))
	}
}

func TestSeedWindow_UnresolvableSymbolErrors(t *testing.T) {
	fx := &fakeExchange{}
	s := &service{
		cfg:       &config.Config{},
		runCfg:    optimizer.RunConfig{CandleWindow: 10},
		exchange:  fx,
		symbolMap: okx.SymbolMap{}, // no entries
		windows:   make(map[targetKey]*candleWindow),
	}

	if err := s.seedWindow("BTC"); err == nil {
		t.Fatal("expected an error for an unresolvable symbol, got nil")
	}
}
