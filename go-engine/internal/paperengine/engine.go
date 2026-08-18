// Package paperengine implements the Paper Trading Engine (CLAUDE.md §8): it evaluates
// strategies against live prices, opens virtual orders with full SL/TP features, and monitors
// them against the real price feed until SL or TP is hit. Closed trades are persisted and become
// the RL training data — no real orders are ever sent to OKX from this package.
//
// Driven entirely by the WS-fed Redis event bus (CLAUDE.md §12), not REST polling: every tick
// triggers an immediate SL/TP check (no missed intra-bar wicks, no polling delay), and every
// finalized candle triggers strategy re-evaluation + persistence. A REST call is used only once,
// at startup, to seed the initial rolling candle window.
package paperengine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/rez/okxBot/go-engine/internal/metrics"
	"github.com/rez/okxBot/go-engine/internal/okx"
	"github.com/rez/okxBot/go-engine/internal/okx/rest"
	"github.com/rez/okxBot/go-engine/internal/port"
	"github.com/rez/okxBot/go-engine/internal/strategy"
	"github.com/rez/okxBot/go-engine/internal/stream"
)

// Engine runs the paper-trading loop for a single instrument, driven by ticks/candles from Redis.
type Engine struct {
	InstID         string
	Bar            string // candle timeframe used for strategy evaluation, e.g. "1m"
	CandleWindow   int    // how many recent candles to keep in memory for strategy evaluation
	Strategies     []strategy.Strategy
	RESTClient     *rest.Client // used once at startup to seed the initial candle window
	TickConsumer   *stream.Consumer
	CandleConsumer *stream.Consumer
	Repo           port.Repository
	NotionalUSD    float64
	MaxOpenOrders  int
	Logger         *slog.Logger

	candles []strategy.Candle // append-only working window, touched only by the candle consumer
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

// Run seeds the initial candle window via REST, then consumes ticks/candles from Redis until
// ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := e.seedCandles(); err != nil {
		return fmt.Errorf("seed initial candle window: %w", err)
	}

	errCh := make(chan error, 2)
	go func() {
		errCh <- e.TickConsumer.Run(ctx, func(ctx context.Context, data []byte) error {
			return e.handleTick(ctx, data, logger)
		})
	}()
	go func() {
		errCh <- e.CandleConsumer.Run(ctx, func(ctx context.Context, data []byte) error {
			return e.handleCandle(ctx, data, logger)
		})
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (e *Engine) seedCandles() error {
	raw, err := e.RESTClient.GetCandles(e.InstID, e.Bar, e.CandleWindow)
	if err != nil {
		return err
	}
	candles, err := toStrategyCandles(raw)
	if err != nil {
		return err
	}
	e.candles = candles
	return nil
}

func (e *Engine) handleTick(ctx context.Context, data []byte, logger *slog.Logger) error {
	var tick tickEvent
	if err := json.Unmarshal(data, &tick); err != nil {
		return fmt.Errorf("decode tick: %w", err)
	}
	if tick.InstID != e.InstID {
		return nil // shared stream across instruments; this engine only cares about its own
	}
	price, err := strconv.ParseFloat(tick.Last, 64)
	if err != nil {
		return fmt.Errorf("parse tick price %q: %w", tick.Last, err)
	}
	return e.monitorOpenOrders(ctx, price, logger)
}

func (e *Engine) handleCandle(ctx context.Context, data []byte, logger *slog.Logger) error {
	var event candleEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("decode candle event: %w", err)
	}
	if event.InstID != e.InstID || len(event.Candle) < 6 {
		return nil
	}

	raw := okx.Candle{Ts: event.Candle[0], Open: event.Candle[1], High: event.Candle[2], Low: event.Candle[3], Close: event.Candle[4], Vol: event.Candle[5]}
	if len(event.Candle) >= 9 {
		raw.Confirm = event.Candle[8]
	}

	c, err := parseCandle(raw)
	if err != nil {
		return fmt.Errorf("parse candle: %w", err)
	}

	e.candles = append(e.candles, c)
	if len(e.candles) > e.CandleWindow {
		e.candles = e.candles[len(e.candles)-e.CandleWindow:]
	}

	if raw.Confirm != "1" {
		return nil // still forming; wait for the finalized bar before persisting/evaluating
	}
	if err := e.Repo.SaveCandle(ctx, toPortCandle(e.InstID, e.Bar, raw)); err != nil {
		logger.Warn("failed to persist candle", "instId", e.InstID, "error", err)
	}
	return e.evaluateStrategies(ctx, c.Close, logger)
}

func (e *Engine) evaluateStrategies(ctx context.Context, price float64, logger *slog.Logger) error {
	open, err := e.Repo.ListOpenPaperOrders(ctx, e.InstID)
	if err != nil {
		return fmt.Errorf("list open paper orders: %w", err)
	}
	if e.MaxOpenOrders > 0 && len(open) >= e.MaxOpenOrders {
		return nil
	}

	for _, s := range e.Strategies {
		signal, err := s.Evaluate(e.candles)
		if err != nil {
			logger.Warn("strategy evaluation failed", "strategy", s.Name(), "instId", e.InstID, "error", err)
			continue
		}
		metrics.StrategySignalsTotal.WithLabelValues(s.Name(), e.InstID, string(signal.Side)).Inc()
		if signal.Side == strategy.Hold {
			continue
		}

		order := buildPaperOrder(e.InstID, price, signal, e.NotionalUSD)
		id, err := e.Repo.OpenPaperOrder(ctx, order)
		if err != nil {
			logger.Error("failed to open paper order", "strategy", s.Name(), "instId", e.InstID, "error", err)
			continue
		}
		metrics.PaperOrdersOpenedTotal.WithLabelValues(s.Name(), e.InstID, string(signal.Side)).Inc()
		logger.Info("opened paper order", "id", id, "strategy", s.Name(), "instId", e.InstID,
			"side", signal.Side, "entry", price, "confidence", signal.Confidence)
	}
	return nil
}

func (e *Engine) monitorOpenOrders(ctx context.Context, price float64, logger *slog.Logger) error {
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
		metrics.PaperOrdersRealizedPnL.WithLabelValues(e.InstID).Add(pnl)
		logger.Info("closed paper order", "id", o.ID, "instId", e.InstID, "reason", reason, "closePx", price, "pnl", pnl)
	}
	return nil
}

func closeReason(o port.PaperOrder, price float64) (string, bool) {
	switch o.Side {
	case "buy":
		if o.SLPx != nil && price <= *o.SLPx {
			return "sl", true
		}
		if o.TPPx != nil && price >= *o.TPPx {
			return "tp", true
		}
	case "sell":
		if o.SLPx != nil && price >= *o.SLPx {
			return "sl", true
		}
		if o.TPPx != nil && price <= *o.TPPx {
			return "tp", true
		}
	}
	return "", false
}

func realizedPnL(o port.PaperOrder, closePx float64) float64 {
	direction := 1.0
	if o.Side == "sell" {
		direction = -1.0
	}
	return direction * (closePx - o.EntryPx) / o.EntryPx * o.Size * o.Leverage
}

func buildPaperOrder(instID string, price float64, signal strategy.Signal, notionalUSD float64) port.PaperOrder {
	var slPx, tpPx *float64
	direction := 1.0
	if signal.Side == strategy.Sell {
		direction = -1.0
	}
	if signal.SLPct > 0 {
		v := price - direction*signal.SLPct*price
		slPx = &v
	}
	if signal.TPPct > 0 {
		v := price + direction*signal.TPPct*price
		tpPx = &v
	}

	return port.PaperOrder{
		InstID:   instID,
		Side:     string(signal.Side),
		EntryPx:  price,
		SLPx:     slPx,
		TPPx:     tpPx,
		Size:     notionalUSD,
		Leverage: 1,
	}
}

func toStrategyCandles(raw []okx.Candle) ([]strategy.Candle, error) {
	out := make([]strategy.Candle, 0, len(raw))
	// OKX returns newest-first; strategies expect oldest-first.
	for i := len(raw) - 1; i >= 0; i-- {
		c, err := parseCandle(raw[i])
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func parseCandle(raw okx.Candle) (strategy.Candle, error) {
	open, err := strconv.ParseFloat(raw.Open, 64)
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("parse open %q: %w", raw.Open, err)
	}
	high, err := strconv.ParseFloat(raw.High, 64)
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("parse high %q: %w", raw.High, err)
	}
	low, err := strconv.ParseFloat(raw.Low, 64)
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("parse low %q: %w", raw.Low, err)
	}
	closePx, err := strconv.ParseFloat(raw.Close, 64)
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("parse close %q: %w", raw.Close, err)
	}
	vol, err := strconv.ParseFloat(raw.Vol, 64)
	if err != nil {
		return strategy.Candle{}, fmt.Errorf("parse vol %q: %w", raw.Vol, err)
	}
	return strategy.Candle{Open: open, High: high, Low: low, Close: closePx, Volume: vol}, nil
}

func toPortCandle(instID, bar string, raw okx.Candle) port.Candle {
	msStr := raw.Ts
	ms, _ := strconv.ParseInt(msStr, 10, 64)
	c, _ := parseCandle(raw)
	return port.Candle{
		InstID: instID,
		Bar:    bar,
		Ts:     time.UnixMilli(ms).UTC(),
		Open:   c.Open,
		High:   c.High,
		Low:    c.Low,
		Close:  c.Close,
		Volume: c.Volume,
	}
}
