package backtest

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
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

	// pnls collects every trade's realized PnL, so the significance test measures the spread from
	// the run itself rather than assuming a constant — the same strategy on a $40 account and a
	// $2,600 one has the same edge and very different absolute PnL.
	pnls []float64

	// volumeRank is each instrument's place in the run's own volume ordering, 0..1. Roster-wide, so
	// it cannot be derived inside one instrument's replay — computed once up front and read here.
	volumeRank map[string]decimal.Decimal
	// tokenTrades counts the trades the simulation has closed on each instrument so far, feeding
	// the token profile's "how much do I know here" input. Accumulated from the run's own books,
	// exactly like the strategy profile, so it never reports knowledge the policy did not have yet.
	tokenTrades map[string]int

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
	r.tokenTrades = map[string]int{}
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

	// Volume rank across the roster, from the same candles the replay reads. Roster-wide by
	// definition — one instrument's replay cannot know where it sits among the others — so it is
	// computed once here rather than inside runOne.
	r.volumeRank = volumeRanks(ctx, r.Src, r.Cfg)

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

	// AFTER the loop above, not before it. Significance reads PnLPerTrade, which is derived there —
	// computing it first silently compared zeros and reported every gap as 0.00 with t=0.00, a
	// table that looks like a finished measurement and contains none.
	if base, ok := r.result.ByStrategy[BaselineKind]; ok && base.Trades > 0 {
		r.result.Significance = SignificanceVsBaseline(r.result.ByStrategy, BaselineKind, PnLSpread(r.pnls))
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
		st := f()
		if len(r.Cfg.Params) > 0 {
			// Through WithParams rather than by assignment, so each kind clamps to its own declared
			// range and resets accumulated state — the contract §16.8's audit found 7 of 14
			// strategies violating when copied directly.
			st = st.WithParams(r.Cfg.Params)
		}
		strats[kind] = st
	}

	// One open position per (strategy, instrument), matching the live rule since 2026-09-14.
	open := map[string]*position{}

	// The last signal seen on this (instrument, bar), retained and re-attached to cadence-driven
	// updates — conductor.CarriedSignal's behaviour, which lifecycle.go feeds to every live update.
	// runOne is already scoped to one instrument+bar, so one variable is the whole map.
	var carried *domain.StrategySignal

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
				// The live cadence, applied to replayed time: an update fires once PnL has moved
				// past the threshold OR the ceiling has elapsed (conductor.ShouldUpdate, §15.12).
				// Time comes from the candle's own timestamp, so a gap in the series ages the
				// position by the real elapsed time rather than by a bar count.
				moved := upl.Sub(p.lastUpdatePnL).Abs().GreaterThanOrEqual(conductor.DefaultUpdatePnLThresholdPct)
				elapsed := c.Timestamp.Sub(p.lastUpdateAt) >= conductor.DefaultUpdateMaxInterval
				if moved || elapsed {
					r.recordUpdate(p, c, carried)
					p.lastUpdatePnL = upl
					p.lastUpdateAt = c.Timestamp
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
		// Reference carries BTC's window so a cross-market strategy can actually see it
		// (strategy.MarketView.Reference). Without this, btc_divergence would hold on every bar and
		// silently score as a strategy that never trades rather than one that was never given its
		// input — the failure mode §30.1 records a whole suite passing vacuously through.
		view := strategy.MarketView{
			Bars:      map[string][]strategy.Candle{bar: window},
			Bar:       bar,
			Candles:   window,
			Reference: btcWindow,
		}

		for kind, s := range strats {
			sig, ok := evaluate(s, view)
			if !ok {
				continue
			}
			if held, busy := open[kind]; busy {
				// A real signal, at a real timestamp, that the live engine WOULD have asked the
				// model about — §15.12 routes it as an `update` because the strategy already holds
				// this token. The backtest has no model to ask, so it cannot be traded; counting it
				// is what stops it being an invisible drop. Evaluated BEFORE the busy check (it used
				// to be after) so a held position does not hide that its strategy kept firing.
				//
				// Split by direction, because the two are different questions. Same-side is the
				// strategy repeating itself and "do nothing" is very likely right. OPPOSITE-side is
				// the strategy saying the setup has inverted while capital is committed to the old
				// one — which is exactly the moment §15.12's early-close decision exists for, and
				// the only training signal that decision could ever learn from.
				side := "buy"
				if sig.Side == strategy.Sell {
					side = "sell"
				}
				// §15.12: a strategy firing while this token already holds a position is an
				// `update`, not a new buy/sell. The live engine routes it that way; the backtest
				// used to DROP it, so 125,000 real signals — three for every trade taken — never
				// became training data at all, and the model never saw the case where a strategy
				// speaks while capital is already committed.
				// The signal as the model would receive it, built by the same path an open uses —
				// reconstructing one here would risk the two drifting apart.
				ds := r.signalProfile(kind, bar, sig, c.Close)
				r.recordUpdate(held, c, ds)
				carried = ds
				held.lastUpdatePnL = pnlPct(held, c.Close)
				held.lastUpdateAt = c.Timestamp

				if side == held.side {
					r.result.Skipped["busy_same_side"]++
				} else {
					r.result.Skipped["busy_opposite_side"]++
					// Whether the held position was WINNING at that moment. This is the number that
					// decides whether an opposite signal is worth acting on: if the trades it fires
					// against were mostly already losing, closing early would have saved money and
					// the signal carries information. If they were mostly winning, it is noise and
					// obeying it would cost money — and no reward scheme can make it useful.
					if pnlPct(held, c.Close).IsNegative() {
						r.result.Skipped["busy_opposite_while_losing"]++
					} else {
						r.result.Skipped["busy_opposite_while_winning"]++
					}
				}
				continue
			}
			// Retained before the open is attempted: conductor.NoteSignal records every signal
			// this (instId, bar) produced, whether or not it became a position, and a later
			// cadence update re-attaches it.
			carried = r.signalProfile(kind, bar, sig, c.Close)

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

// volumeRanks orders the roster by median quote volume and maps each instrument to [0,1].
//
// Median rather than mean: one 100x spike on a thin token would otherwise rank it above BTC, and
// the input is meant to say "how liquid is this normally". A rank rather than raw volume because
// the absolute figure is already carried by LogVolume24h; this answers the different question of
// where the token sits relative to what else is tradeable.
//
// Failure is not fatal. An instrument whose candles cannot be read is left out of the ordering and
// reads as rank 0, which is the truthful answer for a token nothing is known about.
func volumeRanks(ctx context.Context, src CandleSource, cfg Config) map[string]decimal.Decimal {
	type vol struct {
		inst string
		med  float64
	}
	bar := cfg.Bars[0]
	var vs []vol
	for _, instID := range cfg.InstIDs {
		w, err := loadCandles(ctx, src, instID, bar, cfg.From, cfg.To)
		if err != nil || len(w) == 0 {
			continue
		}
		q := make([]float64, 0, len(w))
		for _, c := range w {
			f, _ := c.Volume.Mul(c.Close).Float64()
			q = append(q, f)
		}
		sort.Float64s(q)
		vs = append(vs, vol{instID, q[len(q)/2]})
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].med < vs[j].med })
	out := map[string]decimal.Decimal{}
	if len(vs) == 1 {
		// A single instrument has no ordering to express. 1.0 rather than 0 — it IS the most liquid
		// thing in its own roster, and 0 would read as the least.
		out[vs[0].inst] = decimal.NewFromInt(1)
		return out
	}
	for i, v := range vs {
		out[v.inst] = decimal.NewFromFloat(float64(i) / float64(len(vs)-1))
	}
	return out
}
