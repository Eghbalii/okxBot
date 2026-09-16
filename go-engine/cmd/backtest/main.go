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

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/backtest"
	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
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
		dry     = flag.Bool("dry", false, "score only — print the per-strategy summary and write no dataset")
		// Sweepable, because the first screening run found 35 of 36 strategies losing money — at
		// which point the question stops being "which strategy" and becomes "is the trade economics
		// survivable at all". A flag means one binary answers that in minutes rather than a rebuild
		// per hypothesis.
		lev   = flag.Float64("leverage", 0, "override risk.max_leverage")
		maxRR = flag.Float64("max-rr", 0, "override rl_clamps.max_tp_sl_ratio")
		// The measurement that matters most, from the first full screening: Clamps.Apply only CAPS
		// the ratio, it never raises a modest proposal toward the cap — so with MinTPSLRatio at 1.5
		// the realized ratio came out at 1.73:1, whose breakeven win rate (36.6%) sits just above
		// the 36.0% actually achieved. Raising the floor is the one change that moves that number
		// without touching a single strategy.
		minRR = flag.Float64("min-rr", 0, "override rl_clamps.min_tp_sl_ratio")
		fee   = flag.Float64("fee", -1, "override the taker fee rate (e.g. 0.0002)")
		// The 15%% loss cap (§19.2) bounds stop DISTANCE as maxLoss/leverage, which means leverage and
		// stop width are not independent: at 10x the cap allows 1.50%% of price, at 50x only 0.30%% —
		// one average 5m bar. A stop that narrow is noise-width, crossed routinely without the trade
		// being wrong, so raising leverage under a fixed cap tightens the stop into the noise rather
		// than simply scaling the position. Sweepable so that coupling can be measured apart from
		// leverage itself.
		maxLoss = flag.Float64("max-loss", 0, "override rl_clamps.max_loss_pct")
		// Strategy parameter overrides as name=value pairs, applied to every kind in the run; a kind
		// ignores names it does not declare, so one flag sweeps a parameter across whichever
		// strategies actually have it.
		//
		// Added because the screening's most interesting finding is a parameter question:
		// stoch_cross reaches its target on 60%% of trades and still loses, because that target is
		// 1%% against a 1.5%% stop — a 0.67 reward:risk whose breakeven is 64%%. Whether widening it
		// helps depends on how fast the win rate falls as the target moves away, which a sweep
		// measures and arithmetic cannot.
		params = flag.String("param", "", "strategy parameter overrides, name=value[,name=value]")
		// Found 2026-09-15: positionSlots used a fixed `instruments * 4` guess regardless of how
		// many kinds were actually running, sized for production's small live roster. A screening
		// run passes dozens of kinds at once — at 10 instruments x 34 kinds that is 340 real
		// concurrent slots against the guess's 40, an 8.5x overcommitment per position that made
		// every dollar figure in a screening run's dataset too large by roughly that factor. This
		// override lets the account scale with the roster actually being run, independent of
		// production's real $40 (cfg.Account.InitialUSD), which stays what a live deploy uses.
		account = flag.Float64("account", 0, "override account.initial_usd (default: config's real value)")
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

	// A screening run writes no dataset. Scoring 26 strategies to decide which few to keep produces
	// a 125MB file that is discarded the moment the answer is read, and writing it is most of the
	// run's wall time.
	var sink backtest.Sink = backtest.DiscardSink{}
	if !*dry {
		js, err := backtest.NewJSONLSink(*out)
		if err != nil {
			logger.Error("open output", "path", *out, "error", err)
			os.Exit(1)
		}
		sink = js
		defer func() {
			if err := js.Close(); err != nil {
				logger.Error("close output", "error", err)
			}
		}()
	}
	overrides, err := parseParams(*params)
	if err != nil {
		logger.Error("parse -param", "error", err)
		os.Exit(1)
	}

	runner := &backtest.Runner{
		Cfg: backtest.Config{
			InstIDs: instIDs,
			Bars:    barList,
			Kinds:   splitList(*kinds),
			From:    from,
			To:      to,
			// The same account shape live paper trading runs, so the policy learns sizing against
			// the economics it will actually be served (§15.6) — unless -account overrides it for a
			// screening run whose roster is wider than production's, where $40 split across every
			// slot would size positions far below anything meaningful (see -account's own comment).
			InitialUSD:     overrideDec(cfg.Account.InitialUSD, *account),
			MaxLeverage:    overrideDec(cfg.Risk.MaxLeverage, *lev),
			PositionSlots:  positionSlots(instIDs, splitList(*kinds)),
			MaxPositionPct: cfg.Account.MaxPositionPct,
			CandleWindow:   cfg.PaperTrading.CandleLimit,
			Params:         overrides,
			// The production clamps. Training without them would let the policy learn placements Go
			// silently rejects — and score them as though they had been taken (§19.2, §45).
			Clamps: conductor.Clamps{
				MinSLDistPct: cfg.PaperTrading.RLClamps.MinSLDistPct,
				MaxSLDistPct: cfg.PaperTrading.RLClamps.MaxSLDistPct,
				MinTPSLRatio: overrideDec(cfg.PaperTrading.RLClamps.MinTPSLRatio, *minRR),
				MaxTPSLRatio: overrideDec(cfg.PaperTrading.RLClamps.MaxTPSLRatio, *maxRR),
				MaxLossPct:   overrideDec(cfg.PaperTrading.RLClamps.MaxLossPct, *maxLoss),
			},
		},
		Src:    repo,
		Sink:   sink,
		Logger: logger,
	}

	if *fee >= 0 {
		backtest.SetTakerFee(decimal.NewFromFloat(*fee))
	}

	logger.Info("backtest starting",
		"instruments", instIDs, "bars", barList, "out", *out,
		"account", runner.Cfg.InitialUSD, "maxLeverage", runner.Cfg.MaxLeverage,
		"positionSlots", runner.Cfg.PositionSlots)

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
// pairs (§32.4) — one slot per kind actually being run, not a guess.
//
// Found 2026-09-15: this used to hardcode `instruments * 4`, a nominal per-token strategy count
// sized for production's small live roster. A screening run passes dozens of kinds at once — one
// real run (10 instruments, 34 kinds after excluding the 12 V1s superseded by a V2) has 340 real
// concurrent slots against the guess's 40, an 8.5x overcommitment that inflated every position
// (and therefore every dollar PnL/reward figure in the resulting dataset) by roughly that factor,
// while every strategy's WIN RATE stayed correct — the trade-selection logic never depended on
// this number, only its dollar sizing did. Slots is instruments x kinds actually running, matching
// production's own rule exactly rather than approximating it.
func positionSlots(instIDs, kinds []string) int {
	nKinds := len(kinds)
	if nKinds == 0 {
		// -kinds empty means "every registered kind" (main's own default, splitList("")==nil),
		// so the divisor must count the same set the run will actually iterate.
		nKinds = len(strategy.Factories)
	}
	n := len(instIDs) * nKinds
	if n <= 0 {
		return 1
	}
	return n
}

// overrideDec returns v as a decimal when it is positive, else the configured value. Zero means
// "not set" rather than "zero leverage", which no caller could mean.
func overrideDec(cfgVal decimal.Decimal, v float64) decimal.Decimal {
	if v > 0 {
		return decimal.NewFromFloat(v)
	}
	return cfgVal
}

// parseParams turns "a=1,b=2" into decimal overrides.
func parseParams(s string) (map[string]decimal.Decimal, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]decimal.Decimal{}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("bad override %q, want name=value", pair)
		}
		v, err := decimal.NewFromString(strings.TrimSpace(kv[1]))
		if err != nil {
			return nil, fmt.Errorf("bad value in %q: %w", pair, err)
		}
		out[strings.TrimSpace(kv[0])] = v
	}
	return out, nil
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
