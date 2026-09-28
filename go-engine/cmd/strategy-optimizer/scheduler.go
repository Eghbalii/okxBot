package main

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/optimizer"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// tickWorkers bounds how many lineages are backtested concurrently within one tick pass. A fully
// sequential pass over thousands of lineages (measured: still mid-first-pass after 30+ minutes on
// a 2-core server, 2026-09-28) meant any single lineage was revisited so rarely that the panel
// looked frozen even though the pipeline was working. Kept modest (not "one per core") because
// each worker also holds a Postgres connection and drives real candle-replay CPU work, and this
// server has previously been pushed into an OOM/crash-loop by less concurrent load than this
// (CLAUDE.md §16.10/§35.7) — 4 is deliberately conservative on a 2-core box, not a throughput
// maximum.
const tickWorkers = 4

// scheduler owns the periodic tick loop across every configured lineage. A "lineage" is derived
// fresh on every scheduled pass from config.RiskProfiles' own Bars/StrategyKinds crossed with the
// exchange's currently paper-enabled token roster (port.Repository.ListInstruments) — never a
// static list — so a token the discovery scan admits or drops is picked up automatically, the
// same reasoning usecase.RosterFor already established for the trading services.
type scheduler struct {
	cfg    *config.Config
	repo   port.Repository
	store  *optimizer.Store
	loop   *optimizer.Loop
	logger *slog.Logger
}

// run fires one full pass across every lineage on every tick of interval, plus once immediately
// at startup so a freshly deployed service doesn't sit idle for a whole interval before doing
// anything.
func (s *scheduler) run(ctx context.Context, interval time.Duration) {
	s.tickAll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tickAll(ctx)
		}
	}
}

func (s *scheduler) tickAll(ctx context.Context) {
	lineages, err := s.lineages(ctx)
	if err != nil {
		s.logger.Error("failed to build lineages", "error", err)
		return
	}
	s.logger.Info("tick starting", "lineages", len(lineages), "workers", tickWorkers)

	work := make(chan optimizer.Lineage)
	var wg sync.WaitGroup
	for i := 0; i < tickWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for lin := range work {
				s.tickOne(ctx, lin)
			}
		}()
	}
	for _, lin := range lineages {
		select {
		case work <- lin:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return
		}
	}
	close(work)
	wg.Wait()
	s.logger.Info("tick complete", "lineages", len(lineages))
}

func (s *scheduler) tickOne(ctx context.Context, lin optimizer.Lineage) {
	profile := s.cfg.StrategyOptimizer.RiskProfiles[lin.RiskProfile]
	params, err := s.backtestParams(ctx, lin, profile)
	if err != nil {
		s.logger.Error("failed to build backtest params", "lineage", lin, "error", err)
		return
	}
	vc, err := s.store.GetValidationConfig(ctx, lin.RiskProfile)
	if err != nil {
		s.logger.Error("failed to load validation config", "riskProfile", lin.RiskProfile, "error", err)
		return
	}
	if err := s.loop.Tick(ctx, lin, params, vc); err != nil {
		s.logger.Error("tick failed", "lineage", lin, "error", err)
	}
}

// lineages derives every (kind, inst_id, bar, exchange, risk_profile) target from config —
// crossing each risk profile's configured Bars/StrategyKinds with its exchange's own currently
// paper-enabled instruments, read fresh from the database on every pass rather than cached, so a
// token enabled/disabled between passes takes effect on the very next tick.
func (s *scheduler) lineages(ctx context.Context) ([]optimizer.Lineage, error) {
	var out []optimizer.Lineage
	for riskProfile, profile := range s.cfg.StrategyOptimizer.RiskProfiles {
		kinds := profile.StrategyKinds
		if len(kinds) == 0 {
			for k := range strategy.Factories {
				kinds = append(kinds, k)
			}
		}

		instIDs := profile.InstIDs
		if len(instIDs) == 0 {
			instruments, err := s.repo.ListInstruments(ctx, port.InstrumentFilter{Exchange: profile.Exchange, Enabled: "paper"})
			if err != nil {
				return nil, err
			}
			for _, inst := range instruments {
				instIDs = append(instIDs, inst.Symbol)
			}
		}

		for _, kind := range kinds {
			for _, instID := range instIDs {
				for _, bar := range profile.Bars {
					out = append(out, optimizer.Lineage{
						Kind: kind, InstID: instID, Bar: bar,
						Exchange: profile.Exchange, RiskProfile: riskProfile,
					})
				}
			}
		}
	}
	return out, nil
}

// backtestParams builds the process-level backtest wiring for one lineage from its risk
// profile's own leverage ceiling and this process's shared production clamps (§19.2/§45 — a
// backtest run without them would validate placements the live engine would silently reject).
func (s *scheduler) backtestParams(ctx context.Context, lin optimizer.Lineage, profile config.RiskProfileConfig) (optimizer.BacktestParams, error) {
	vc, err := s.store.GetValidationConfig(ctx, lin.RiskProfile)
	if err != nil {
		return optimizer.BacktestParams{}, err
	}
	lookback, err := time.ParseDuration(normalizeLookback(vc.BacktestLookback))
	if err != nil {
		return optimizer.BacktestParams{}, err
	}
	return optimizer.BacktestParams{
		InitialUSD:     s.cfg.Account.InitialUSD,
		MaxLeverage:    profile.MaxLeverage,
		PositionSlots:  1, // one strategy, one token, one bar per backtest call — see loop.go's runAndRecord
		MaxPositionPct: s.cfg.Account.MaxPositionPct,
		Clamps:         conductorClamps(s.cfg),
		CandleWindow:   s.cfg.PaperTrading.CandleLimit,
		Lookback:       lookback,
	}, nil
}

// normalizeLookback converts the panel's day-suffixed convention ("30d") into a
// time.ParseDuration-compatible string ("720h0m0s") — kept separate from ValidationConfig's own
// storage format so the panel can keep showing/editing days without this package needing its own
// duration-string dialect. time.ParseDuration itself has no "d" unit, so "30d" would otherwise
// fail to parse with a confusing "unknown unit" error.
func normalizeLookback(s string) string {
	if len(s) == 0 || s[len(s)-1] != 'd' {
		return s
	}
	days, err := strconv.Atoi(s[:len(s)-1])
	if err != nil {
		return s // let time.ParseDuration produce the real error for a malformed value
	}
	return (time.Duration(days) * 24 * time.Hour).String()
}

func conductorClamps(cfg *config.Config) conductor.Clamps {
	return conductor.Clamps{
		MinSLDistPct: cfg.PaperTrading.RLClamps.MinSLDistPct,
		MaxSLDistPct: cfg.PaperTrading.RLClamps.MaxSLDistPct,
		MaxLossPct:   cfg.PaperTrading.RLClamps.MaxLossPct,
		MinTPSLRatio: cfg.PaperTrading.RLClamps.MinTPSLRatio,
		MaxTPSLRatio: cfg.PaperTrading.RLClamps.MaxTPSLRatio,
	}
}
