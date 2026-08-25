package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/rez/okxBot/go-engine/internal/domain"
	"github.com/rez/okxBot/go-engine/internal/metrics"
	"github.com/rez/okxBot/go-engine/internal/port"
	"github.com/rez/okxBot/go-engine/internal/strategy"
)

// PaperTrader implements the Paper Trading Engine (CLAUDE.md §8): it evaluates strategies
// against live prices, opens virtual orders with full SL/TP features, and monitors them against
// the real price feed until SL or TP is hit. Closed trades are persisted and become the RL
// training data — no real orders are ever sent to the exchange from this use-case.
//
// Driven entirely by the WS-fed event bus (CLAUDE.md §12), not REST polling: every tick triggers
// an immediate SL/TP check (no missed intra-bar wicks, no polling delay), and every finalized
// candle triggers strategy re-evaluation + persistence. The exchange port is used only once, at
// startup, to seed the initial rolling candle window.
type PaperTrader struct {
	InstID         string
	Bar            string // candle timeframe used for strategy evaluation, e.g. "1m"
	CandleWindow   int    // how many recent candles to keep in memory for strategy evaluation
	Strategies     []strategy.Strategy
	Exchange       port.ExchangeClient // used once at startup to seed the initial candle window
	TickConsumer   port.MarketDataConsumer
	CandleConsumer port.MarketDataConsumer
	Repo           port.Repository
	NotionalUSD    decimal.Decimal
	MaxOpenOrders  int
	Logger         *slog.Logger

	candles []domain.Candle // append-only working window, touched only by the candle consumer
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

// Run seeds the initial candle window via the exchange port, then consumes ticks/candles from
// the event bus until ctx is cancelled.
func (e *PaperTrader) Run(ctx context.Context) error {
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

func (e *PaperTrader) seedCandles() error {
	candles, err := e.Exchange.GetCandles(e.InstID, e.Bar, e.CandleWindow)
	if err != nil {
		return err
	}
	// Exchange returns newest-first; strategies expect oldest-first.
	out := make([]domain.Candle, len(candles))
	for i, c := range candles {
		out[len(candles)-1-i] = c
	}
	e.candles = out
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

func (e *PaperTrader) handleCandle(ctx context.Context, data []byte, logger *slog.Logger) error {
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
	c, err := parseCandleFields(event.Candle[1], event.Candle[2], event.Candle[3], event.Candle[4], event.Candle[5])
	if err != nil {
		return fmt.Errorf("parse candle: %w", err)
	}

	e.candles = append(e.candles, c)
	if len(e.candles) > e.CandleWindow {
		e.candles = e.candles[len(e.candles)-e.CandleWindow:]
	}

	if confirm != "1" {
		return nil // still forming; wait for the finalized bar before persisting/evaluating
	}
	ms, _ := strconv.ParseInt(event.Candle[0], 10, 64)
	if err := e.Repo.SaveCandle(ctx, port.Candle{InstID: e.InstID, Bar: e.Bar, Ts: time.UnixMilli(ms).UTC(), Candle: c}); err != nil {
		logger.Warn("failed to persist candle", "instId", e.InstID, "error", err)
	}
	return e.evaluateStrategies(ctx, c.Close, logger)
}

func (e *PaperTrader) evaluateStrategies(ctx context.Context, price decimal.Decimal, logger *slog.Logger) error {
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

func buildPaperOrder(instID string, price decimal.Decimal, signal strategy.Signal, notionalUSD decimal.Decimal) port.PaperOrder {
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

	return port.PaperOrder{
		InstID:   instID,
		Side:     string(signal.Side),
		EntryPx:  price,
		SLPx:     slPx,
		TPPx:     tpPx,
		Size:     notionalUSD,
		Leverage: decimal.NewFromInt(1),
	}
}

func parseCandleFields(open, high, low, close, vol string) (domain.Candle, error) {
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
	return domain.Candle{Open: o, High: h, Low: l, Close: c, Volume: v}, nil
}
