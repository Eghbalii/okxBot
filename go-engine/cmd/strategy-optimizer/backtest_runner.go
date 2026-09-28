package main

import (
	"context"
	"log/slog"

	"github.com/eghbalii/okxBot/go-engine/internal/backtest"
	"github.com/eghbalii/okxBot/go-engine/internal/optimizer"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
)

// newBacktestRunner adapts *postgres.Repository into an optimizer.BacktestRunner — a fresh
// backtest.Runner is constructed per call rather than reused, since Runner carries per-run
// mutable state (account, peak, records) that must never leak between two different candidates'
// backtests. backtest.DiscardSink is used throughout: this pipeline only needs the summary
// Result, never the JSONL dataset cmd/backtest itself writes for RL warm-start training.
func newBacktestRunner(repo *postgres.Repository, logger *slog.Logger) optimizer.BacktestFunc {
	return func(ctx context.Context, cfg backtest.Config) (backtest.Result, error) {
		runner := &backtest.Runner{
			Cfg:    cfg,
			Src:    repo,
			Sink:   backtest.DiscardSink{},
			Logger: logger,
		}
		return runner.Run(ctx)
	}
}
