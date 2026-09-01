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
	"github.com/eghbalii/okxBot/go-engine/internal/gatewayclient"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Real trading calls OKX only through cmd/okx-gateway, never rest.Client directly (CLAUDE.md
	// §27.1): the trader is the priority consumer under the gateway's per-endpoint-class rate
	// limiting, and centralizing here is what makes the aggregate OKX-side request rate an
	// enforceable, observable number instead of five independently-limited processes. "trader" is
	// the ONLY consumer identity the gateway treats as priority — this is the one call site in the
	// whole codebase allowed to use it. cmd/trader no longer holds OKX_API_KEY/SECRET/PASSPHRASE at
	// all — only the gateway does — so there is no local credential check to make here anymore.
	exchangeClient := gatewayclient.New(cfg.Gateway.URL, "trader")
	rlClient := rlclient.New(cfg.RLService.URL)

	// Mode selects which account_equity row this process's balance timeline is recorded under, and
	// gates the auto-reset behavior: "real" is never auto-topped-up when drained (CLAUDE.md §15.7).
	//
	// Derived from the GATEWAY's own reported Simulated flag (GET /health), not a local
	// OKX_SIMULATED_TRADING read — since credentials moved to the gateway (§27.1), that's the only
	// place real-vs-demo is actually known. This keeps the "mode must never disagree with the
	// credentials in use" invariant intact across the process boundary: cmd/trader has no
	// credentials of its own to disagree with, so it defers to whichever the gateway is holding.
	health, err := exchangeClient.Health(ctx)
	if err != nil {
		logger.Error("failed to reach okx-gateway for mode detection; refusing to start", "error", err)
		os.Exit(1)
	}
	mode := "real"
	if health.Simulated {
		mode = "demo"
	}

	// CLAUDE.md §15.7/§15.6: real money is a deliberate step, taken only after paper (and ideally
	// demo) show a real win rate. Require it to be stated outright rather than reached by leaving
	// OKX_SIMULATED_TRADING (on the gateway) unset — an unset env var is the single easiest way to
	// end up live by accident, and everything else in this stack defaults to simulated.
	if mode == "real" && !cfg.Trading.AllowRealMoney {
		logger.Error("refusing to start against REAL money: okx-gateway reports simulated=false " +
			"and trading.allow_real_money is false. Set trading.allow_real_money: true only when " +
			"you intend to trade real capital (CLAUDE.md §15.6's paper -> demo -> real progression).")
		os.Exit(1)
	}
	logger.Info("trading mode resolved", "mode", mode, "gatewaySimulated", health.Simulated)

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

	balances, err := exchangeClient.GetBalance("USDT")
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

	logger.Info("starting trader", "instIds", cfg.Trading.InstIDs, "gatewaySimulated", health.Simulated)

	// Run one Trader per configured instrument, each polling independently.
	errCh := make(chan error, len(cfg.Trading.InstIDs))
	for _, instID := range cfg.Trading.InstIDs {
		trader := &usecase.Trader{
			InstID:       instID,
			Exchange:     exchangeClient,
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
