// Command strategy-optimizer runs cmd/strategy-optimizer (CLAUDE.md §16): a long-lived service
// that time-boxes real-market-data trial runs of candidate strategy.Strategy parameter sets per
// (inst_id, kind) target, driven by the Python/Optuna sidecar (optimizer-service/) for candidate
// proposals, and persists the best-performing candidate as a new durable sub-strategy row on a
// configurable schedule. This REVISES CLAUDE.md §16.4's original "manually-triggered only"
// framing — see §16.7. Deliberately independent of cmd/paper-trader/cmd/trader: trial positions
// here are disposable Redis state, never paper_orders rows, and their SL/TP-touch outcome is the
// only judgment signal (CLAUDE.md §16.1's "keep the RL agent out of this" decision).
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
	"github.com/eghbalii/okxBot/go-engine/internal/optimizer"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
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
	// Same "must work regardless of which service started first" reasoning as cmd/api (see its
	// main.go) — the optimizer needs origin rows to discover ParamSpecs even if paper-trader/api
	// haven't run yet on a fresh install.
	if err := strategy.SeedOrigins(ctx, repo); err != nil {
		logger.Error("failed to seed origin strategies", "error", err)
		os.Exit(1)
	}

	restClient := rest.New(cfg.OKX.RESTBaseURL, cfg.OKX.APIKey, cfg.OKX.APISecret, cfg.OKX.APIPassphrase, cfg.OKX.Simulated)
	sidecar := optimizer.NewSidecarClient(cfg.Optimizer.URL)
	store := optimizer.NewTrialStore(cfg.Redis.Addr)
	defer store.CloseConn()

	runCfg := optimizer.RunConfig{
		RunDuration:           mustParseDuration(cfg.Optimizer.RunDuration, 4*time.Hour, logger),
		MinTradesPerCandidate: cfg.Optimizer.MinTradesPerCandidate,
		MinImprovementPct:     cfg.Optimizer.MinImprovementPct,
		MinWinRatePctFloor:    cfg.Optimizer.MinWinRatePctFloor,
		Bar:                   cfg.Optimizer.Bar,
		CandleWindow:          cfg.Optimizer.CandleWindow,
		BatchSize:             cfg.Optimizer.BatchSize,
		TrialTTLBuffer:        time.Duration(cfg.Optimizer.TrialTTLBufferSec) * time.Second,
		MaxLossPct:            cfg.Optimizer.MaxLossPct,
	}

	svc := &service{
		cfg:        cfg,
		runCfg:     runCfg,
		repo:       repo,
		exchange:   restClient,
		sidecar:    sidecar,
		store:      store,
		logger:     logger,
		windows:    make(map[targetKey]*candleWindow),
		activeRuns: make(map[targetKey]*optimizer.Run),
	}

	// One shared Kafka consumer-group reader per topic (tickers + the configured bar), fanned out
	// to each target instrument by instId via kafkastream.Dispatcher — mirrors cmd/paper-trader's
	// wiring. Kafka consumer groups own whole partitions (no per-instrument consumer identity the
	// way Redis Streams' XREADGROUP had), scoped to the optimizer's own consumer group name so it
	// never competes for offsets with paper-trader/trader.
	tickDispatcher := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.tickers", "strategy-optimizer"))
	candleDispatcher := kafkastream.NewDispatcher(kafkastream.NewConsumer(cfg.Kafka.Brokers, "okx.candles."+cfg.Optimizer.Bar, "strategy-optimizer"))

	instIDs := uniqueInstIDs(cfg.Optimizer.Targets)
	for _, instID := range instIDs {
		instID := instID
		tickDispatcher.Register(instID, func(ctx context.Context, data []byte) error {
			return svc.handleTick(ctx, instID, data)
		})
		candleDispatcher.Register(instID, func(ctx context.Context, data []byte) error {
			return svc.handleCandle(ctx, instID, data)
		})

		if err := svc.seedWindow(instID); err != nil {
			logger.Error("failed to seed initial candle window", "instId", instID, "error", err)
		}
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

	// Built-in scheduler (CLAUDE.md §16 decision 3): fires one time-boxed run per configured
	// target on cfg.Optimizer.ScheduleInterval, sequentially — bounded, not "everything at once"
	// (CLAUDE.md §16.6's caution, applied the same way §15.1 applied it to token/strategy count).
	scheduleInterval := mustParseDuration(cfg.Optimizer.ScheduleInterval, 24*time.Hour, logger)
	go svc.runScheduler(ctx, scheduleInterval)

	httpServer := &http.Server{
		Addr:    cfg.Optimizer.Addr,
		Handler: svc.routes(),
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("starting strategy-optimizer", "addr", cfg.Optimizer.Addr, "targets", cfg.Optimizer.Targets, "scheduleInterval", scheduleInterval, "runDuration", runCfg.RunDuration)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("strategy-optimizer server exited", "error", err)
		os.Exit(1)
	}
}

// targetKey identifies one (inst_id, kind) optimization target.
type targetKey struct {
	InstID string
	Kind   string
}

// candleWindow holds one instrument's shared candle window (CLAUDE.md §16 decision 4: "you'll
// likely need your own candle-window-holding loop since this evaluates MANY candidate strategies
// concurrently for one (token,kind)") — one window per instrument (not per target/candidate),
// since every candidate for that instrument evaluates against the same real market data.
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
	w.candles = append(w.candles, c)
	if len(w.candles) > limit {
		w.candles = w.candles[len(w.candles)-limit:]
	}
}

// service holds cmd/strategy-optimizer's dependencies and runtime state.
type service struct {
	cfg      *config.Config
	runCfg   optimizer.RunConfig
	repo     port.Repository
	exchange port.ExchangeClient
	sidecar  *optimizer.SidecarClient
	store    *optimizer.TrialStore
	logger   *slog.Logger

	windowsMu sync.Mutex
	windows   map[targetKey]*candleWindow // keyed by targetKey but only InstID matters (shared per inst)

	runsMu     sync.Mutex
	activeRuns map[targetKey]*optimizer.Run
	pastRuns   []optimizer.RunStatus // finished runs, most-recent-last, for GET /status lookups by id
}

func (s *service) instWindow(instID string) *candleWindow {
	s.windowsMu.Lock()
	defer s.windowsMu.Unlock()
	key := targetKey{InstID: instID}
	w, ok := s.windows[key]
	if !ok {
		w = &candleWindow{}
		s.windows[key] = w
	}
	return w
}

func (s *service) seedWindow(instID string) error {
	raw, err := s.exchange.GetCandles(instID, s.cfg.Optimizer.Bar, s.runCfg.CandleWindow)
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

	for _, run := range s.runsForInst(instID) {
		run.CheckTick(ctx, price)
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
		return nil // still forming
	}
	c, err := parseCandleFields(event.Candle)
	if err != nil {
		return fmt.Errorf("parse candle: %w", err)
	}

	w := s.instWindow(instID)
	w.append(c, s.runCfg.CandleWindow)
	window := w.snapshot()

	for _, run := range s.runsForInst(instID) {
		run.EnsureCandidates(ctx)
		run.EvaluateCandle(ctx, window, c.Close)
	}
	return nil
}

func (s *service) runsForInst(instID string) []*optimizer.Run {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	var out []*optimizer.Run
	for k, r := range s.activeRuns {
		if k.InstID == instID {
			out = append(out, r)
		}
	}
	return out
}

// runScheduler fires one time-boxed run per configured target every interval, sequentially
// (bounded concurrency: never more than one run in flight per target, and targets are started
// one after another rather than all at once — CLAUDE.md §16.6's "don't default to everything at
// once" caution, decision 3's "avoid unbounded parallel Optuna+trial load").
func (s *service) runScheduler(ctx context.Context, interval time.Duration) {
	// Kick off the first pass shortly after startup rather than waiting a full interval, so a
	// freshly-deployed optimizer doesn't sit idle for up to 24h before ever running.
	timer := time.NewTimer(1 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			for _, target := range s.cfg.Optimizer.Targets {
				if ctx.Err() != nil {
					return
				}
				if err := s.startRun(ctx, target.InstID, target.Kind); err != nil {
					s.logger.Error("scheduled optimization run failed to start", "instId", target.InstID, "kind", target.Kind, "error", err)
					continue
				}
				s.awaitRun(ctx, target.InstID, target.Kind)
			}
			timer.Reset(interval)
		}
	}
}

// startRun begins a new time-boxed run for (instID, kind) if one isn't already active for that
// target. Reuses the shared instrument candle window seeded/maintained by the tick/candle
// consumers above.
func (s *service) startRun(ctx context.Context, instID, kind string) error {
	key := targetKey{InstID: instID, Kind: kind}

	s.runsMu.Lock()
	if existing, ok := s.activeRuns[key]; ok && !existing.Expired() {
		s.runsMu.Unlock()
		return fmt.Errorf("a run is already active for %s/%s", instID, kind)
	}
	s.runsMu.Unlock()

	factory, ok := strategy.Factories[kind]
	if !ok {
		return fmt.Errorf("unknown strategy kind %q", kind)
	}
	origin := factory()

	runID := fmt.Sprintf("%s-%s-%d", instID, kind, time.Now().UTC().UnixNano())
	run := optimizer.NewRun(runID, instID, kind, origin, s.runCfg, s.sidecar, s.store, s.repo, s.logger)

	s.runsMu.Lock()
	s.activeRuns[key] = run
	s.runsMu.Unlock()

	if err := run.EnsureCandidates(ctx); err != nil {
		s.logger.Warn("initial candidate suggestion failed", "instId", instID, "kind", kind, "error", err)
	}
	s.logger.Info("started optimization run", "runId", runID, "instId", instID, "kind", kind, "bar", s.cfg.Optimizer.Bar, "runDuration", s.runCfg.RunDuration)
	return nil
}

// awaitRun blocks (with periodic candidate top-ups) until the given target's active run expires,
// then finalizes it and files it under pastRuns. Used by the sequential scheduler; the manual
// POST /optimize path does not block the HTTP handler on this (see handleOptimize).
func (s *service) awaitRun(ctx context.Context, instID, kind string) {
	key := targetKey{InstID: instID, Kind: kind}
	s.runsMu.Lock()
	run, ok := s.activeRuns[key]
	s.runsMu.Unlock()
	if !ok {
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := run.EnsureCandidates(ctx); err != nil {
				s.logger.Warn("candidate top-up failed", "runId", run.ID, "error", err)
			}
			if run.Expired() {
				status := run.Finalize(ctx)
				s.runsMu.Lock()
				delete(s.activeRuns, key)
				s.pastRuns = append(s.pastRuns, status)
				s.runsMu.Unlock()
				s.logger.Info("finished optimization run", "runId", run.ID, "outcome", status.Outcome, "persisted", status.Persisted)
				return
			}
		}
	}
}

func (s *service) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /optimize", s.handleOptimize)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

type optimizeRequest struct {
	InstID string `json:"inst_id"`
	Kind   string `json:"kind"`
}

// handleOptimize starts one manually-triggered, time-boxed run right now (CLAUDE.md §16 decision
// 3). Returns immediately with the run id; the run itself proceeds in the background (a 4h time
// box is far too long to hold an HTTP request open for) and is polled via GET /status.
func (s *service) handleOptimize(w http.ResponseWriter, r *http.Request) {
	var req optimizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.InstID == "" || req.Kind == "" {
		writeError(w, http.StatusBadRequest, "inst_id and kind are required")
		return
	}

	if err := s.startRun(r.Context(), req.InstID, req.Kind); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	go s.awaitRun(context.Background(), req.InstID, req.Kind)

	key := targetKey{InstID: req.InstID, Kind: req.Kind}
	s.runsMu.Lock()
	run := s.activeRuns[key]
	s.runsMu.Unlock()

	writeJSON(w, http.StatusAccepted, run.Status())
}

// handleStatus polls a run's progress by run_id (CLAUDE.md §16 decision 3) — checks active runs
// first, then finished ones.
func (s *service) handleStatus(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("run_id")
	if runID == "" {
		writeError(w, http.StatusBadRequest, "run_id is a required query param")
		return
	}

	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	for _, run := range s.activeRuns {
		if run.ID == runID {
			writeJSON(w, http.StatusOK, run.Status())
			return
		}
	}
	for _, st := range s.pastRuns {
		if st.RunID == runID {
			writeJSON(w, http.StatusOK, st)
			return
		}
	}
	writeError(w, http.StatusNotFound, "run not found (it may have been active before this process last restarted — run state is in-memory only)")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func uniqueInstIDs(targets []config.OptimizerTarget) []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range targets {
		if !seen[t.InstID] {
			seen[t.InstID] = true
			out = append(out, t.InstID)
		}
	}
	return out
}

func mustParseDuration(s string, fallback time.Duration, logger *slog.Logger) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		logger.Warn("invalid duration in config, using fallback", "value", s, "fallback", fallback, "error", err)
		return fallback
	}
	return d
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
