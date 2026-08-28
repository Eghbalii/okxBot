// Command trader runs the live trading loop: RL inference + risk-checked order execution.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/rest"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/risk"
	"github.com/eghbalii/okxBot/go-engine/internal/rlclient"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	if cfg.OKX.APIKey == "" || cfg.OKX.APISecret == "" || cfg.OKX.APIPassphrase == "" {
		logger.Error("missing OKX API credentials; set OKX_API_KEY/OKX_API_SECRET/OKX_API_PASSPHRASE")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	restClient := rest.New(cfg.OKX.RESTBaseURL, cfg.OKX.APIKey, cfg.OKX.APISecret, cfg.OKX.APIPassphrase, cfg.OKX.Simulated)
	rlClient := rlclient.New(cfg.RLService.URL)

	// Mode selects which account_equity row this process's balance timeline is recorded under, and
	// gates the auto-reset behavior: "real" is never auto-topped-up when drained (CLAUDE.md §15.7).
	//
	// Derived from OKX.Simulated rather than configured separately, deliberately: the mode must
	// never be able to disagree with the credentials actually in use. A separate `mode: demo`
	// setting alongside real API keys would record real losses in the demo account's timeline and,
	// worse, make them eligible for the paper/demo auto-reset — real money silently "topped up" in
	// the books. Tying the two together makes that combination unrepresentable.
	mode := "real"
	if cfg.OKX.Simulated {
		mode = "demo"
	}

	// CLAUDE.md §15.7/§15.6: real money is a deliberate step, taken only after paper (and ideally
	// demo) show a real win rate. Require it to be stated outright rather than reached by leaving
	// OKX_SIMULATED_TRADING unset — an unset env var is the single easiest way to end up live by
	// accident, and everything else in this stack defaults to simulated.
	if mode == "real" && !cfg.Trading.AllowRealMoney {
		logger.Error("refusing to start against REAL money: OKX_SIMULATED_TRADING is not enabled " +
			"and trading.allow_real_money is false. Set trading.allow_real_money: true only when " +
			"you intend to trade real capital (CLAUDE.md §15.6's paper -> demo -> real progression).")
		os.Exit(1)
	}
	logger.Info("trading mode resolved", "mode", mode, "simulated", cfg.OKX.Simulated)

	// Postgres is OPTIONAL here, unlike in cmd/paper-trader: it's used only to record the equity
	// timeline for the panel's chart (CLAUDE.md §15.7). A database problem must never stop a live
	// trading loop, so a failed connection degrades to "no timeline recorded" rather than exiting.
	var repo port.Repository
	if pgRepo, err := postgres.New(ctx, cfg.Postgres.DSN); err != nil {
		logger.Warn("equity timeline disabled: could not connect to postgres", "error", err)
	} else {
		defer pgRepo.Close()
		if err := pgRepo.Migrate(ctx); err != nil {
			logger.Warn("equity timeline: migrations failed", "error", err)
		}
		repo = pgRepo
	}

	balances, err := restClient.GetBalance("USDT")
	if err != nil {
		logger.Error("failed to fetch initial balance", "error", err)
		os.Exit(1)
	}
	startEquity := decimal.Zero
	if len(balances) > 0 {
		startEquity = balances[0].Eq
	}

	riskLimits := risk.Limits{
		MaxLeverage:             cfg.Risk.MaxLeverage,
		MaxPositionNotionalUSD:  cfg.Risk.MaxPositionNotionalUSD,
		MaxDailyDrawdownPct:     cfg.Risk.MaxDailyDrawdownPct,
		MinLiquidationBufferPct: cfg.Risk.MinLiquidationBufferPct,
	}
	riskManager := risk.NewManager(riskLimits, startEquity)

	logger.Info("starting trader", "instIds", cfg.Trading.InstIDs, "simulated", cfg.OKX.Simulated)

	// Run one Trader per configured instrument, each polling independently.
	errCh := make(chan error, len(cfg.Trading.InstIDs))
	for _, instID := range cfg.Trading.InstIDs {
		trader := &usecase.Trader{
			InstID:       instID,
			Exchange:     restClient,
			Model:        rlClient,
			RiskManager:  riskManager,
			PollInterval: time.Duration(cfg.Trading.PollIntervalSec) * time.Second,
			TdMode:       cfg.Trading.TdMode,
			PosMode:      cfg.Trading.PosMode,
			MinOrderUSD:  cfg.Trading.MinOrderUSD,
			Logger:       logger,
			// Same roster the paper-trading path builds observations from (CLAUDE.md §15.1), so
			// live and demo observations match the shape the model was trained on.
			ActiveTokens:      cfg.Trading.InstIDs,
			Mode:              mode,
			AccountInitialUSD: cfg.Account.InitialUSD,
			Repo:              repo,
		}
		go func() { errCh <- trader.Run(ctx) }()
	}

	<-ctx.Done()
	logger.Info("shutting down trader")
}
