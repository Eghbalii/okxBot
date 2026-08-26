package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// StrategyAssignment pairs a live Strategy value with the candle timeframe it evaluates against
// and the DB row (StrategyID) it was configured from. A PaperTrader can run several strategies
// across several timeframes concurrently — each strategy only re-evaluates when its own assigned
// bar closes. StrategyID is threaded onto every paper order this assignment opens (CLAUDE.md
// §11.3), so it must be populated whenever the assignment is resolved from a durable
// port.StrategyAssignment/port.StrategyConfig row — 0 is only valid for ad-hoc, unpersisted use
// (e.g. tests).
type StrategyAssignment struct {
	Bar        string
	Strategy   strategy.Strategy
	StrategyID int64
}

// PaperTrader implements the Paper Trading Engine (CLAUDE.md §8): it evaluates strategies
// against live prices, opens virtual orders with full SL/TP features, and monitors them against
// the real price feed until SL or TP is hit. Closed trades are persisted and become the RL
// training data — no real orders are ever sent to the exchange from this use-case.
//
// Driven entirely by the WS-fed event bus (CLAUDE.md §12), not REST polling: every tick triggers
// an immediate SL/TP check (no missed intra-bar wicks, no polling delay), and every finalized
// candle triggers re-evaluation of strategies assigned to that candle's timeframe, and is
// persisted. The exchange port is used only once per configured timeframe, at startup, to seed
// each candle window.
type PaperTrader struct {
	InstID          string
	Bars            []string // candle timeframes to maintain windows for, e.g. ["1m", "15m", "1h"]
	CandleWindow    int      // how many recent candles to keep in memory per timeframe
	Strategies      []StrategyAssignment
	Exchange        port.ExchangeClient // used once per bar at startup to seed each candle window
	TickConsumer    port.MarketDataConsumer
	CandleConsumers map[string]port.MarketDataConsumer // keyed by bar
	Repo            port.Repository
	NotionalUSD     decimal.Decimal
	MaxOpenOrders   int
	Logger          *slog.Logger

	// Model/ActiveTokens/TokenBudgetUSD wire the RL agent's in-trade SL/TP adjustment pass
	// (CLAUDE.md §15.4). Model may be nil, in which case the adjustment pass is skipped entirely —
	// this lets PaperTrader run exactly as before (strategy-only) wherever the RL service isn't
	// configured, same "additive, never required" pattern as the rest of §15's rollout.
	Model          port.ModelClient
	ActiveTokens   []string        // the roster used to build the token-identity one-hot, CLAUDE.md §15.3
	TokenBudgetUSD decimal.Decimal // this token's configured paper-mode sub-budget, CLAUDE.md §15.6/§15.7

	// candlesMu guards candles: each bar has its own consumer goroutine (see Run), so writes to
	// this map (even to distinct keys) must be synchronized — concurrent map writes are a fatal
	// Go runtime error, not just a race.
	candlesMu sync.Mutex
	candles   map[string][]domain.Candle // keyed by bar
}

type tickEvent struct {
	InstID string `json:"instId"`
	Last   string `json:"last"`
}

type candleEvent struct {
	InstID string   `json:"instId"`
	Bar    string   `json:"bar"`
	Candle []string `json:"candle"`
}

// Run seeds each timeframe's initial candle window via the exchange port, then consumes
// ticks/candles from the event bus until ctx is cancelled.
func (e *PaperTrader) Run(ctx context.Context) error {
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := e.seedCandles(); err != nil {
		return fmt.Errorf("seed initial candle windows: %w", err)
	}

	errCh := make(chan error, 1+len(e.CandleConsumers))
	go func() {
		errCh <- e.TickConsumer.Run(ctx, func(ctx context.Context, data []byte) error {
			return e.handleTick(ctx, data, logger)
		})
	}()
	for bar, consumer := range e.CandleConsumers {
		bar, consumer := bar, consumer
		go func() {
			errCh <- consumer.Run(ctx, func(ctx context.Context, data []byte) error {
				return e.handleCandle(ctx, bar, data, logger)
			})
		}()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (e *PaperTrader) seedCandles() error {
	candles := make(map[string][]domain.Candle, len(e.Bars))
	for _, bar := range e.Bars {
		raw, err := e.Exchange.GetCandles(e.InstID, bar, e.CandleWindow)
		if err != nil {
			return fmt.Errorf("seed candle window for bar %s: %w", bar, err)
		}
		// Exchange returns newest-first; strategies expect oldest-first.
		out := make([]domain.Candle, len(raw))
		for i, c := range raw {
			out[len(raw)-1-i] = c
		}
		candles[bar] = out
	}
	// Run before this returns hasn't started the per-bar consumer goroutines yet, so no lock is
	// strictly needed here, but take it anyway for consistency with every other candles access.
	e.candlesMu.Lock()
	e.candles = candles
	e.candlesMu.Unlock()
	return nil
}

func (e *PaperTrader) handleTick(ctx context.Context, data []byte, logger *slog.Logger) error {
	var tick tickEvent
	if err := json.Unmarshal(data, &tick); err != nil {
		return fmt.Errorf("decode tick: %w", err)
	}
	if tick.InstID != e.InstID {
		return nil // shared stream across instruments; this engine only cares about its own
	}
	price, err := decimal.NewFromString(tick.Last)
	if err != nil {
		return fmt.Errorf("parse tick price %q: %w", tick.Last, err)
	}
	return e.monitorOpenOrders(ctx, price, logger)
}

func (e *PaperTrader) handleCandle(ctx context.Context, bar string, data []byte, logger *slog.Logger) error {
	var event candleEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("decode candle event: %w", err)
	}
	if event.InstID != e.InstID || len(event.Candle) < 6 {
		return nil
	}

	confirm := ""
	if len(event.Candle) >= 9 {
		confirm = event.Candle[8]
	}
	c, err := parseCandleFields(event.Candle[0], event.Candle[1], event.Candle[2], event.Candle[3], event.Candle[4], event.Candle[5])
	if err != nil {
		return fmt.Errorf("parse candle (bar %s): %w", bar, err)
	}

	e.candlesMu.Lock()
	window := append(e.candles[bar], c)
	if len(window) > e.CandleWindow {
		window = window[len(window)-e.CandleWindow:]
	}
	e.candles[bar] = window
	e.candlesMu.Unlock()

	if confirm != "1" {
		return nil // still forming; wait for the finalized bar before persisting/evaluating
	}
	if err := e.Repo.SaveCandle(ctx, port.Candle{InstID: e.InstID, Bar: bar, Candle: c}); err != nil {
		logger.Warn("failed to persist candle", "instId", e.InstID, "bar", bar, "error", err)
	}
	if err := e.evaluateStrategies(ctx, bar, c.Close, logger); err != nil {
		return err
	}
	// CLAUDE.md §15.4: RL-driven SL/TP adjustment runs at candle-close cadence (not every tick) to
	// avoid overreacting to noise. Best-effort: a failure here must never block strategy evaluation
	// or candle persistence above, which already succeeded.
	if e.Model != nil {
		e.adjustOpenOrdersWithRL(ctx, bar, c.Close, logger)
	}
	return nil
}

func (e *PaperTrader) evaluateStrategies(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) error {
	open, err := e.Repo.ListOpenPaperOrders(ctx, e.InstID)
	if err != nil {
		return fmt.Errorf("list open paper orders: %w", err)
	}
	if e.MaxOpenOrders > 0 && len(open) >= e.MaxOpenOrders {
		return nil
	}

	e.candlesMu.Lock()
	window := append([]domain.Candle(nil), e.candles[bar]...) // snapshot: don't hold the lock across Strategy.Evaluate
	e.candlesMu.Unlock()

	for _, a := range e.Strategies {
		if a.Bar != bar {
			continue // only re-evaluate strategies assigned to the timeframe that just closed
		}
		s := a.Strategy
		signal, err := s.Evaluate(window)
		if err != nil {
			logger.Warn("strategy evaluation failed", "strategy", s.Name(), "instId", e.InstID, "bar", bar, "error", err)
			continue
		}
		metrics.StrategySignalsTotal.WithLabelValues(s.Name(), e.InstID, string(signal.Side)).Inc()
		if signal.Side == strategy.Hold {
			continue
		}

		order := buildPaperOrder(e.InstID, price, signal, e.NotionalUSD, a.StrategyID)
		id, err := e.Repo.OpenPaperOrder(ctx, order)
		if err != nil {
			logger.Error("failed to open paper order", "strategy", s.Name(), "instId", e.InstID, "error", err)
			continue
		}
		metrics.PaperOrdersOpenedTotal.WithLabelValues(s.Name(), e.InstID, string(signal.Side)).Inc()
		logger.Info("opened paper order", "id", id, "strategy", s.Name(), "instId", e.InstID, "bar", bar,
			"side", signal.Side, "entry", price, "confidence", signal.Confidence)
	}
	return nil
}

func (e *PaperTrader) monitorOpenOrders(ctx context.Context, price decimal.Decimal, logger *slog.Logger) error {
	open, err := e.Repo.ListOpenPaperOrders(ctx, e.InstID)
	if err != nil {
		return fmt.Errorf("list open paper orders: %w", err)
	}
	metrics.PaperOrdersOpenGauge.WithLabelValues(e.InstID).Set(float64(len(open)))

	for _, o := range open {
		reason, hit := closeReason(o, price)
		if !hit {
			continue
		}
		pnl := realizedPnL(o, price)
		if err := e.Repo.ClosePaperOrder(ctx, o.ID, price, reason, pnl); err != nil {
			logger.Error("failed to close paper order", "id", o.ID, "error", err)
			continue
		}
		metrics.PaperOrdersClosedTotal.WithLabelValues(e.InstID, reason).Inc()
		// Prometheus metrics have no decimal support; InexactFloat64 is acceptable here since
		// this is write-only telemetry, not a value used in further financial arithmetic.
		metrics.PaperOrdersRealizedPnL.WithLabelValues(e.InstID).Add(pnl.InexactFloat64())
		logger.Info("closed paper order", "id", o.ID, "instId", e.InstID, "reason", reason, "closePx", price, "pnl", pnl)

		// CLAUDE.md §15.4/§15.6/§15.7: only a baseline order's outcome counts toward the token's
		// tracked budget/reward — an rl_adjusted fork is tracking-only (its whole purpose is to be
		// compared against its baseline parent afterward, not to be treated as a second real bet).
		if o.Variant == "baseline" || o.Variant == "" {
			if e.TokenBudgetUSD.IsPositive() {
				if _, reset, err := e.Repo.ApplyTokenPnL(ctx, e.InstID, pnl); err != nil {
					logger.Error("failed to apply token pnl", "instId", e.InstID, "error", err)
				} else if reset {
					logger.Warn("token budget drained, reset to configured budget", "instId", e.InstID, "budgetUsd", e.TokenBudgetUSD)
				}
			}
		}
	}
	return nil
}

func closeReason(o port.PaperOrder, price decimal.Decimal) (string, bool) {
	switch o.Side {
	case "buy":
		if o.SLPx != nil && price.LessThanOrEqual(*o.SLPx) {
			return "sl", true
		}
		if o.TPPx != nil && price.GreaterThanOrEqual(*o.TPPx) {
			return "tp", true
		}
	case "sell":
		if o.SLPx != nil && price.GreaterThanOrEqual(*o.SLPx) {
			return "sl", true
		}
		if o.TPPx != nil && price.LessThanOrEqual(*o.TPPx) {
			return "tp", true
		}
	}
	return "", false
}

func realizedPnL(o port.PaperOrder, closePx decimal.Decimal) decimal.Decimal {
	direction := decimal.NewFromInt(1)
	if o.Side == "sell" {
		direction = decimal.NewFromInt(-1)
	}
	return direction.Mul(closePx.Sub(o.EntryPx)).Div(o.EntryPx).Mul(o.Size).Mul(o.Leverage)
}

func buildPaperOrder(instID string, price decimal.Decimal, signal strategy.Signal, notionalUSD decimal.Decimal, strategyID int64) port.PaperOrder {
	var slPx, tpPx *decimal.Decimal
	direction := decimal.NewFromInt(1)
	if signal.Side == strategy.Sell {
		direction = decimal.NewFromInt(-1)
	}
	if signal.SLPct.IsPositive() {
		v := price.Sub(direction.Mul(signal.SLPct).Mul(price))
		slPx = &v
	}
	if signal.TPPct.IsPositive() {
		v := price.Add(direction.Mul(signal.TPPct).Mul(price))
		tpPx = &v
	}

	var strategyIDPtr *int64
	if strategyID != 0 {
		strategyIDPtr = &strategyID
	}

	return port.PaperOrder{
		InstID:     instID,
		StrategyID: strategyIDPtr,
		Side:       string(signal.Side),
		EntryPx:    price,
		SLPx:       slPx,
		TPPx:       tpPx,
		Size:       notionalUSD,
		Leverage:   decimal.NewFromInt(1),
	}
}

func parseCandleFields(ts, open, high, low, close, vol string) (domain.Candle, error) {
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse ts %q: %w", ts, err)
	}
	o, err := decimal.NewFromString(open)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse open %q: %w", open, err)
	}
	h, err := decimal.NewFromString(high)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse high %q: %w", high, err)
	}
	l, err := decimal.NewFromString(low)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse low %q: %w", low, err)
	}
	c, err := decimal.NewFromString(close)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse close %q: %w", close, err)
	}
	v, err := decimal.NewFromString(vol)
	if err != nil {
		return domain.Candle{}, fmt.Errorf("parse vol %q: %w", vol, err)
	}
	return domain.Candle{Timestamp: time.UnixMilli(ms).UTC(), Open: o, High: h, Low: l, Close: c, Volume: v}, nil
}
