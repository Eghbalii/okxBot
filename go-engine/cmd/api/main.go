// Command api runs the dashboard/reporting backend (CLAUDE.md §11): RL model status, strategy
// CRUD + assignments + per-strategy stats, and the positions panel across paper/demo/real trading
// modes. No auth in v1 — reachable only over the OpenVPN tunnel into the server's network, so
// Config.API.Addr should stay bound to a private interface, not 0.0.0.0.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/api"
	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
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

	repo, err := postgres.New(ctx, cfg.Postgres.DSN)
	if err != nil {
		logger.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	defer repo.Close()
	if err := repo.Migrate(ctx); err != nil {
		logger.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
	// Seed origin strategy rows here too (not just cmd/paper-trader) so the Strategies panel works
	// even if paper-trader has never run yet — the two services must not depend on start order.
	if err := strategy.SeedOrigins(ctx, repo); err != nil {
		logger.Error("failed to seed origin strategies", "error", err)
		os.Exit(1)
	}

	srv := &api.Server{
		Repo:       repo,
		RLBaseURL:  cfg.RLService.URL,
		GrafanaURL: cfg.API.GrafanaURL,
		ProcessMgr: cfg.API.ProcessMgr,
		Units:      cfg.API.Units,
		Logger:     logger,
	}

	httpServer := &http.Server{
		Addr:    cfg.API.Addr,
		Handler: srv.Routes(),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("starting api server", "addr", cfg.API.Addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("api server exited", "error", err)
		os.Exit(1)
	}
}
