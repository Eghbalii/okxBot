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
type priceUpdate struct {
	Type   string `json:"type"` // "price"
	InstID string `json:"instId"`
	Price  string `json:"price"`
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
type candleUpdate struct {
	Type      string `json:"type"` // "candle"
	InstID    string `json:"instId"`
	Bar       string `json:"bar"`
	Timestamp string `json:"ts"` // epoch ms, as OKX sends it
	Open      string `json:"open"`
	High      string `json:"high"`
	Low       string `json:"low"`
	Close     string `json:"close"`
	Volume    string `json:"volume"`
	Confirmed bool   `json:"confirmed"`
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
		Repo:               repo,
		RLBaseURL:          cfg.RLService.URL,
		GrafanaURL:         cfg.API.GrafanaURL,
		TesterBaseURL:      cfg.Tester.URL,
		PaperTraderBaseURL: cfg.PaperTrading.URL,
		TraderBaseURL:      cfg.Trading.URL,
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
	pricesConsumer := kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.tickers", "api-ws-bridge-tickers")
	go func() {
		err := pricesConsumer.Run(ctx, func(_ context.Context, data []byte) error {
			var tick tickEvent
			if err := json.Unmarshal(data, &tick); err != nil {
				return nil // malformed tick: skip rather than fail the whole consumer loop
			}
			out, err := json.Marshal(priceUpdate{Type: "price", InstID: tick.InstID, Price: tick.Last})
			if err != nil {
				return nil
			}
			srv.Broadcast(out)
			return nil
		})
		if err != nil && ctx.Err() == nil {
			logger.Error("prices consumer exited", "error", err)
		}
	}()

	// Live candles per configured timeframe, so the chart renders the forming bar from OKX's own
	// OHLC instead of reconstructing it from ticks (see candleUpdate above). One consumer per bar
	// because each timeframe is its own topic (CLAUDE.md §12), each with its own group so none of
	// them competes for offsets with paper-trader's.
	for _, bar := range cfg.Ingestion.Bars {
		bar := bar
		c := kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.candles."+bar, "api-ws-bridge-candles-"+bar)
		go func() {
			err := c.Run(ctx, func(_ context.Context, data []byte) error {
				var ev candleEvent
				if err := json.Unmarshal(data, &ev); err != nil {
					return nil // malformed: skip rather than fail the consumer loop
				}
				// OKX sends [ts, o, h, l, c, vol, volCcy, volCcyQuote, confirm]. Guard on length
				// rather than assuming: a short array would panic on index, taking the goroutine
				// (and with it this bar's whole stream) down silently.
				if len(ev.Candle) < 6 {
					return nil
				}
				out, err := json.Marshal(candleUpdate{
					Type:      "candle",
					InstID:    ev.InstID,
					Bar:       ev.Bar,
					Timestamp: ev.Candle[0],
					Open:      ev.Candle[1],
					High:      ev.Candle[2],
					Low:       ev.Candle[3],
					Close:     ev.Candle[4],
					Volume:    ev.Candle[5],
					Confirmed: ev.Candle[len(ev.Candle)-1] == "1",
				})
				if err != nil {
					return nil
				}
				srv.Broadcast(out)
				return nil
			})
			if err != nil && ctx.Err() == nil {
				logger.Error("candles consumer exited", "bar", bar, "error", err)
			}
		}()
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
		_ = pricesConsumer.Close()
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
