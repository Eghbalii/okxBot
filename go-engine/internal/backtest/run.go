package backtest

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

// Runner replays one instrument+bar's history against a set of strategies.
//
// One instrument at a time, sharing a simulated account across them, because that is how the live
// engines work: one shared balance (§15.6), one position per (strategy, instrument).
type Runner struct {
	Cfg    Config
	Src    CandleSource
	Sink   Sink
	Logger *slog.Logger

	// account is the shared simulated balance, and peak its high-water mark — the drawdown penalty
	// measures from the peak, not the starting balance (§32.2's reason).
	account decimal.Decimal
	peak    decimal.Decimal

	records map[string]*stratRecord
	result  Result

	// nextOrderID numbers simulated trades. Not cosmetic: the learner pairs a decision with the
	// outcome that lands hours later BY ORDER ID (§15.11), so a dataset whose samples share an id —
	// or carry none — would let one trade's reward train another trade's decision. The live
	// validator rejects a terminal call without one for exactly this reason.
	nextOrderID int64
}

// Run replays every configured instrument and bar, writing one sample per completed trade.
func (r *Runner) Run(ctx context.Context) (Result, error) {
	r.account = r.Cfg.InitialUSD
	r.peak = r.Cfg.InitialUSD
	r.records = map[string]*stratRecord{}
	r.nextOrderID = 0
	r.result = Result{
		ByReason:   map[string]int{},
		Skipped:    map[string]int{},
		ByStrategy: map[string]*KindStats{},
	}

	kinds := r.Cfg.Kinds
	if len(kinds) == 0 {
		for k := range strategy.Factories {
			kinds = append(kinds, k)
		}
	}

	// BTC's own history, loaded once per bar and shared by every instrument — the market-wide
	// reference block is the same series for all of them, and reloading it per instrument would
	// read the same rows N times.
	btc := map[string][]domain.Candle{}
	for _, bar := range r.Cfg.Bars {
		w, err := loadCandles(ctx, r.Src, btcSymbol, bar, r.Cfg.From, r.Cfg.To)
		if err != nil {
			return r.result, fmt.Errorf("load btc %s: %w", bar, err)
		}
		if len(w) == 0 {
			return r.result, fmt.Errorf("no BTC history for bar %s — the market-wide reference "+
				"block cannot be built, and a zeroed one would read as 'BTC is flat and "+
				"uncorrelated' rather than as missing data", bar)
		}
		btc[bar] = w
	}

	for _, instID := range r.Cfg.InstIDs {
		for _, bar := range r.Cfg.Bars {
			if err := r.runOne(ctx, instID, bar, kinds, btc[bar]); err != nil {
				// One instrument's failure must not abandon the rest of the dataset — the §17
				// precedent for partial failure being expected rather than fatal.
				r.Logger.Warn("backtest: instrument failed", "instId", instID, "bar", bar, "error", err)
				r.result.Skipped["instrument_error"]++
			}
		}
	}

	if r.result.Samples > 0 {
		r.result.WinRate = float64(r.result.Wins) / float64(r.result.Samples)
		r.result.MeanReward = r.result.MeanReward / float64(r.result.Samples)
	}
	for _, k := range r.result.ByStrategy {
		if k.Trades == 0 {
			continue
		}
		n := float64(k.Trades)
		k.WinRate = float64(k.Wins) / n
		k.MeanReward = k.rewardSum / n
		k.AvgHoldBars = float64(k.holdSum) / n
		k.PnLPerTrade = k.PnLUSD / n
	}
	return r.result, nil
}

// btcSymbol is the symbol BTC's candles are stored under. Explicit for the same reason
// cmd/paper-trader's is: the symbol changed once already (§33.4), and a literal buried in a call
// would have gone quietly wrong rather than failing.
const btcSymbol = "BTC"

func (r *Runner) runOne(ctx context.Context, instID, bar string, kinds []string, btcAll []domain.Candle) error {
	candles, err := loadCandles(ctx, r.Src, instID, bar, r.Cfg.From, r.Cfg.To)
	if err != nil {
		return err
	}
	minLen := r.Cfg.CandleWindow
	if minLen <= 0 {
		minLen = 300
	}
	warmup := usecase.MinCandlesForIndicators
	if domain.ReturnsWindow+1 > warmup {
		warmup = domain.ReturnsWindow + 1
	}
	if len(candles) <= warmup {
		return fmt.Errorf("only %d candles, need more than %d", len(candles), warmup)
	}

	// Build each strategy fresh. WithParams on a warmed instance would carry accumulated state into
	// a differently-configured variant (§16.8's state-leak audit), and here every kind starts with
	// no history by construction.
	strats := map[string]strategy.Strategy{}
	for _, kind := range kinds {
		f, ok := strategy.Factories[kind]
		if !ok {
			r.result.Skipped["unknown_kind"]++
			continue
		}
		strats[kind] = f()
	}

	// One open position per (strategy, instrument), matching the live rule since 2026-09-14.
	open := map[string]*position{}

	// btcAt walks BTC's series alongside this instrument's, so the reference block reflects what BTC
	// was doing AT THAT MOMENT rather than at the end of the run — using the latest BTC candle for
	// a decision made weeks earlier would be lookahead, and the most damaging kind: it would tell
	// the policy the future of the whole market.
	btcIdx := 0

	for i := warmup; i < len(candles); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c := candles[i]

		for btcIdx+1 < len(btcAll) && !btcAll[btcIdx+1].Timestamp.After(c.Timestamp) {
			btcIdx++
		}
		if btcIdx < domain.ReturnsWindow {
			continue // BTC has no window yet at this point in history
		}
		btcWindow := btcAll[:btcIdx+1]

		// Close first, then open: a position that resolves on this bar must not still be holding
		// its slot when the same bar's signal is evaluated, and resolving after would let one setup
		// be counted while its predecessor was still open.
		for kind, p := range open {
			reason, exit, hit := touchReason(p.side, p.slPx, p.tpPx, c)
			if !hit {
				upl := pnlPct(p, c.Close)
				if upl.GreaterThan(p.pnlMaxPct) {
					p.pnlMaxPct = upl
				}
				if upl.LessThan(p.pnlMinPct) {
					p.pnlMinPct = upl
				}
				continue
			}
			r.closePosition(p, exit, reason, c, i, btcWindow)
			delete(open, kind)
		}

		window := candles[:i+1]
		if len(window) > minLen {
			window = window[len(window)-minLen:]
		}
		view := strategy.MarketView{Bars: map[string][]strategy.Candle{bar: window}, Bar: bar, Candles: window}

		for kind, s := range strats {
			if _, busy := open[kind]; busy {
				continue
			}
			sig, ok := evaluate(s, view)
			if !ok {
				continue
			}
			p, err := r.openPosition(instID, bar, kind, sig, window, btcWindow, i, c)
			if err != nil {
				r.result.Skipped[err.Error()]++
				continue
			}
			open[kind] = p
		}
	}
	return nil
}
