package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// fundingRateFetcher is the narrow slice of port.ExchangeClient this poller needs — a data-loading
// job should not be able to place an order, and the type system enforces that rather than the
// implementation being careful (same reasoning as port.HistoryCandleFetcher, CLAUDE.md §17).
type fundingRateFetcher interface {
	GetFundingRateHistory(instID string, limit int) ([]domain.FundingRate, error)
}

// fundingRateSaver is the narrow slice of port.Repository this poller needs, for the same reason
// as fundingRateFetcher above — and so tests can fake just this one method instead of the entire
// (much larger) Repository interface.
type fundingRateSaver interface {
	SaveFundingRates(ctx context.Context, rates []port.FundingRate) error
}

// runFundingRatePoller keeps the funding_rates table current (CLAUDE.md, 2026-09-06): OKX's real
// funding rate swings roughly 60x between calm and volatile periods and, on the X-Perp instruments
// this project actually trades, can even run the OPPOSITE sign from the standard market — a single
// config constant cannot track either, which is the whole reason this exists instead of one.
//
// Resolves each short internal symbol (e.g. "BTC") to its real OKX instId via symbolMap before
// calling the exchange, and stores rows keyed by the SHORT symbol — matching every other table in
// this codebase (CLAUDE.md §33.4's "short symbols everywhere, OKX's wire format only at the
// boundary" design). One instrument failing to resolve or fetch is logged and skipped, never fatal
// to the loop — a funding-rate gap degrades PnL precision, it must never take down paper trading.
func runFundingRatePoller(ctx context.Context, client fundingRateFetcher, repo fundingRateSaver, symbolMap okx.SymbolMap, instIDs []string, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = time.Hour
	}
	poll := func() {
		for _, symbol := range instIDs {
			realInstID, err := symbolMap.Resolve(symbol)
			if err != nil {
				logger.Warn("funding rate poll: symbol resolution failed", "symbol", symbol, "error", err)
				continue
			}
			rates, err := client.GetFundingRateHistory(realInstID, 10)
			if err != nil {
				logger.Warn("funding rate poll: fetch failed", "symbol", symbol, "instId", realInstID, "error", err)
				continue
			}
			stored := make([]port.FundingRate, len(rates))
			for i, r := range rates {
				// Stored under the SHORT symbol, not r.InstID (which is OKX's real wire-format
				// instId) — SumFundingCost looks rows up by the same short symbol paper_orders
				// itself uses.
				stored[i] = port.FundingRate{InstID: symbol, FundingTime: r.FundingTime, FundingRate: r.FundingRate}
			}
			if err := repo.SaveFundingRates(ctx, stored); err != nil {
				logger.Warn("funding rate poll: save failed", "symbol", symbol, "error", err)
				continue
			}
			logger.Info("funding rate poll: saved", "symbol", symbol, "count", len(stored))
		}
	}

	poll() // once immediately at startup, not only after the first tick — a fresh process should
	// not run for a full interval on entirely stale/absent funding data.
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}
