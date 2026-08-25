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

	"github.com/rez/okxBot/go-engine/internal/config"
	"github.com/rez/okxBot/go-engine/internal/okx/rest"
	"github.com/rez/okxBot/go-engine/internal/risk"
	"github.com/rez/okxBot/go-engine/internal/rlclient"
	"github.com/rez/okxBot/go-engine/internal/usecase"
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
		}
		go func() { errCh <- trader.Run(ctx) }()
	}

	<-ctx.Done()
	logger.Info("shutting down trader")
}
