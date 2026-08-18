// Package paperengine implements the Paper Trading Engine (CLAUDE.md §8): it evaluates
// strategies against live prices, opens virtual orders with full SL/TP features, and monitors
// them against the real price feed until SL or TP is hit. Closed trades are persisted and become
// the RL training data — no real orders are ever sent to OKX from this package.
package paperengine

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/rez/okxBot/go-engine/internal/okx"
	"github.com/rez/okxBot/go-engine/internal/okx/rest"
	"github.com/rez/okxBot/go-engine/internal/port"
	"github.com/rez/okxBot/go-engine/internal/strategy"
)

// Engine runs the paper-trading loop for a single instrument.
type Engine struct {
	InstID        string
	Bar           string // candle timeframe used for strategy evaluation, e.g. "1m"
	CandleLimit   int
	Strategies    []strategy.Strategy
	RESTClient    *rest.Client
	Repo          port.Repository
	NotionalUSD   float64
	MaxOpenOrders int
	PollInterval  time.Duration
	Logger        *slog.Logger
}

// Run polls candles/price and drives the paper-trading loop until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ticker := time.NewTicker(e.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := e.step(ctx, logger); err != nil {
				logger.Error("paper-trading step failed", "instId", e.InstID, "error", err)
			}
		}
	}
}

func (e *Engine) step(ctx context.Context, logger *slog.Logger) error {
	rawCandles, err := e.RESTClient.GetCandles(e.InstID, e.Bar, e.CandleLimit)
	if err != nil {
		return fmt.Errorf("fetch candles: %w", err)
	}
	candles, err := toStrategyCandles(rawCandles)
	if err != nil {
		return fmt.Errorf("parse candles: %w", err)
	}
	if len(candles) == 0 {
		return nil
	}

	// Persist finalized bars so the engine also serves as the candle -> Postgres writer.
	for i, raw := range rawCandles {
		if raw.Confirm != "1" {
			continue
		}
		if err := e.Repo.SaveCandle(ctx, toPortCandle(e.InstID, e.Bar, raw)); err != nil {
			logger.Warn("failed to persist candle", "instId", e.InstID, "index", i, "error", err)
		}
	}

	currentPrice := candles[len(candles)-1].Close
	if err := e.monitorOpenOrders(ctx, currentPrice, logger); err != nil {
		logger.Error("monitor open paper orders failed", "instId", e.InstID, "error", err)
	}

	return e.evaluateStrategies(ctx, candles, currentPrice, logger)
}

func (e *Engine) evaluateStrategies(ctx context.Context, candles []strategy.Candle, price float64, logger *slog.Logger) error {
	open, err := e.Repo.ListOpenPaperOrders(ctx, e.InstID)
	if err != nil {
		return fmt.Errorf("list open paper orders: %w", err)
	}
	if e.MaxOpenOrders > 0 && len(open) >= e.MaxOpenOrders {
		return nil
	}

	for _, s := range e.Strategies {
		signal, err := s.Evaluate(candles)
		if err != nil {
			logger.Warn("strategy evaluation failed", "strategy", s.Name(), "instId", e.InstID, "error", err)
			continue
		}
		if signal.Side == strategy.Hold {
			continue
		}

		order := buildPaperOrder(e.InstID, price, signal, e.NotionalUSD)
		id, err := e.Repo.OpenPaperOrder(ctx, order)
		if err != nil {
			logger.Error("failed to open paper order", "strategy", s.Name(), "instId", e.InstID, "error", err)
			continue
		}
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
