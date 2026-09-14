// Command backtest builds the RL model's warm-start training set by replaying stored candles
// (docs/RL_V8_PLAN.md).
//
// Its own binary, and its own package, because this is the piece someone cloning the repository
// runs to train a model from scratch — the operator's explicit reason for wanting it as a named
// component rather than a flag on an existing service. It touches no exchange and places no orders:
// it reads the candles table, runs the real strategies over it, and writes one JSON line per
// completed trade.
//
//	go run ./cmd/backtest -out data/warmstart.jsonl
//	go run ./cmd/backtest -inst SOL,ZEC -bars 5m,15m -from 2026-08-01 -out data/warmstart.jsonl
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/backtest"
	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

func main() {
	var (
		cfgPath = flag.String("config", "", "config file (defaults to the usual search path)")
		out     = flag.String("out", "data/warmstart.jsonl", "where to write the dataset")
		insts   = flag.String("inst", "", "comma-separated instruments (default: every instrument with candles)")
		bars    = flag.String("bars", "", "comma-separated bars (default: paper_trading.bars)")
		kinds   = flag.String("kinds", "", "comma-separated strategy kinds (default: every registered kind)")
		fromStr = flag.String("from", "", "start date, YYYY-MM-DD (default: the earliest candle held)")
		toStr   = flag.String("to", "", "end date, YYYY-MM-DD (default: now)")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, err := postgres.New(ctx, cfg.Postgres.DSN)
	if err != nil {
		logger.Error("connect postgres", "error", err)
		os.Exit(1)
	}
	defer repo.Close()

	instIDs := splitList(*insts)
	if len(instIDs) == 0 {
		instIDs = cfg.Trading.InstIDs
	}
	barList := splitList(*bars)
	if len(barList) == 0 {
		barList = cfg.PaperTrading.Bars
	}
	if len(instIDs) == 0 || len(barList) == 0 {
		logger.Error("nothing to replay", "instruments", len(instIDs), "bars", len(barList))
		os.Exit(1)
	}

	from, err := parseDate(*fromStr)
	if err != nil {
		logger.Error("parse -from", "error", err)
		os.Exit(1)
	}
	to, err := parseDate(*toStr)
	if err != nil {
		logger.Error("parse -to", "error", err)
		os.Exit(1)
	}

	sink, err := backtest.NewJSONLSink(*out)
	if err != nil {
		logger.Error("open output", "path", *out, "error", err)
		os.Exit(1)
	}
	// Deferred AND checked: a buffered writer silently drops up to a megabyte of samples if it is
	// never flushed, and a dataset short by its last thousand trades looks exactly like one that
	// ended there.
	defer func() {
		if err := sink.Close(); err != nil {
			logger.Error("close output", "error", err)
		}
	}()

	runner := &backtest.Runner{
		Cfg: backtest.Config{
			InstIDs: instIDs,
			Bars:    barList,
			Kinds:   splitList(*kinds),
			From:    from,
			To:      to,
			// The same account shape live paper trading runs, so the policy learns sizing against
			// the economics it will actually be served (§15.6).
			InitialUSD:    cfg.Account.InitialUSD,
			MaxLeverage:   cfg.Risk.MaxLeverage,
			PositionSlots: positionSlots(cfg, instIDs),
			CandleWindow:  cfg.PaperTrading.CandleLimit,
			// The production clamps. Training without them would let the policy learn placements Go
			// silently rejects — and score them as though they had been taken (§19.2, §45).
			Clamps: conductor.Clamps{
				MinSLDistPct: cfg.PaperTrading.RLClamps.MinSLDistPct,
				MaxSLDistPct: cfg.PaperTrading.RLClamps.MaxSLDistPct,
				MinTPSLRatio: cfg.PaperTrading.RLClamps.MinTPSLRatio,
				MaxTPSLRatio: cfg.PaperTrading.RLClamps.MaxTPSLRatio,
				MaxLossPct:   cfg.PaperTrading.RLClamps.MaxLossPct,
			},
		},
		Src:    repo,
		Sink:   sink,
		Logger: logger,
	}

	logger.Info("backtest starting",
		"instruments", instIDs, "bars", barList, "out", *out,
		"account", cfg.Account.InitialUSD, "maxLeverage", cfg.Risk.MaxLeverage)

	started := time.Now()
	res, err := runner.Run(ctx)
	if err != nil {
		logger.Error("backtest failed", "error", err)
		os.Exit(1)
	}

	// Printed as JSON as well as logged: this summary is what the operator reads to decide whether
	// the dataset is worth training on, and a run that produced mostly losses or discarded most of
	// its signals should be obvious before a model is built from it.
	summary, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(summary))
	logger.Info("backtest complete",
		"samples", res.Samples, "winRate", res.WinRate, "pnl", res.TotalPnL,
		"meanReward", res.MeanReward, "elapsed", time.Since(started).Round(time.Second))
}

// positionSlots mirrors the live sizing divisor: equity is split evenly across (strategy, token)
// pairs (§32.4). Approximated here as instruments x a nominal per-token strategy count, because the
// dataset deliberately runs every registered kind while live trading runs a small roster — using
// the live divisor would size dataset positions far larger than production ever opens.
func positionSlots(cfg *config.Config, instIDs []string) int {
	n := len(instIDs) * 4
	if n <= 0 {
		return 1
	}
	return n
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseDate(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, nil
	}
	return time.Parse("2006-01-02", s)
}
