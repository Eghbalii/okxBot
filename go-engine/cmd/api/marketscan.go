package main

import (
	"context"
	"log/slog"

	"github.com/eghbalii/okxBot/go-engine/internal/api"
	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/gatewayclient"
	mexcrest "github.com/eghbalii/okxBot/go-engine/internal/mexc/rest"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

// This file wires token discovery into cmd/api. It is the ONE place the config's exchange list turns
// into live clients, which is what keeps the operator's "read the exchange list from the database
// later" instruction a one-function change: the scanner itself only ever sees
// []usecase.ExchangeSource and never learns where the list came from.

// scannerAdapter converts usecase.ScanResult into the shape internal/api describes, so the api
// package does not import the scanner's concrete type just to describe its output.
type scannerAdapter struct{ inner *usecase.MarketScanner }

func (a scannerAdapter) Scan(ctx context.Context) []api.ScanResultView {
	out := make([]api.ScanResultView, 0, len(a.inner.Exchanges))
	for _, r := range a.inner.Scan(ctx) {
		out = append(out, api.ScanResultView{
			Exchange: r.Exchange, Scanned: r.Scanned,
			Candidates: r.Candidates, Admitted: r.Admitted, Err: r.Err,
		})
	}
	return out
}

// buildExchangeSources turns the configured exchange list into live clients.
//
// OKX goes through cmd/okx-gateway, not a direct REST client: the gateway is the only process
// holding OKX credentials (§27.1), and routing the scan through it means the scan's usage shares the
// same rate-limit budget and shows up in the per-consumer request counter (§40) rather than spending
// OKX's budget from somewhere nothing can see.
//
// MEXC goes direct, because there is no MEXC gateway — and for this call it needs none: all-tickers
// is unauthenticated, so no credential boundary is being crossed. That asymmetry is deliberate and
// worth stating rather than papering over: when MEXC trading is wired up it will need its own gateway
// for the authenticated half, and this is not that.
func buildExchangeSources(cfg *config.Config, logger *slog.Logger) []usecase.ExchangeSource {
	var out []usecase.ExchangeSource
	for _, ex := range cfg.Scan.Exchanges {
		src := usecase.ExchangeSource{
			Name: ex.Name, InstType: ex.InstType, QuoteSuffixes: ex.QuoteSuffixes,
		}
		switch ex.Name {
		case "okx":
			src.Client = gatewayclient.New(cfg.Gateway.URL, "api")
			src.TradesLive = true
			src.AccountExchange = "okx"
		case "mexc":
			src.Client = mexcrest.New(cfg.MEXC.RESTBaseURL, cfg.MEXC.APIKey, cfg.MEXC.APISecret)
			// paper-trader-mexc is a real, running execution instance now (2026-09-22
			// multi-exchange paper trading) — a MEXC discovery admission DOES spend a share of a
			// real account's sizing budget, so this must top up like OKX's own. AccountExchange is
			// the paper-trading profile label PAPER_EXCHANGE=MEXC_100x_1 resolves to
			// (strings.ToLower in cmd/paper-trader/main.go), not the bare exchange name "mexc" —
			// account_equity is keyed by that label, never the discovery roster's exchange name.
			src.TradesLive = true
			src.AccountExchange = "mexc_100x_1"
		default:
			// config.validateScanExchanges already refused an unknown name at startup, so reaching
			// here means a new exchange was added to that allowlist without being wired up. Log
			// loudly and skip rather than nil-panic on the first scan.
			logger.Error("scan: no client wired for exchange, skipping", "exchange", ex.Name)
			continue
		}
		out = append(out, src)
	}
	return out
}

// buildBalanceSources builds the Home page's per-exchange balance row.
//
// An exchange with no credentials gets a NIL client on purpose, which the panel renders as "not
// configured". MEXC's authenticated half has never run against a real account (§46.6), so reporting
// a zero balance for it would be a plausible-looking lie — a missing key and an empty account mean
// very different things, and the panel must be able to tell them apart.
func buildBalanceSources(cfg *config.Config) []api.ExchangeBalanceSource {
	out := []api.ExchangeBalanceSource{{
		Name: "okx", Ccy: cfg.Trading.ExecSettleCcy,
		Client: gatewayclient.New(cfg.Gateway.URL, "api"),
	}}

	mexc := api.ExchangeBalanceSource{Name: "mexc", Ccy: "USDT"}
	if cfg.MEXC.APIKey != "" && cfg.MEXC.APISecret != "" {
		mexc.Client = mexcrest.New(cfg.MEXC.RESTBaseURL, cfg.MEXC.APIKey, cfg.MEXC.APISecret)
	}
	out = append(out, mexc)
	return out
}

func newMarketScanner(cfg *config.Config, repo port.Repository, logger *slog.Logger) *usecase.MarketScanner {
	return &usecase.MarketScanner{
		Repo:           repo,
		Exchanges:      buildExchangeSources(cfg, logger),
		Logger:         logger,
		TopN:           cfg.Scan.TopN,
		MinVolumeUSD:   cfg.Scan.MinVolumeUSD,
		PerTokenCapUSD: cfg.Scan.PerTokenCapUSD,
	}
}
