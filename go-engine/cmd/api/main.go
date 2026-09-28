// Command api runs the dashboard/reporting backend (CLAUDE.md §11): RL model status, strategy
// CRUD + assignments + per-strategy stats, and the positions panel across paper/demo/real trading
// modes. No auth in v1 — reachable only over the OpenVPN tunnel into the server's network, so
// Config.API.Addr should stay bound to a private interface, not 0.0.0.0.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/api"
	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/gatewayclient"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

// tickEvent mirrors usecase.tickEvent's decode of the raw OKX tickers payload (CLAUDE.md §12) —
// duplicated rather than exported/shared because cmd/api only needs the two fields it re-broadcasts
// below, not the full ticker shape.
type tickEvent struct {
	InstID string `json:"instId"`
	Last   string `json:"last"`
}

// priceUpdate is the panel's live-price WebSocket message (CLAUDE.md §11.4's positions panel):
// last-traded price per instrument, pushed on every tick so the panel can compute moment-to-moment
// unrealized PnL client-side from entry_px/size/leverage rather than polling REST for it. Same
// "type" discriminator convention as usecase.PaperOrderEvent so the panel can tell the two kinds of
// message on this one socket apart.
//
// Exchange was added 2026-09-23 alongside the MEXC price consumer below — this project's own short
// symbols (SOL, AVAX, XRP, DOGE, ...) are traded on BOTH exchanges under the same short instId, so
// broadcasting price ticks with no exchange tag would make the panel's live-price map ambiguous the
// moment both an OKX and a MEXC position exist for the same token at once, silently showing one
// exchange's price on the other's row. Omitted (empty string) means "okx", matching every other
// exchange-scoped field's convention across this codebase (candles.exchange, paper_orders.exchange).
type priceUpdate struct {
	Type     string `json:"type"` // "price"
	InstID   string `json:"instId"`
	Price    string `json:"price"`
	Exchange string `json:"exchange,omitempty"`
}

// runPriceBridge consumes one exchange's tickers topic and broadcasts a reshaped priceUpdate to
// every connected panel client, tagged with exchange (empty for OKX, matching every other
// exchange-scoped field's "empty means okx" convention). Shared by the OKX and MEXC wiring below so
// the reshape/tag logic can never drift between the two — before this existed as a shared function,
// only the OKX topic was ever consumed at all, and a MEXC position's live price/PnL on the panel
// simply never updated (found 2026-09-23, right after the same gap was fixed for the candle chart).
// Runs until ctx is done; Consumer.Run already stops on cancellation (internal/kafkastream), so the
// caller does not need to track or close the underlying consumer separately.
func runPriceBridge(ctx context.Context, brokers []string, topic, consumerGroup, exchange string, srv *api.Server, logger *slog.Logger) {
	consumer := kafkastream.NewConsumer(brokers, topic, consumerGroup)
	err := consumer.Run(ctx, func(_ context.Context, data []byte) error {
		var tick tickEvent
		if err := json.Unmarshal(data, &tick); err != nil {
			return nil // malformed tick: skip rather than fail the whole consumer loop
		}
		out, err := json.Marshal(priceUpdate{Type: "price", InstID: tick.InstID, Price: tick.Last, Exchange: exchange})
		if err != nil {
			return nil
		}
		srv.Broadcast(out)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		logger.Error("prices consumer exited", "topic", topic, "error", err)
	}
}

// candleEvent is the ingestor's own Kafka payload for one candle push (the OKX wire shape: a string
// array, [ts, o, h, l, c, vol, ...], with confirm as the last element).
type candleEvent struct {
	InstID string   `json:"instId"`
	Bar    string   `json:"bar"`
	Candle []string `json:"candle"`
}

// candleUpdate pushes one candle to the panel as OKX reports it, forming bars included.
//
// Why this exists: the chart used to poll REST for candles and synthesize the forming bar from the
// price ticks the browser happened to receive. Two consequences the operator reported — a bar's
// real high/low were whatever the panel had seen (so wicks only appeared later, when the finalized
// row arrived), and that correction lands a FULL BAR late because OKX marks a bar confirm=1 only
// when the next one closes (measured: the 12:20 bar confirmed at 12:25:01). OKX's own confirm=0
// pushes already carry the true OHLC of the forming bar, and the ingestor already publishes every
// one to Kafka — nothing was consuming them for the panel. Forwarding them replaces the guesswork
// with the exchange's own numbers.
//
// Confirmed is passed through so the panel can tell a still-forming bar from a closed one.
//
// Exchange added 2026-09-23, same reasoning/pattern as priceUpdate's own Exchange field: this
// bridge only ever consumed okx.candles.<bar>, so a MEXC chart's history loaded correctly (once the
// REST endpoint was fixed to accept ?exchange=) but the live/forming candle never updated and never
// rolled onto the next bar when one closed — runCandleBridge below is the same fix as
// runPriceBridge, applied to the topic this struct backs.
type candleUpdate struct {
	Type      string `json:"type"` // "candle"
	InstID    string `json:"instId"`
	Bar       string `json:"bar"`
	Timestamp string `json:"ts"` // epoch ms — both OKX and MEXC publish this in ms
	Open      string `json:"open"`
	High      string `json:"high"`
	Low       string `json:"low"`
	Close     string `json:"close"`
	Volume    string `json:"volume"`
	Confirmed bool   `json:"confirmed"`
	Exchange  string `json:"exchange,omitempty"`
}

// runCandleBridge is candleUpdate's counterpart to runPriceBridge above — same shared-function
// reasoning: one consumer per (exchange, bar) topic, reshaped and broadcast identically regardless
// of which exchange it came from, so the decode/tag logic cannot drift between OKX's and MEXC's
// wiring.
func runCandleBridge(ctx context.Context, brokers []string, topic, consumerGroup, exchange string, srv *api.Server, logger *slog.Logger, bar string) {
	consumer := kafkastream.NewConsumer(brokers, topic, consumerGroup)
	err := consumer.Run(ctx, func(_ context.Context, data []byte) error {
		var ev candleEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil // malformed: skip rather than fail the consumer loop
		}
		// [ts, o, h, l, c, vol, volCcy, volCcyQuote, confirm] — cmd/ingestor/mexc.go's
		// mexcCandleArray mirrors OKX's own wire shape exactly, so this decode needs no
		// exchange-specific branch. Guard on length rather than assuming: a short array would panic
		// on index, taking the goroutine (and with it this bar's whole stream) down silently.
		if len(ev.Candle) < 6 {
			return nil
		}
		out, err := json.Marshal(candleUpdate{
			Type: "candle", InstID: ev.InstID, Bar: ev.Bar,
			Timestamp: ev.Candle[0], Open: ev.Candle[1], High: ev.Candle[2],
			Low: ev.Candle[3], Close: ev.Candle[4], Volume: ev.Candle[5],
			Confirmed: ev.Candle[len(ev.Candle)-1] == "1",
			Exchange:  exchange,
		})
		if err != nil {
			return nil
		}
		srv.Broadcast(out)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		logger.Error("candles consumer exited", "bar", bar, "topic", topic, "error", err)
	}
}

// bookLevel mirrors one row of OKX's books5 payload: [price, size, deprecated, numOrders]. Decoded
// as a fixed-shape struct (not passed through as a raw string array) so the panel receives named
// fields rather than needing to know OKX's own positional convention.
type bookLevel [4]string

// orderbookEvent is the ingestor's own Kafka payload for one books5 push (docs/MANUAL_TRADE_PLAN.md
// §7) — the same full-snapshot shape OKX sends, just with instId already rewritten to the short
// internal symbol (cmd/ingestor's own rewriteInstID, matching every other event type on this bus).
type orderbookEvent struct {
	InstID string      `json:"instId"`
	Asks   []bookLevel `json:"asks"`
	Bids   []bookLevel `json:"bids"`
	Ts     string      `json:"ts"`
}

// orderbookUpdate is the panel's live order-book WebSocket message. Reshaped from OKX's raw
// [price, size, _, numOrders] rows into {px, sz} pairs — the panel has no use for the deprecated
// third field or the order count, and shipping named fields keeps the wire shape independent of
// OKX's own positional convention (same reasoning as priceUpdate/candleUpdate above).
type orderbookUpdate struct {
	Type   string        `json:"type"` // "orderbook"
	InstID string        `json:"instId"`
	Asks   []bookLevelKV `json:"asks"`
	Bids   []bookLevelKV `json:"bids"`
	Ts     string        `json:"ts"`
}

type bookLevelKV struct {
	Px string `json:"px"`
	Sz string `json:"sz"`
}

func reshapeBookLevels(rows []bookLevel) []bookLevelKV {
	out := make([]bookLevelKV, 0, len(rows))
	for _, r := range rows {
		out = append(out, bookLevelKV{Px: r[0], Sz: r[1]})
	}
	return out
}

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

	scanner := newMarketScanner(cfg, repo, logger)

	srv := &api.Server{
		Repo:                   repo,
		RLBaseURL:              cfg.RLService.URL,
		GrafanaURL:             cfg.API.GrafanaURL,
		OptimizerBaseURL:       cfg.StrategyOptimizer.ServiceURL,
		PaperTraderBaseURL:     cfg.PaperTrading.URL,
		PaperTraderProfileURLs: parsePaperTraderProfileURLs(os.Getenv("PAPER_TRADER_PROFILE_URLS")),
		TraderBaseURL:          cfg.Trading.URL,
		// Read-only: Report never changes the roster, so opening the Manage Tokens modal cannot
		// enable or disable anything. cmd/trader owns the acting half (AffordabilityService.Run).
		Affordability: &usecase.AffordabilityService{
			Repo:           repo,
			Exchange:       gatewayclient.New(cfg.Gateway.URL, "api"),
			Logger:         logger,
			Mode:           "bot",
			AllTokens:      cfg.Trading.InstIDs,
			Symbols:        okx.SymbolMap(cfg.Trading.SymbolMap),
			ExecInstType:   cfg.Trading.ExecInstType,
			MaxPositionPct: cfg.Account.MaxPositionPct,
			MaxLeverage:    cfg.Risk.MaxLeverage,
		},
		// The panel's manual SL/TP edit amends the position's resting order on the exchange before
		// touching the database (2026-09-09), so cmd/api needs its own gateway client for that one
		// call. Same gateway, same "api" consumer identity as the affordability reporter above —
		// the trader keeps its rate-limit priority over both.
		Protection: gatewayclient.New(cfg.Gateway.URL, "api"),
		// Positions backs GET /api/health's drift check — the evidence that decides whether clearing
		// a halt is safe (CLAUDE.md §48). Routed through the gateway like every other exchange call,
		// so it shares the same rate limit and credential boundary (§27.1).
		Positions: gatewayclient.New(cfg.Gateway.URL, "api"),
		// ManualTrade backs the manual/discretionary trading page (docs/MANUAL_TRADE_PLAN.md) —
		// same gateway/consumer identity as every other cmd/api exchange call above.
		ManualTrade: gatewayclient.New(cfg.Gateway.URL, "api"),
		// TdMode/PosMode for the manual leverage-setting endpoint (docs/MANUAL_TRADE_PLAN.md §3) —
		// the same values cmd/trader's ManualTrader uses for every order it actually places.
		ManualTrading: api.ManualTradingConfig{TdMode: cfg.Trading.TdMode, PosMode: cfg.Trading.PosMode},
		ExecInstIDFor: okx.SymbolMap(cfg.Trading.SymbolMap).Resolve,
		ExecInstType:  cfg.Trading.ExecInstType,
		ProcessMgr:    cfg.API.ProcessMgr,
		Units:         cfg.API.Units,
		Logger:        logger,
		// Must match what the trading services seed their account row with (CLAUDE.md §15.6) —
		// both read through GetAccountEquity, so a different value here would seed a balance the
		// engine never actually traded against.
		AccountInitialUSD: cfg.Account.InitialUSD,

		// The full configured instrument roster, used by GET /api/paper-trading/config's
		// AllInstIDs (the panel's token-manage modal). Was BackfillInstIDs (paired with a
		// now-removed BackfillBars/usecase.Backfill) before the candle backfill feature was
		// removed entirely (2026-09-01, explicit operator instruction: that OKX endpoint must
		// never be called).
		AllInstIDs: cfg.Trading.InstIDs,

		// Home page (2026-09-13): per-exchange balances, and the token-discovery scan. The scan is
		// hosted here rather than in its own service on the operator's own reasoning — it serves
		// nothing to anyone and runs a few times a day, so a container would add a deployment unit
		// and a memory footprint on a 3.9GB box (§35.7) and nothing else.
		ExchangeBalances: buildBalanceSources(cfg),
		Scanner:          scannerAdapter{inner: scanner},
		PerTokenCapUSD:   cfg.Scan.PerTokenCapUSD,

		// Disk cleanup's confirm-per-file candidates (2026-09-20): the repo root as bind-mounted
		// read-write at /host/repo (docker-compose.yml), separate from the existing read-only
		// /app/configs mount. Empty when HOST_ROOT_DIR is unset, which cleanly disables the
		// feature rather than scanning a path that doesn't exist — this is optional precisely so a
		// deployment that hasn't added the mount yet doesn't crash on startup.
		HostRootDir: os.Getenv("HOST_ROOT_DIR"),
	}
	routes := srv.Routes() // must be called before Hub() usage below so the same *wsHub backs both

	// The scheduled discovery scan. Runs one scan immediately so a freshly deployed cmd/api has a
	// market snapshot without waiting out the first interval — a Home page showing an empty market
	// for hours after a deploy reads as a broken feature rather than as a pending job.
	go scanner.RunEvery(ctx, cfg.Scan.Interval)

	// CLAUDE.md §11.4/§12: paper-order open/close events published by cmd/paper-trader onto Kafka
	// are relayed to every connected panel WebSocket client, replacing 5s position polling for the
	// alert (open/SL/TP) path. A dedicated consumer group ("api-ws-bridge") so this never competes
	// for offsets with paper-trader/strategy-optimizer's own groups on the same topic.
	orderEventsConsumer := kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.paper-order-events", "api-ws-bridge")
	go func() {
		err := orderEventsConsumer.Run(ctx, func(_ context.Context, data []byte) error {
			srv.Broadcast(data)
			return nil
		})
		if err != nil && ctx.Err() == nil {
			logger.Error("paper order events consumer exited", "error", err)
		}
	}()

	// Live last-traded price per instrument, for the positions panel's moment-to-moment PnL
	// (CLAUDE.md §11.4) — a distinct consumer group ("api-ws-bridge-tickers") from paper-trader's
	// own "paper-trader" group on the same okx.tickers topic, so this never competes for offsets or
	// skips messages paper-trader also needs. Reshaped to {type,instId,price} rather than forwarded
	// as OKX's raw wire payload, so the panel doesn't need to know OKX's ticker JSON shape.
	//
	// A SECOND price consumer for mexc.tickers was added 2026-09-23 — found live: the panel's chart
	// was fixed to read MEXC candles correctly, but the positions table's live price/PnL still
	// showed nothing for a MEXC position, because this bridge only ever consumed OKX's own topic.
	// runPriceBridge is shared between the two so the reshape/broadcast logic (and the exchange tag
	// on the outgoing message, priceUpdate's own doc comment) can never drift between them.
	go runPriceBridge(ctx, cfg.Kafka.Brokers, "okx.tickers", "api-ws-bridge-tickers", "", srv, logger)
	go runPriceBridge(ctx, cfg.Kafka.Brokers, "mexc.tickers", "api-ws-bridge-tickers-mexc", "mexc", srv, logger)

	// Live candles per configured timeframe, so the chart renders the forming bar from the
	// exchange's own OHLC instead of reconstructing it from ticks (see candleUpdate above). One
	// consumer per (exchange, bar) — each timeframe is its own topic per exchange (CLAUDE.md §12),
	// each with its own group so none of them competes for offsets with paper-trader's.
	//
	// The MEXC loop was added 2026-09-23 — same gap and same fix as runPriceBridge just above: this
	// bridge only ever consumed okx.candles.<bar>, so a MEXC chart's live/forming candle never
	// updated even after the REST history endpoint was fixed. cmd/api only loads config.yaml (OKX's
	// own), never config.mexc.yaml, so this reuses cfg.Ingestion.Bars for MEXC's topics too — both
	// ingestors are deployed with the same bar list (5m/15m/1H/4H/1D); a bar MEXC doesn't actually
	// publish is a silent no-op on that one topic, not an error.
	for _, bar := range cfg.Ingestion.Bars {
		go runCandleBridge(ctx, cfg.Kafka.Brokers, "okx.candles."+bar, "api-ws-bridge-candles-"+bar, "", srv, logger, bar)
		go runCandleBridge(ctx, cfg.Kafka.Brokers, "mexc.candles."+bar, "api-ws-bridge-candles-mexc-"+bar, "mexc", srv, logger, bar)
	}

	// Live order book (docs/MANUAL_TRADE_PLAN.md §7) — books5 snapshots for every configured
	// instrument, broadcast to every connected panel client the same way prices/candles already
	// are (measured: books5 pushes at roughly the same rate as tickers, so this doesn't change the
	// bridge's existing broadcast-everything-filter-client-side shape, CLAUDE.md §11.4). A
	// dedicated consumer group so this never competes for offsets with anything else reading
	// okx.orderbook in the future.
	orderbookConsumer := kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.orderbook", "api-ws-bridge-orderbook")
	go func() {
		err := orderbookConsumer.Run(ctx, func(_ context.Context, data []byte) error {
			var ev orderbookEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				return nil // malformed: skip rather than fail the consumer loop
			}
			out, err := json.Marshal(orderbookUpdate{
				Type:   "orderbook",
				InstID: ev.InstID,
				Asks:   reshapeBookLevels(ev.Asks),
				Bids:   reshapeBookLevels(ev.Bids),
				Ts:     ev.Ts,
			})
			if err != nil {
				return nil
			}
			srv.Broadcast(out)
			return nil
		})
		if err != nil && ctx.Err() == nil {
			logger.Error("orderbook consumer exited", "error", err)
		}
	}()

	httpServer := &http.Server{
		Addr:    cfg.API.Addr,
		Handler: routes,
	}

	go func() {
		<-ctx.Done()
		srv.CloseWS()
		_ = orderEventsConsumer.Close()
		// The price bridges (runPriceBridge, one per exchange) own and close their own Consumer
		// internally on ctx.Done() — same as the per-bar candle consumers just below, which were
		// never threaded through this explicit-close list either; Consumer.Run already stops on
		// context cancellation (internal/kafkastream), so an explicit Close here would be redundant.
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

// parsePaperTraderProfileURLs parses PAPER_TRADER_PROFILE_URLS, a comma-separated list of
// "<exchange-label>=<base-url>" pairs (2026-09-22, multi-exchange paper trading) — e.g.
// "mexc=http://paper-trader-mexc:8098". Deliberately a single generic env var rather than
// one new env var per profile: any future config-variant paper-trading experiment (a different
// exchange, or the same exchange with a different flag set) is just another entry here, with no
// code change needed to support it. A malformed entry (no "=", empty key) is skipped rather than
// failing startup — a typo in this optional, comparison-only mapping should not take the whole
// API service down.
func parsePaperTraderProfileURLs(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, url, ok := strings.Cut(pair, "=")
		key, url = strings.TrimSpace(key), strings.TrimSpace(url)
		if !ok || key == "" || url == "" {
			continue
		}
		out[key] = url
	}
	return out
}
