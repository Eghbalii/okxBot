// Command strategy-optimizer runs the real backtest-first strategy parameter optimization
// pipeline (rebuilt 2026-09-27, replacing the abandoned live-trial cmd/strategy-optimizer +
// cmd/strategy-tester — CLAUDE.md §21/§33.5 record why the old design never worked and was
// removed entirely rather than patched).
//
// Optuna proposes candidate parameter sets per (kind, inst_id, bar, exchange, risk_profile)
// lineage; each candidate is VALIDATED by replaying it against real historical candles through
// the EXISTING internal/backtest.Runner (docs/RL_V8_PLAN.md's warm-start engine — never
// reimplemented here); only a candidate clearing the panel-configurable thresholds is promoted
// into production's strategies/strategy_assignments, which is what actually starts it paper
// trading.
//
// A single process, exchange-agnostic by construction: RiskProfiles in config map each profile
// ("low", "high", ...) to an exchange, so a new exchange is a config addition, not a new binary
// (operator's explicit instruction, 2026-09-27: "هرچی که میسازیم باید برای تمام صرافی ها کار
// بکنه").
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/optimizer"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
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

	metrics.Serve(envOr("METRICS_ADDR", ":9103"), logger)

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

	store := optimizer.NewStore(repo.Pool())
	sidecar := optimizer.NewSidecarClient(cfg.StrategyOptimizer.URL)

	loop := &optimizer.Loop{
		Store:    store,
		Sidecar:  sidecar,
		Backtest: newBacktestRunner(repo, logger),
		Repo:     repo,
		Logger:   logger,
	}

	scheduleInterval, err := time.ParseDuration(cfg.StrategyOptimizer.ScheduleInterval)
	if err != nil {
		logger.Error("invalid strategy_optimizer.schedule_interval", "value", cfg.StrategyOptimizer.ScheduleInterval, "error", err)
		os.Exit(1)
	}

	sched := &scheduler{
		cfg:    cfg,
		repo:   repo,
		store:  store,
		loop:   loop,
		logger: logger,
	}

	// One-time catch-up sweep for candidates that passed validation before auto-promotion existed
	// (2026-09-28) — runs before the scheduler's own tick loop starts so the panel shows real
	// promoted strategies from the very first request rather than waiting for a fresh tick pass.
	leverageFor := func(riskProfile string) int {
		return int(cfg.StrategyOptimizer.RiskProfiles[riskProfile].MaxLeverage.IntPart())
	}
	if promoted, err := loop.SweepUnpromoted(ctx, leverageFor); err != nil {
		logger.Error("startup sweep failed", "error", err)
	} else {
		logger.Info("startup sweep complete", "promoted", promoted)
	}

	// One-time cleanup, same startup pass: stop paper trading from the old, untuned origin
	// strategies entirely now that the pipeline is live and promoting real candidates (explicit
	// operator instruction, 2026-09-28 — "استراتژی های قبلی رو همه رو پاک کن و فقط از استراتژی
	// های جدید ... استفاده کن"). "پاک کن" here means stop them trading, not delete rows — matches
	// this project's own established never-delete convention (origins/history stay queryable,
	// they just stop opening new positions). Idempotent: a second run finds nothing left enabled
	// to disable and reports 0.
	if disabled, err := repo.DisableOriginAssignments(ctx, "paper"); err != nil {
		logger.Error("failed to disable origin-strategy paper assignments", "error", err)
	} else {
		logger.Info("disabled origin-strategy paper assignments", "count", disabled)
		// The change above only takes effect once cmd/paper-trader itself restarts and re-reads
		// strategy_assignments (CLAUDE.md's own documented "config changes need a restart"
		// posture — assignments are loaded once at that process's startup, never polled). Kick
		// every configured paper-trader instance (one per exchange) so the cleanup is actually
		// live, not just recorded in the database. Best-effort: a paper-trader that's briefly
		// unreachable here still picks up the change on its NEXT restart for any other reason.
		if disabled > 0 {
			restartPaperTraders(logger)
		}
	}

	go sched.run(ctx, scheduleInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /run", sched.handleRunNow)
	mux.HandleFunc("GET /status", sched.handleStatus)
	mux.HandleFunc("GET /config", sched.handleGetConfig)
	mux.HandleFunc("PUT /config", sched.handlePutConfig)
	mux.HandleFunc("GET /candidates", sched.handleListCandidates)
	mux.HandleFunc("GET /candidates/by-status", sched.handleListByStatus)
	mux.HandleFunc("GET /backtest-capital", sched.handleBacktestCapital)
	mux.HandleFunc("POST /candidates/{id}/promote", sched.handlePromote)

	addr := envOr("STRATEGY_OPTIMIZER_ADDR", cfg.StrategyOptimizer.Addr)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		logger.Info("strategy-optimizer listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server failed", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// restartPaperTraders calls POST /restart on every configured paper-trader instance (one per
// exchange, same env var convention cmd/api already uses to reach them — PAPER_TRADER_SERVICE_URL
// for the default/OKX instance, PAPER_TRADER_PROFILE_URLS for additional exchange profiles) so a
// database-level strategy_assignments change actually takes effect. Each instance's own
// POST /restart handler just self-exits; Docker's restart policy relaunches it, and its own
// startup re-reads assignments fresh (CLAUDE.md's established crash-recovery pattern).
func restartPaperTraders(logger *slog.Logger) {
	urls := []string{envOr("PAPER_TRADER_SERVICE_URL", "http://paper-trader:8093")}
	for _, kv := range strings.Split(os.Getenv("PAPER_TRADER_PROFILE_URLS"), ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		if _, url, ok := strings.Cut(kv, "="); ok {
			urls = append(urls, url)
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for _, url := range urls {
		req, err := http.NewRequest(http.MethodPost, url+"/restart", nil)
		if err != nil {
			logger.Error("failed to build paper-trader restart request", "url", url, "error", err)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			logger.Warn("failed to restart paper-trader (will pick up the change on its next restart regardless)", "url", url, "error", err)
			continue
		}
		resp.Body.Close()
		logger.Info("restarted paper-trader to apply cleanup", "url", url, "status", resp.StatusCode)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
