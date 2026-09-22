// Command okx-gateway is the single process that holds real exchange credentials for ONE exchange
// and talks to that exchange's REST API directly (CLAUDE.md §27.1). Every other Go service
// (cmd/trader, cmd/paper-trader, cmd/ingestor, cmd/strategy-tester, cmd/strategy-optimizer) calls
// this gateway over an internal HTTP API instead of constructing its own rest.Client — that is what
// makes the exchange's per-endpoint rate limits an enforceable, observable, single number instead
// of five independent, uncoordinated local limiters.
//
// cmd/trader identifies itself via the X-Gateway-Consumer header set to "trader", which gets
// strict priority (gateway.PriorityTrader) on every endpoint class under contention — no other
// consumer name receives that priority, by design (CLAUDE.md §27.1's "the trader's requests get
// strict priority over every other consumer's requests").
//
// EXCHANGE-PARAMETERIZED (2026-09-22, CLAUDE.md §46.4's own "the gateway never knew it was calling
// OKX" finding made real): gateway.Exchange ("okx" or "mexc", GATEWAY_EXCHANGE env) selects which
// adapter this ONE instance constructs and which credentials it holds. Per explicit operator
// design: running OKX and MEXC side by side for a live comparison means a SECOND deployed instance
// of this same binary with its own config/credentials, never one process juggling two exchanges'
// credentials at once — the routes/limiter/retry/metrics machinery below is already exchange-
// agnostic (proved by cmd/okx-gateway/multiexchange_test.go), so nothing in this file needed to
// fork; only the construction below needed a branch.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/gateway"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	mexcrest "github.com/eghbalii/okxBot/go-engine/internal/mexc/rest"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/rest"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/ws"
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

	svc, simulated, err := buildService(cfg, logger)
	if err != nil {
		logger.Error("failed to build gateway service", "exchange", cfg.Gateway.Exchange, "error", err)
		os.Exit(1)
	}

	// The private WebSocket: the exchange's own push of this account's position/order/balance
	// changes, republished onto the event bus for cmd/trader (2026-09-09 request). It lives here
	// because this is the only process holding real credentials (§27.1) — see accountstream.go.
	// OKX-only for now: MEXC has no private WS client built yet (internal/mexc/ws has public.go
	// only), so a MEXC gateway instance runs on the reconciliation poll alone — the same posture
	// this whole system had before the OKX private WS existed, and CLAUDE.md §27.6's own "push
	// plus poll is defence in depth, not a case for picking only one" already treats the poll
	// alone as a legitimate baseline, not a gap to work around before this can run.
	//
	// Optional and never fatal: if it cannot connect, real trading continues on the reconciliation
	// poll alone, which is the backup this is layered on top of rather than a replacement for.
	// Skipped entirely without credentials, so a demo/unconfigured deployment does not spin a
	// socket that can only ever fail to authenticate.
	if cfg.Gateway.Exchange == "okx" && cfg.OKX.APIKey != "" && len(cfg.Kafka.Brokers) > 0 {
		stream := &accountStream{
			client: &ws.PrivateClient{
				URL:        cfg.OKX.PrivateWSURL,
				APIKey:     cfg.OKX.APIKey,
				APISecret:  cfg.OKX.APISecret,
				Passphrase: cfg.OKX.APIPassphrase,
				Channels:   []string{"positions", "orders"},
				InstType:   cfg.Trading.ExecInstType,
				Logger:     logger,
			},
			publisher:      kafkastream.NewPublisher(cfg.Kafka.Brokers, TopicAccountEvents),
			instIDToSymbol: reverseSymbolMap(cfg.Trading.SymbolMap),
			logger:         logger,
		}
		go func() {
			if err := stream.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("private account stream stopped; real trading falls back to the reconciliation poll", "error", err)
			}
		}()
	} else if cfg.Gateway.Exchange == "okx" {
		logger.Info("private account stream disabled (no OKX credentials or no kafka brokers configured)")
	}

	addr := envOr("GATEWAY_ADDR", cfg.Gateway.Addr)
	if addr == "" {
		addr = "0.0.0.0:8094"
	}
	httpServer := &http.Server{Addr: addr, Handler: svc.routes()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("starting okx-gateway", "exchange", cfg.Gateway.Exchange, "addr", addr, "simulated", simulated)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("okx-gateway server exited", "error", err)
		os.Exit(1)
	}
}

// buildService constructs the exchangeClient and retry predicate for cfg.Gateway.Exchange. An
// unknown value is refused rather than silently falling back to OKX — a typo here would otherwise
// run a "MEXC" gateway that is secretly still OKX, which is a credentials-pointed-at-the-wrong-
// account class of mistake, not a cosmetic one.
func buildService(cfg *config.Config, logger *slog.Logger) (svc *service, simulated bool, err error) {
	limits := gatewayLimitsFromConfig(cfg)
	switch cfg.Gateway.Exchange {
	case "", "okx":
		client := rest.New(cfg.OKX.RESTBaseURL, cfg.OKX.APIKey, cfg.OKX.APISecret, cfg.OKX.APIPassphrase, cfg.OKX.Simulated)
		return &service{
			logger:    logger,
			client:    client,
			limiter:   gateway.NewLimiter(limits),
			retry:     gateway.DefaultRetryPolicy(),
			simulated: cfg.OKX.Simulated,
			// isRetryable left nil: the service's own isRetryableOKXError is the default.
		}, cfg.OKX.Simulated, nil
	case "mexc":
		client := mexcrest.New(cfg.MEXC.RESTBaseURL, cfg.MEXC.APIKey, cfg.MEXC.APISecret)
		return &service{
			logger:  logger,
			client:  client,
			limiter: gateway.NewLimiter(limits),
			retry:   gateway.DefaultRetryPolicy(),
			// MEXC has no demo/simulated environment equivalent to OKX's (CLAUDE.md §46, config.go's
			// own note on this) — always false, never read as "safe to place a real order without
			// meaning to" the way OKX's flag is.
			simulated:   false,
			isRetryable: mexcrest.IsRetryableError,
		}, false, nil
	default:
		return nil, false, fmt.Errorf("unknown gateway.exchange %q (want \"okx\" or \"mexc\")", cfg.Gateway.Exchange)
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
