package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/shopspring/decimal"
)

type fakeFundingFetcher struct {
	rates map[string][]domain.FundingRate
	err   error
	calls []string // real instIDs the poller actually asked for
}

func (f *fakeFundingFetcher) GetFundingRateHistory(instID string, limit int) ([]domain.FundingRate, error) {
	f.calls = append(f.calls, instID)
	if f.err != nil {
		return nil, f.err
	}
	return f.rates[instID], nil
}

type fakeFundingSaver struct {
	saved []port.FundingRate
}

func (f *fakeFundingSaver) SaveFundingRates(ctx context.Context, rates []port.FundingRate) error {
	f.saved = append(f.saved, rates...)
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestRunFundingRatePoller_ResolvesSymbolBeforeFetching confirms the poller calls the exchange
// with OKX's real wire-format instId (via symbolMap), never the short internal symbol — mirroring
// the same resolve-at-the-boundary discipline CLAUDE.md §33.4 requires of every OKX call site.
func TestRunFundingRatePoller_ResolvesSymbolBeforeFetching(t *testing.T) {
	fetcher := &fakeFundingFetcher{rates: map[string][]domain.FundingRate{}}
	saver := &fakeFundingSaver{}
	symbolMap := okx.SymbolMap{"BTC": "BTC-USD_UM_XPERP-310404"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runFundingRatePoller(ctx, fetcher, saver, symbolMap, []string{"BTC"}, time.Hour, testLogger())
		close(done)
	}()
	cancel()
	<-done

	if len(fetcher.calls) != 1 || fetcher.calls[0] != "BTC-USD_UM_XPERP-310404" {
		t.Errorf("expected exactly one call with the resolved instId, got %v", fetcher.calls)
	}
}

// TestRunFundingRatePoller_StoresUnderShortSymbol confirms saved rows use the SHORT symbol
// ("BTC"), not OKX's own instId field on the returned rate — SumFundingCost looks rows up by the
// same short symbol paper_orders itself uses, so storing under the wire-format instId would make
// every lookup silently return nothing.
func TestRunFundingRatePoller_StoresUnderShortSymbol(t *testing.T) {
	now := time.Now()
	fetcher := &fakeFundingFetcher{rates: map[string][]domain.FundingRate{
		"BTC-USD_UM_XPERP-310404": {
			{InstID: "BTC-USD_UM_XPERP-310404", FundingTime: now, FundingRate: decimal.RequireFromString("-0.0002")},
		},
	}}
	saver := &fakeFundingSaver{}
	symbolMap := okx.SymbolMap{"BTC": "BTC-USD_UM_XPERP-310404"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runFundingRatePoller(ctx, fetcher, saver, symbolMap, []string{"BTC"}, time.Hour, testLogger())
		close(done)
	}()
	cancel()
	<-done

	if len(saver.saved) != 1 {
		t.Fatalf("expected 1 saved rate, got %d", len(saver.saved))
	}
	if saver.saved[0].InstID != "BTC" {
		t.Errorf("expected stored InstID='BTC' (the short symbol), got %q", saver.saved[0].InstID)
	}
	if !saver.saved[0].FundingRate.Equal(decimal.RequireFromString("-0.0002")) {
		t.Errorf("expected the rate value to pass through unchanged, got %s", saver.saved[0].FundingRate)
	}
}

// TestRunFundingRatePoller_OneSymbolFailureDoesNotBlockOthers confirms a symbol that fails to
// resolve or fetch is skipped, not fatal — a funding-rate gap for one instrument must never stop
// every other instrument's rates from being polled and saved.
func TestRunFundingRatePoller_OneSymbolFailureDoesNotBlockOthers(t *testing.T) {
	now := time.Now()
	fetcher := &fakeFundingFetcher{rates: map[string][]domain.FundingRate{
		"ETH-USD_UM_XPERP-310404": {
			{InstID: "ETH-USD_UM_XPERP-310404", FundingTime: now, FundingRate: decimal.RequireFromString("0.0001")},
		},
	}}
	saver := &fakeFundingSaver{}
	// "MISSING" has no symbolMap entry at all — Resolve must fail for it, and the loop must still
	// reach "ETH" afterward.
	symbolMap := okx.SymbolMap{"ETH": "ETH-USD_UM_XPERP-310404"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runFundingRatePoller(ctx, fetcher, saver, symbolMap, []string{"MISSING", "ETH"}, time.Hour, testLogger())
		close(done)
	}()
	cancel()
	<-done

	if len(saver.saved) != 1 || saver.saved[0].InstID != "ETH" {
		t.Errorf("expected ETH to still be saved despite MISSING failing to resolve, got %+v", saver.saved)
	}
}
