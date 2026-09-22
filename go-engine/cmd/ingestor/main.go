// Command ingestor connects to an exchange's public WebSockets and streams ticker + candle data
// into Kafka (the internal event bus, CLAUDE.md §12) for downstream consumption by the Paper
// Trading Engine, the trading engine, and research/feature building.
//
// EXCHANGE-PARAMETERIZED (2026-09-22, mirroring cmd/okx-gateway's own GATEWAY_EXCHANGE/buildService
// pattern exactly, CLAUDE.md §46.4): cfg.Ingestion.Exchange ("okx" or "mexc", INGEST_EXCHANGE env)
// selects which branch runs. Per the same "second deployed instance of the same binary" design
// already used for cmd/okx-gateway and cmd/paper-trader — running OKX and MEXC ingestion side by
// side is a second invocation of this process with INGEST_EXCHANGE=mexc and its own CONFIG_PATH,
// never one process juggling two exchanges' WS clients at once. The OKX branch (runOKXIngestor) is
// the pre-existing implementation, unchanged in behavior; the MEXC branch (runMEXCIngestor) is new.
//
// An unknown exchange value is refused at startup rather than silently defaulting to OKX — the
// same "a typo here is a credentials/data-pointed-at-the-wrong-place mistake, not a cosmetic one"
// reasoning as buildService's own refusal.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/okx"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/ws"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

// bookLevel/orderbookEvent mirror cmd/api's own private types of the same name byte-for-byte (the
// wire contract both sides of okx.orderbook must agree on) — kept as a separate copy rather than a
// shared package because neither side imports the other today and a shared type for exactly one
// Kafka topic's payload would be more indirection than the two ~10-line structs it replaces.
type bookLevel [4]string

type orderbookEvent struct {
	InstID string      `json:"instId"`
	Asks   []bookLevel `json:"asks"`
	Bids   []bookLevel `json:"bids"`
	Ts     string      `json:"ts"`
}

// toWireLevels converts the merger's BookLevel (raw price/size strings) into the [4]string wire
// shape cmd/api's reshapeBookLevels already knows how to read — only indices 0/1 are ever
// populated (price, size); the "deprecated"/numOrders fields (books5's own [2]/[3]) have no
// equivalent from a merged book and are left empty, which reshapeBookLevels never reads anyway.
func toWireLevels(levels []okx.BookLevel) []bookLevel {
	out := make([]bookLevel, len(levels))
	for i, l := range levels {
		out[i] = bookLevel{l.Px, l.Sz, "", ""}
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

	metrics.Serve(envOr("METRICS_ADDR", ":9101"), logger)

	switch cfg.Ingestion.Exchange {
	case "", "okx":
		runOKXIngestor(ctx, stop, cfg, repo, logger)
	case "mexc":
		runMEXCIngestor(ctx, stop, cfg, repo, logger)
	default:
		logger.Error("unknown ingestion.exchange (want \"okx\" or \"mexc\")", "exchange", cfg.Ingestion.Exchange)
		os.Exit(1)
	}
}

// runOKXIngestor is the pre-existing OKX ingestor, unchanged in behavior from before this file was
// made exchange-parameterized (2026-09-22) — every comment/design note below predates that split
// and still describes this branch exactly.
func runOKXIngestor(ctx context.Context, stop context.CancelFunc, cfg *config.Config, repo *postgres.Repository, logger *slog.Logger) {
	// The instrument roster now comes from the DATABASE, not config.yaml (2026-09-13, migration
	// 000031). That is what lets the token-discovery scan put a newly-found token to work: while the
	// roster was trading.inst_ids plus a hand-maintained trading.symbol_map, a scanned token had
	// neither an entry nor an exec instId and could never be subscribed to.
	//
	// config.yaml's own list is still the seed for a database that has never held a roster — the
	// state of every deployment the moment this lands — so this deploy changes nothing about what is
	// collected until a scan or an operator says otherwise. See usecase.RosterFor.
	seedExecIDs, err := usecase.SeedExecIDs(cfg.Trading.SymbolMap, cfg.Trading.InstIDs)
	if err != nil {
		logger.Error("failed to resolve trading.inst_ids against trading.symbol_map", "error", err)
		os.Exit(1)
	}
	roster, err := usecase.RosterFor(ctx, repo, "okx", "ingest",
		cfg.Trading.InstIDs, seedExecIDs, cfg.Trading.ExecInstType, logger)
	if err != nil {
		logger.Error("failed to load instrument roster", "error", err)
		os.Exit(1)
	}
	if len(roster.Symbols) == 0 {
		// Starting with nothing to subscribe to would leave a process that looks healthy and
		// collects no data at all — the silent-data-gap failure §9 exists to prevent.
		logger.Error("instrument roster is empty for the ingest consumer — nothing to subscribe to")
		os.Exit(1)
	}

	wsInstIDs := make([]string, 0, len(roster.Symbols))
	symbolFor := make(map[string]string, len(roster.Symbols))
	for _, sym := range roster.Symbols {
		wireID := roster.ExecInstID[sym]
		wsInstIDs = append(wsInstIDs, wireID)
		symbolFor[wireID] = sym
	}
	resolveSymbol := func(wireInstID string) (string, error) {
		sym, ok := symbolFor[wireInstID]
		if !ok {
			return "", fmt.Errorf("received data for OKX instId %q with no roster entry", wireInstID)
		}
		return sym, nil
	}

	// Watch for the roster changing underneath us and restart when it does. A WS subscription is
	// fixed for the life of its socket, so adding an instrument means re-subscribing; exiting and
	// letting the restart policy relaunch re-derives every piece of state from the database, where a
	// partial in-place reload is how a service ends up half-subscribed with nothing reporting it
	// (the same mechanism cmd/strategy-tester §18 and cmd/paper-trader §22 use for config changes).
	go (&usecase.RosterWatcher{
		Repo: repo, Exchange: "okx", Consumer: "ingest",
		Interval: time.Minute, Logger: logger, Baseline: roster.Symbols,
		OnChange: func(reason string) {
			logger.Info("restarting to pick up the new instrument roster", "reason", reason)
			stop()
		},
	}).Run(ctx)

	tickerPub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.tickers")
	defer tickerPub.Close()

	tickerClient := &ws.PublicClient{
		URL:     cfg.OKX.PublicWSURL,
		Channel: "tickers",
		InstIDs: wsInstIDs,
		Logger:  logger,
		Handler: func(msg ws.Message) {
			sym, err := resolveSymbol(msg.Arg.InstID)
			if err != nil {
				logger.Warn("failed to resolve inbound ticker instId to a symbol", "error", err)
				return
			}
			var raw []json.RawMessage
			if err := json.Unmarshal(msg.Data, &raw); err != nil {
				logger.Warn("failed to decode ticker payload", "error", err)
				return
			}
			for _, r := range raw {
				// Rewrite the wire instId field to the short symbol before publishing, so every
				// downstream consumer (paper-trader, BotTrader, the panel) only ever sees the
				// internal identity, never OKX's own wire format.
				rewritten, err := rewriteInstID(r, sym)
				if err != nil {
					logger.Warn("failed to rewrite ticker instId", "error", err)
					continue
				}
				if err := tickerPub.Publish(ctx, sym, rewritten); err != nil {
					logger.Warn("failed to publish tick to kafka", "error", err)
					continue
				}
				metrics.IngestorEventsTotal.WithLabelValues("tick", sym).Inc()
			}
		},
	}

	// Candlesticks live on the business WS endpoint in OKX v5, separate from public tickers. One
	// WS connection + one Kafka topic per configured timeframe, so consumers only ever see the
	// bar they subscribed to and don't need to filter out other timeframes themselves. Each
	// topic's messages are keyed by instId (internal/kafkastream.Publisher), so one instrument's
	// candles stay strictly ordered within its own partition.
	candlePubs := make(map[string]*kafkastream.Publisher, len(cfg.Ingestion.Bars))
	candleClients := make([]*ws.PublicClient, 0, len(cfg.Ingestion.Bars))
	for _, bar := range cfg.Ingestion.Bars {
		bar := bar
		pub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.candles."+bar)
		candlePubs[bar] = pub
		defer pub.Close()

		candleClients = append(candleClients, &ws.PublicClient{
			URL:     cfg.OKX.BusinessWSURL,
			Channel: "candle" + bar,
			InstIDs: wsInstIDs,
			Logger:  logger,
			Handler: func(msg ws.Message) {
				sym, err := resolveSymbol(msg.Arg.InstID)
				if err != nil {
					logger.Warn("failed to resolve inbound candle instId to a symbol", "bar", bar, "error", err)
					return
				}
				var bars [][]string
				if err := json.Unmarshal(msg.Data, &bars); err != nil {
					logger.Warn("failed to decode candle payload", "bar", bar, "error", err)
					return
				}
				for _, row := range bars {
					event := candleEvent{InstID: sym, Bar: bar, Candle: row}
					if err := pub.Publish(ctx, sym, event); err != nil {
						logger.Warn("failed to publish candle to kafka", "bar", bar, "error", err)
						continue
					}
					metrics.IngestorEventsTotal.WithLabelValues("candle", sym).Inc()
				}
			},
		})
	}

	// Order book (docs/MANUAL_TRADE_PLAN.md §7, widened 2026-09-19): the full `books` channel (up
	// to 400 levels/side, snapshot + incremental delta updates + a checksum field) replaces
	// `books5`, which is hard-capped at exactly 5 levels/side with no way to widen it — confirmed
	// live before this change (the operator needed at least 7-8 rows, which books5 structurally
	// cannot provide). `books` is public/unauthenticated on the same host every other socket here
	// already uses. Subscription stays permanent/full-roster, unchanged from the original books5
	// design (§7's original "always on for the whole roster" call, confirmed still correct — OKX
	// lets many instruments share one connection either way).
	//
	// One okx.BookMerger per instrument (NOT safe for concurrent use, internal/okx/orderbook.go's
	// own doc comment) — safe here because every message for one instId always arrives on this
	// same read-then-dispatch goroutine sequence, the same single-goroutine-per-instrument
	// assumption every other per-instrument state in this codebase already relies on. A checksum
	// mismatch drops the book and logs; the WS client's own reconnect-with-backoff (already
	// implemented in ws.PublicClient.Run) resends a fresh snapshot on the next connection, so no
	// separate resubscribe logic is needed here — reconnecting the whole client is a safe superset.
	//
	// Publishing is rate-limited to orderbookPublishInterval per instrument: `books` pushes a delta
	// on every price-level change (far more often than books5's periodic full snapshot), but the
	// panel only needs a human-perceptible refresh rate, not every tick-level book event.
	orderbookPub := kafkastream.NewPublisher(cfg.Kafka.Brokers, "okx.orderbook")
	defer orderbookPub.Close()

	// okx.orderbook is far higher-volume than every other topic (a delta roughly every 150ms per
	// instrument, vs. tickers/candles' much sparser rate), so the broker-wide retention/segment
	// defaults tuned for those (docker-compose.yml, 6h/128MB) let this one topic alone accumulate
	// several GB — measured at 3.9GB on 2026-09-20, the single largest disk consumer found while
	// investigating why the Resources page's cleanup button reclaimed almost nothing (it only ever
	// pruned Docker's build cache, which was never where the real usage was). Order-book depth is
	// meaningless to retain past a very short window — nothing ever reads a stale snapshot — so
	// this overrides just this one topic to 30 minutes with a smaller segment size (segment.bytes
	// has to be small enough to actually close within the retention window, or retention can never
	// delete anything). Self-healing on every restart: internal/kafkastream.EnsureTopicRetention's
	// own doc comment explains why this can't simply be a docker-compose.yml env var.
	const orderbookRetentionMs = 30 * 60 * 1000    // 30 minutes
	const orderbookSegmentBytes = 16 * 1024 * 1024 // 16MB
	kafkastream.EnsureTopicRetentionInBackground(ctx, logger, cfg.Kafka.Brokers, "okx.orderbook", orderbookRetentionMs, orderbookSegmentBytes)

	// orderbookDepth was 8 when this section first shipped (2026-09-19) — just enough to show a
	// handful of raw rows. That is not enough RAW data for the panel's own price-relative grouping
	// to aggregate meaningfully: with only 8 levels spanning a narrow price band, a wider bucket
	// (10x/100x the tick size) collapses nearly all of them into one or two buckets, which reads as
	// "grouping does nothing" — reported directly by the operator the same day. Raised to 50 so a
	// wide grouping selection actually has enough raw levels underneath it to combine into several
	// real buckets; the panel itself only ever renders up to 16 rows total (OrderbookLadder's own
	// per-side cap), so this is headroom for aggregation, not a change to what's displayed.
	const orderbookDepth = 50
	const orderbookPublishInterval = 150 * time.Millisecond

	mergers := make(map[string]*okx.BookMerger, len(wsInstIDs))
	lastPublished := make(map[string]time.Time, len(wsInstIDs))

	orderbookClient := &ws.PublicClient{
		URL:     cfg.OKX.PublicWSURL,
		Channel: "books",
		InstIDs: wsInstIDs,
		Logger:  logger,
		Handler: func(msg ws.Message) {
			sym, err := resolveSymbol(msg.Arg.InstID)
			if err != nil {
				logger.Warn("failed to resolve inbound orderbook instId to a symbol", "error", err)
				return
			}
			var pushes []okx.BooksPush
			if err := json.Unmarshal(msg.Data, &pushes); err != nil {
				logger.Warn("failed to decode orderbook payload", "instId", sym, "error", err)
				return
			}
			if len(pushes) == 0 {
				return
			}

			merger, ok := mergers[sym]
			if !ok {
				merger = okx.NewBookMerger()
				mergers[sym] = merger
			}
			if err := merger.Apply(msg.Action, pushes[0]); err != nil {
				logger.Warn("orderbook checksum/merge failed, dropping book until resubscribe", "instId", sym, "action", msg.Action, "error", err)
				delete(mergers, sym) // force a fresh snapshot to be required before this instrument's book is used again
				return
			}

			if since := time.Since(lastPublished[sym]); since < orderbookPublishInterval {
				return
			}
			lastPublished[sym] = time.Now()

			asks, bids := merger.TopN(orderbookDepth)
			event := orderbookEvent{
				InstID: sym,
				Asks:   toWireLevels(asks),
				Bids:   toWireLevels(bids),
				Ts:     pushes[0].Ts,
			}
			// Publish takes the STRUCT, not pre-marshaled bytes — Publisher.Publish already calls
			// json.Marshal internally (matching every other publisher call in this file). Passing
			// an already-marshaled []byte here was a real bug caught live: plain []byte has no
			// custom MarshalJSON, so Go's default encoding base64-encodes it into a JSON STRING
			// (unlike json.RawMessage, which the ticker rewrite path above correctly uses and which
			// marshals to itself verbatim) — cmd/api's consumer then failed
			// `json: cannot unmarshal string into Go value of type main.orderbookEvent` on every
			// single message, silently (the handler swallows a decode error and returns nil), so
			// the orderbook Kafka topic filled up and the consumer group's offset advanced
			// normally while broadcasting nothing to the panel at all.
			if err := orderbookPub.Publish(ctx, sym, event); err != nil {
				logger.Warn("failed to publish orderbook snapshot to kafka", "error", err)
				return
			}
			metrics.IngestorEventsTotal.WithLabelValues("orderbook", sym).Inc()
		},
	}

	logger.Info("starting okx ingestor", "instIds", roster.Symbols, "bars", cfg.Ingestion.Bars)

	errCh := make(chan error, 2+len(candleClients))
	go func() { errCh <- tickerClient.Run(ctx) }()
	go func() { errCh <- orderbookClient.Run(ctx) }()
	for _, c := range candleClients {
		c := c
		go func() { errCh <- c.Run(ctx) }()
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down ingestor")
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			logger.Error("ingestor stopped with error", "error", err)
			os.Exit(1)
		}
	}
}

// candleEvent wraps a raw candle array with the instrument id and bar it belongs to, since the
// candle array itself (unlike ticker payloads) doesn't carry instId, and each bar publishes to its
// own Kafka topic but shares this same event shape. Shared verbatim by both the OKX and MEXC
// branches — usecase.decodeCandle (internal/usecase/tickfeed.go) is the single decoder both must
// agree with; see runMEXCIngestor.go's own comment on the array layout it constructs to match it.
type candleEvent struct {
	InstID string   `json:"instId"`
	Bar    string   `json:"bar"`
	Candle []string `json:"candle"`
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// rewriteInstID re-marshals a raw OKX ticker payload with its "instId" field replaced by the
// short internal symbol, so a downstream consumer reading this Kafka message never sees OKX's own
// wire-format instId at all (CLAUDE.md §27, 2026-09-04 design). The candle path doesn't need this
// helper — candleEvent is a struct this code controls directly, not a raw JSON passthrough.
func rewriteInstID(raw json.RawMessage, symbol string) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode ticker payload for instId rewrite: %w", err)
	}
	m["instId"] = symbol
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("re-encode ticker payload after instId rewrite: %w", err)
	}
	return out, nil
}
