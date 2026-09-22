package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	mexcrest "github.com/eghbalii/okxBot/go-engine/internal/mexc/rest"
	mexcws "github.com/eghbalii/okxBot/go-engine/internal/mexc/ws"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

// runMEXCIngestor is the MEXC counterpart of runOKXIngestor (2026-09-22) — same roster/restart-on-
// change/Kafka-publish shape, MEXC's own WS protocol underneath (internal/mexc/ws, already built
// and live-verified, CLAUDE.md §46). It publishes to "mexc.tickers"/"mexc.candles.<bar>" — a
// distinct topic PREFIX, not a payload field — so usecase.decodeTick/decodeCandle
// (internal/usecase/tickfeed.go) need no changes at all: both only ever read the topic they were
// told to consume, never which exchange produced it. The wire SHAPE published here is therefore
// deliberately identical to OKX's own tickEvent/candleEvent (instId/last, instId/bar/candle),
// constructed field-by-field from MEXC's decoded domain types rather than passed through raw.
//
// No orderbook or funding-rate ingestion for MEXC in this pass — paper trading does not need
// either, and building them was explicitly out of scope for the first MEXC ingestion pipeline.
func runMEXCIngestor(ctx context.Context, stop context.CancelFunc, cfg *config.Config, repo *postgres.Repository, logger *slog.Logger) {
	// MEXC symbols ARE their own exec id (internal/mexc/adapter.go's IdentitySymbolResolver, §46.1)
	// — config.mexc.yaml's own trading.symbol_map already reflects this (each entry maps to
	// itself), so SeedExecIDs works unchanged here.
	seedExecIDs, err := usecase.SeedExecIDs(cfg.Trading.SymbolMap, cfg.Trading.InstIDs)
	if err != nil {
		logger.Error("failed to resolve trading.inst_ids against trading.symbol_map", "error", err)
		os.Exit(1)
	}
	roster, err := usecase.RosterFor(ctx, repo, "mexc", "ingest",
		cfg.Trading.InstIDs, seedExecIDs, cfg.Trading.ExecInstType, logger)
	if err != nil {
		logger.Error("failed to load instrument roster", "error", err)
		os.Exit(1)
	}
	if len(roster.Symbols) == 0 {
		logger.Error("instrument roster is empty for the ingest consumer — nothing to subscribe to")
		os.Exit(1)
	}

	wsSymbols := make([]string, 0, len(roster.Symbols))
	symbolFor := make(map[string]string, len(roster.Symbols))
	for _, sym := range roster.Symbols {
		wireSym := roster.ExecInstID[sym]
		wsSymbols = append(wsSymbols, wireSym)
		symbolFor[wireSym] = sym
	}
	resolveSymbol := func(wireSymbol string) (string, error) {
		sym, ok := symbolFor[wireSymbol]
		if !ok {
			return "", fmt.Errorf("received data for MEXC symbol %q with no roster entry", wireSymbol)
		}
		return sym, nil
	}

	// Same restart-on-roster-change mechanism as the OKX branch (usecase.RosterWatcher's own doc
	// comment covers the reasoning identically for either exchange).
	go (&usecase.RosterWatcher{
		Repo: repo, Exchange: "mexc", Consumer: "ingest",
		Interval: time.Minute, Logger: logger, Baseline: roster.Symbols,
		OnChange: func(reason string) {
			logger.Info("restarting to pick up the new instrument roster", "reason", reason)
			stop()
		},
	}).Run(ctx)

	tickerPub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "mexc.tickers")
	defer tickerPub.Close()

	tickerClient := &mexcws.PublicClient{
		URL:     cfg.MEXC.PublicWSURL,
		Method:  "sub.ticker",
		Symbols: wsSymbols,
		Logger:  logger,
		Handler: func(msg mexcws.Message) {
			sym, err := resolveSymbol(msg.Symbol)
			if err != nil {
				logger.Warn("failed to resolve inbound ticker symbol", "error", err)
				return
			}
			t, err := mexcws.DecodeTicker(msg)
			if err != nil {
				logger.Warn("failed to decode mexc ticker payload", "error", err)
				return
			}
			// tickEvent{instId, last} — usecase.decodeTick's exact expected shape
			// (internal/usecase/tickfeed.go). Built field-by-field from the decoded domain.Ticker
			// rather than a raw-JSON rewrite (unlike OKX's rewriteInstID) since MEXC's own push
			// payload has a completely different shape and there is nothing to "rewrite" — this
			// constructs the wire contract PaperTrader expects directly.
			payload := struct {
				InstID string `json:"instId"`
				Last   string `json:"last"`
			}{InstID: sym, Last: t.Last.String()}
			if err := tickerPub.Publish(ctx, sym, payload); err != nil {
				logger.Warn("failed to publish tick to kafka", "error", err)
				return
			}
			metrics.IngestorEventsTotal.WithLabelValues("tick", sym).Inc()
		},
	}

	// One WS client + one Kafka topic + one KlineFinalizer per configured timeframe, mirroring the
	// OKX branch's own per-bar structure — a bar with no MEXC interval equivalent
	// (rest.IntervalFor's ok=false) is skipped with a loud warning rather than silently
	// subscribing to nothing, the same "loud failure over a silent data gap" rule as §9's
	// bar-casing validation.
	//
	// A SEPARATE finalizer per bar, not one shared across all of them (found live 2026-09-22: a
	// single finalizer here crash-looped the whole process with "fatal error: concurrent map
	// writes" within a minute of real traffic). mexcws.KlineFinalizer's own doc comment states its
	// safety invariant plainly — "not safe for concurrent use... one finalizer belongs to one
	// stream, and the WS client's dispatch goroutine is single-threaded" — and this loop spawns a
	// SEPARATE PublicClient (hence a separate dispatch goroutine, internal/mexc/ws/public.go's own
	// connectAndStream) per bar below, so a finalizer shared across the loop violates that
	// invariant the instant two bars' goroutines call Observe concurrently, which real MEXC
	// traffic across 5 simultaneous kline subscriptions does almost immediately. Each finalizer is
	// still internally keyed by (symbol, interval) — see its own doc comment on why that matters
	// within one stream — but the stream boundary here is per-bar, not per-process.
	candlePubs := make(map[string]*kafkastream.Publisher, len(cfg.Ingestion.Bars))
	candleClients := make([]*mexcws.PublicClient, 0, len(cfg.Ingestion.Bars))
	for _, bar := range cfg.Ingestion.Bars {
		bar := bar
		interval, ok := wsIntervalFor(bar)
		if !ok {
			logger.Warn("mexc has no interval equivalent for this bar, skipping", "bar", bar)
			continue
		}
		finalizer := mexcws.NewKlineFinalizer()
		pub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "mexc.candles."+bar)
		candlePubs[bar] = pub
		defer pub.Close()

		candleClients = append(candleClients, &mexcws.PublicClient{
			URL:      cfg.MEXC.PublicWSURL,
			Method:   "sub.kline",
			Interval: interval,
			Symbols:  wsSymbols,
			Logger:   logger,
			Handler: func(msg mexcws.Message) {
				sym, err := resolveSymbol(msg.Symbol)
				if err != nil {
					logger.Warn("failed to resolve inbound candle symbol", "bar", bar, "error", err)
					return
				}
				c, err := mexcws.DecodeKline(msg)
				if err != nil {
					logger.Warn("failed to decode mexc kline payload", "bar", bar, "error", err)
					return
				}
				// MEXC has no "this bar is closed" flag (unlike OKX's confirm=1) — the finalizer
				// infers closure by observing a push for a newer bar's open time and returns the
				// PREVIOUS, now-complete bar exactly once (mexcws.KlineFinalizer's own doc
				// comment). Every push before that is the bar still forming and is correctly
				// dropped here — publishing it would re-evaluate strategies on an unfinished
				// candle (CLAUDE.md §14).
				finalized, ok := finalizer.Observe(sym, interval, c)
				if !ok {
					return
				}
				event := candleEvent{InstID: sym, Bar: bar, Candle: mexcCandleArray(finalized)}
				if err := pub.Publish(ctx, sym, event); err != nil {
					logger.Warn("failed to publish candle to kafka", "bar", bar, "error", err)
					return
				}
				metrics.IngestorEventsTotal.WithLabelValues("candle", sym).Inc()
			},
		})
	}

	logger.Info("starting mexc ingestor", "symbols", roster.Symbols, "bars", cfg.Ingestion.Bars)

	errCh := make(chan error, 1+len(candleClients))
	go func() { errCh <- tickerClient.Run(ctx) }()
	for _, c := range candleClients {
		c := c
		go func() { errCh <- c.Run(ctx) }()
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down mexc ingestor")
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			logger.Error("mexc ingestor stopped with error", "error", err)
			os.Exit(1)
		}
	}
}

// mexcCandleArray builds usecase.decodeCandle's exact expected raw array layout
// (internal/usecase/tickfeed.go: index 0=ts_ms, 1=open, 2=high, 3=low, 4=close, 5=volume, index 8
// (if present)="1" means confirmed/finalized) from a finalized domain.Candle. Indices 6-7 (OKX's
// own volCcy/volCcyQuote fields) have no MEXC equivalent and are left empty — decodeCandle never
// reads them, only checks len(...) >= 6 for the fields this function fills and len(...) >= 9 for
// the optional confirm flag at index 8.
//
// confirm is always "1" here because this is only ever called with a candle the finalizer has
// already confirmed closed (mexcws.KlineFinalizer.Observe's ok=true case) — MEXC's own wire
// protocol has no partial/forming-bar equivalent to publish in the first place, unlike OKX which
// pushes confirm=0 for the still-forming bar on the same event shape.
func mexcCandleArray(c domain.Candle) []string {
	return []string{
		strconv.FormatInt(c.Timestamp.UnixMilli(), 10),
		c.Open.String(), c.High.String(), c.Low.String(), c.Close.String(), c.Volume.String(),
		"", "", // indices 6-7: OKX's volCcy/volCcyQuote, no MEXC equivalent, unread by decodeCandle
		"1", // index 8: confirm flag — always finalized by the time this is built
	}
}

// wsIntervalFor translates an internal bar name into the interval string MEXC's WEBSOCKET kline
// subscription accepts — which is NOT always identical to rest.IntervalFor's REST-endpoint
// vocabulary, discovered live 2026-09-22: the REST candles endpoint accepts "Hour1" for 1H
// (rest.IntervalFor's own "verified live" comment, predating this file), but the WS sub.kline
// method rejects "Hour1" outright ({"channel":"rs.error","data":"Not support interval"}) and only
// accepts "Min60" for the same timeframe. Every other bar (1m/5m/15m/30m/4H/8H/1D/1W/1M) was
// confirmed identical between the two endpoints by direct live testing before this fix — this is a
// narrow, one-bar exchange quirk, not a systematic REST/WS naming difference, so it is handled as
// a single override here rather than forking rest.IntervalFor's whole table (which the REST candle
// path still depends on being correct for "Hour1").
func wsIntervalFor(bar string) (string, bool) {
	if bar == "1H" {
		return "Min60", true
	}
	return mexcrest.IntervalFor(bar)
}
