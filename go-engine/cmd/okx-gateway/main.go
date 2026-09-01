// Command okx-gateway is the single process that holds real OKX credentials and talks to OKX's
// REST API directly (CLAUDE.md §27.1). Every other Go service (cmd/trader, cmd/paper-trader,
// cmd/ingestor, cmd/strategy-tester, cmd/strategy-optimizer) calls this gateway over an internal
// HTTP API instead of constructing its own rest.Client — that is what makes OKX's per-endpoint
// rate limits an enforceable, observable, single number instead of five independent,
// uncoordinated local limiters.
//
// cmd/trader identifies itself via the X-Gateway-Consumer header set to "trader", which gets
// strict priority (gateway.PriorityTrader) on every endpoint class under contention — no other
// consumer name receives that priority, by design (CLAUDE.md §27.1's "the trader's requests get
// strict priority over every other consumer's requests").
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/gateway"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/rest"
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

	metrics.Serve(envOr("METRICS_ADDR", ":9105"), logger)

	restClient := rest.New(cfg.OKX.RESTBaseURL, cfg.OKX.APIKey, cfg.OKX.APISecret, cfg.OKX.APIPassphrase, cfg.OKX.Simulated)

	limits := gatewayLimitsFromConfig(cfg)
	svc := &service{
		logger:    logger,
		client:    restClient,
		limiter:   gateway.NewLimiter(limits),
		retry:     gateway.DefaultRetryPolicy(),
		simulated: cfg.OKX.Simulated,
	}

	addr := envOr("GATEWAY_ADDR", "0.0.0.0:8094")
	httpServer := &http.Server{Addr: addr, Handler: svc.routes()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("starting okx-gateway", "addr", addr, "simulated", cfg.OKX.Simulated, "limits", limits)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("okx-gateway server exited", "error", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// gatewayLimitsFromConfig builds the per-endpoint-class limits from config.Gateway (falling back
// to gateway.DefaultLimits() per class when a class is left unconfigured/zero) — CLAUDE.md
// §27.1's explicit "build the gateway's limits as config values, not hardcoded constants"
// requirement, so a correction after checking OKX's real docs never needs a code change.
func gatewayLimitsFromConfig(cfg *config.Config) map[gateway.EndpointClass]gateway.ClassLimit {
	defaults := gateway.DefaultLimits()
	out := make(map[gateway.EndpointClass]gateway.ClassLimit, len(defaults))
	configured := map[gateway.EndpointClass]config.GatewayClassLimit{
		gateway.ClassTrade:    cfg.Gateway.Trade,
		gateway.ClassLeverage: cfg.Gateway.Leverage,
		gateway.ClassAccount:  cfg.Gateway.Account,
		gateway.ClassMarket:   cfg.Gateway.Market,
	}
	for class, def := range defaults {
		c := configured[class]
		limit := def
		if c.Capacity > 0 {
			limit.Capacity = c.Capacity
		}
		if c.Refill > 0 {
			limit.Refill = c.Refill
		}
		if c.IntervalMs > 0 {
			limit.Interval = time.Duration(c.IntervalMs) * time.Millisecond
		}
		out[class] = limit
	}
	return out
}
