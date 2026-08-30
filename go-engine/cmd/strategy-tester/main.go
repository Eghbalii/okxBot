// Command strategy-tester runs an independent strategy-validation service (2026-08-30 request):
// a standalone paper-trading copy of the production loop that opens real virtual positions
// against live prices, entirely apart from the RL agent and cmd/paper-trader. Every registered
// strategy.Factories kind trades on every configured instrument at a single fixed timeframe, one
// position at a time per instrument, with fixed size/leverage (no RL sizing/SL-TP-adjust/update
// mechanic — this measures raw signal quality, nothing else). Storage is entirely separate
// (internal/tester, migration 000010) so this can never collide with or be mistaken for
// production paper-trading data.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/kafkastream"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/okx/rest"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/tester"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
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

	metrics.Serve(envOr("METRICS_ADDR", ":9104"), logger)

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
	store := tester.NewStore(repo.Pool())

	// Panel-editable overrides (bar/notional/leverage) take effect at startup — a save from the
	// panel writes this row then exits the process (see handleRestart), Docker's restart policy
	// brings it back reading the row it just wrote.
	if rc, err := store.GetRuntimeConfig(ctx); err != nil {
		logger.Error("failed to load tester runtime config", "error", err)
		os.Exit(1)
	} else {
		if rc.Bar != nil {
			cfg.Tester.Bar = *rc.Bar
		}
		if rc.NotionalUSD != nil {
			cfg.Tester.NotionalUSD = *rc.NotionalUSD
		}
		if rc.Leverage != nil {
			cfg.Tester.Leverage = *rc.Leverage
		}
	}

	restClient := rest.New(cfg.OKX.RESTBaseURL, cfg.OKX.APIKey, cfg.OKX.APISecret, cfg.OKX.APIPassphrase, cfg.OKX.Simulated)

	// One version-1 row per registered kind, created if missing. Every kind starts enabled — the
	// operator later disables ones they don't want live from the panel's config section.
	kinds := make([]string, 0, len(strategy.Factories))
	for kind := range strategy.Factories {
		kinds = append(kinds, kind)
		if _, err := store.EnsureOriginVersion(ctx, kind); err != nil {
			logger.Error("failed to seed origin version", "kind", kind, "error", err)
			os.Exit(1)
		}
	}

	svc := &service{
		cfg:     cfg,
		store:   store,
		logger:  logger,
		windows: make(map[string]*candleWindow),
	}
	if err := svc.reloadStrategies(ctx); err != nil {
		logger.Error("failed to load enabled versions", "error", err)
		os.Exit(1)
	}

	instIDs := cfg.Tester.InstIDs
	for _, instID := range instIDs {
		if err := svc.seedWindow(restClient, instID); err != nil {
			logger.Error("failed to seed candle window", "instId", instID, "error", err)
		}
	}

	// Same shared-reader-per-topic + Dispatcher fan-out pattern as cmd/paper-trader/
	// cmd/strategy-optimizer (CLAUDE.md §12): one Kafka consumer group ("strategy-tester") reading
	// the tick topic and the single configured bar's candle topic, routed to per-instrument
	// handlers in-process.
	tickDispatcher := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.tickers", "strategy-tester"))
	candleDispatcher := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.candles."+cfg.Tester.Bar, "strategy-tester"))
	for _, instID := range instIDs {
		instID := instID
		tickDispatcher.Register(instID, func(ctx context.Context, data []byte) error {
			return svc.handleTick(ctx, instID, data)
		})
		candleDispatcher.Register(instID, func(ctx context.Context, data []byte) error {
			return svc.handleCandle(ctx, instID, data)
		})
	}

	go func() {
		if err := tickDispatcher.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("tick dispatcher exited", "error", err)
		}
	}()
	go func() {
		if err := candleDispatcher.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("candle dispatcher exited", "error", err)
		}
	}()

	httpServer := &http.Server{Addr: cfg.Tester.Addr, Handler: svc.routes()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("starting strategy-tester", "addr", cfg.Tester.Addr, "bar", cfg.Tester.Bar,
		"instIds", instIDs, "kinds", kinds, "notionalUsd", cfg.Tester.NotionalUSD, "leverage", cfg.Tester.Leverage)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("strategy-tester server exited", "error", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// candleWindow is a mutex-guarded rolling candle buffer for one instrument (this service trades a
// single fixed bar, so unlike PaperTrader/optimizer there is only ever one window per instrument,
// not one per bar).
type candleWindow struct {
	mu      sync.Mutex
	candles []domain.Candle
}

func (w *candleWindow) snapshot() []domain.Candle {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]domain.Candle(nil), w.candles...)
}

func (w *candleWindow) append(c domain.Candle, limit int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := len(w.candles); n > 0 && w.candles[n-1].Timestamp.Equal(c.Timestamp) {
		w.candles[n-1] = c
	} else {
		w.candles = append(w.candles, c)
	}
	if len(w.candles) > limit {
		w.candles = w.candles[len(w.candles)-limit:]
	}
}

// runningVersion pairs a live strategy.Strategy instance with the durable version row it came
// from, keyed by kind.
type runningVersion struct {
	versionID int64
	kind      string
	live      strategy.Strategy
}

// service holds cmd/strategy-tester's dependencies and runtime state.
type service struct {
	cfg    *config.Config
	store  *tester.Store
	logger *slog.Logger

	windowsMu sync.Mutex
	windows   map[string]*candleWindow // keyed by instID

	versionsMu sync.Mutex
	versions   []runningVersion // the currently-enabled version per kind

	// openMu serializes "check no open position exists, then open one" across every
	// instrument+kind combination the same way PaperTrader.openMu does — a strategy fires on one
	// bar-close goroutine per instrument, and two kinds firing on the same instrument at the same
	// instant must not both open a position (CLAUDE.md §16.9's exact race, applied here).
	openMu sync.Mutex
}

func (s *service) reloadStrategies(ctx context.Context) error {
	rows, err := s.store.EnabledVersions(ctx)
	if err != nil {
		return fmt.Errorf("load enabled versions: %w", err)
	}
	out := make([]runningVersion, 0, len(rows))
	for _, v := range rows {
		live, err := strategy.FromConfig(v.Kind, v.Config)
		if err != nil {
			s.logger.Error("failed to build strategy from version", "kind", v.Kind, "versionId", v.ID, "error", err)
			continue
		}
		out = append(out, runningVersion{versionID: v.ID, kind: v.Kind, live: live})
	}
	s.versionsMu.Lock()
	s.versions = out
	s.versionsMu.Unlock()
	return nil
}

func (s *service) instWindow(instID string) *candleWindow {
	s.windowsMu.Lock()
	defer s.windowsMu.Unlock()
	w, ok := s.windows[instID]
	if !ok {
		w = &candleWindow{}
		s.windows[instID] = w
	}
	return w
}

func (s *service) seedWindow(exchange interface {
	GetCandles(instID, bar string, limit int) ([]domain.Candle, error)
}, instID string) error {
	raw, err := exchange.GetCandles(instID, s.cfg.Tester.Bar, s.cfg.Tester.CandleWindow)
	if err != nil {
		return fmt.Errorf("seed candle window for %s: %w", instID, err)
	}
	out := make([]domain.Candle, len(raw))
	for i, c := range raw {
		out[len(raw)-1-i] = c // exchange returns newest-first; strategies expect oldest-first
	}
	w := s.instWindow(instID)
	w.mu.Lock()
	w.candles = out
	w.mu.Unlock()
	return nil
}

type tickEvent struct {
	InstID string `json:"instId"`
	Last   string `json:"last"`
}

func (s *service) handleTick(ctx context.Context, instID string, data []byte) error {
	var tick tickEvent
	if err := json.Unmarshal(data, &tick); err != nil {
		return fmt.Errorf("decode tick: %w", err)
	}
	if tick.InstID != instID {
		return nil
	}
	price, err := decimal.NewFromString(tick.Last)
	if err != nil {
		return fmt.Errorf("parse tick price %q: %w", tick.Last, err)
	}
	return s.checkOpenOrders(ctx, instID, price)
}

// checkOpenOrders closes any open position for instID whose SL/TP the live tick just touched —
// same domain logic paper-trading and the optimizer use (usecase.SLTPTouchReason), so a position
// closes at exactly the price level production code would agree it closed at.
func (s *service) checkOpenOrders(ctx context.Context, instID string, price decimal.Decimal) error {
	open, err := s.store.ListOpenOrders(ctx, instID)
	if err != nil {
		return fmt.Errorf("list open tester orders: %w", err)
	}
	for _, o := range open {
		reason, hit := usecase.SLTPTouchReason(o.Side, o.SLPx, o.TPPx, price)
		if !hit {
			continue
		}
		pnl := tester.RealizedPnL(o.Side, o.EntryPx, price, o.Size, o.Leverage)
		if err := s.store.CloseOrder(ctx, o.ID, reason, price, pnl); err != nil {
			s.logger.Error("failed to close tester order", "orderId", o.ID, "error", err)
			continue
		}
		s.logger.Info("closed tester order", "id", o.ID, "instId", instID, "reason", reason, "closePx", price, "pnl", pnl)
	}
	return nil
}

type candleEvent struct {
	InstID string   `json:"instId"`
	Bar    string   `json:"bar"`
	Candle []string `json:"candle"`
}

func (s *service) handleCandle(ctx context.Context, instID string, data []byte) error {
	var event candleEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("decode candle event: %w", err)
	}
	if event.InstID != instID || len(event.Candle) < 6 {
		return nil
	}
	confirm := ""
	if len(event.Candle) >= 9 {
		confirm = event.Candle[8]
	}
	if confirm != "1" {
		return nil // still forming; strategies here are candle-close-driven, same as production
	}
	c, err := parseCandleFields(event.Candle)
	if err != nil {
		return fmt.Errorf("parse candle: %w", err)
	}

	w := s.instWindow(instID)
	w.append(c, s.cfg.Tester.CandleWindow)
	window := w.snapshot()

	return s.evaluateVersions(ctx, instID, window, c.Close)
}

// evaluateVersions runs every enabled version against the freshly-closed candle for instID,
// opening a new position for the first one that fires — gated on this instrument having no open
// position at all (CLAUDE.md §16.9's one-position-per-instrument rule, applied here across every
// kind rather than per assignment, since this service is deliberately "all kinds, one slot").
func (s *service) evaluateVersions(ctx context.Context, instID string, window []domain.Candle, price decimal.Decimal) error {
	s.openMu.Lock()
	defer s.openMu.Unlock()

	open, err := s.store.ListOpenOrders(ctx, instID)
	if err != nil {
		return fmt.Errorf("list open tester orders: %w", err)
	}
	if len(open) > 0 {
		return nil
	}

	s.versionsMu.Lock()
	versions := append([]runningVersion(nil), s.versions...)
	s.versionsMu.Unlock()

	for _, rv := range versions {
		signal, err := rv.live.Evaluate(window)
		if err != nil {
			s.logger.Warn("tester strategy evaluation failed", "kind", rv.kind, "instId", instID, "error", err)
			continue
		}
		if signal.Side == strategy.Hold {
			continue
		}

		order := tester.BuildOrder(instID, rv.versionID, s.cfg.Tester.Bar, price, s.cfg.Tester.NotionalUSD, s.cfg.Tester.Leverage, signal)
		if order.SLPx == nil {
			// Same non-negotiable rule as production (CLAUDE.md §16.9): a position with no
			// stop-loss is unbounded downside, not a missed opportunity. Skip rather than open
			// unprotected — this service has no clamp/EnsureStop pass to fall back on.
			s.logger.Warn("skipping signal with no stop-loss", "kind", rv.kind, "instId", instID)
			continue
		}
		id, err := s.store.OpenOrder(ctx, order)
		if err != nil {
			s.logger.Error("failed to open tester order", "kind", rv.kind, "instId", instID, "error", err)
			continue
		}
		s.logger.Info("opened tester order", "id", id, "kind", rv.kind, "versionId", rv.versionID,
			"instId", instID, "side", order.Side, "entryPx", price)
		return nil // one open position per instrument; the rest of this candle's signals wait for next time
	}
	return nil
}

func parseCandleFields(fields []string) (domain.Candle, error) {
	ms, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse ts %q: %w", fields[0], err)
	}
	o, err := decimal.NewFromString(fields[1])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse open %q: %w", fields[1], err)
	}
	h, err := decimal.NewFromString(fields[2])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse high %q: %w", fields[2], err)
	}
	l, err := decimal.NewFromString(fields[3])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse low %q: %w", fields[3], err)
	}
	c, err := decimal.NewFromString(fields[4])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse close %q: %w", fields[4], err)
	}
	v, err := decimal.NewFromString(fields[5])
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse vol %q: %w", fields[5], err)
	}
	return domain.Candle{Timestamp: time.UnixMilli(ms).UTC(), Open: o, High: h, Low: l, Close: c, Volume: v}, nil
}
