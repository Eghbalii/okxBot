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
	Model        port.ModelClient
	ActiveTokens []string // the roster used to build the token-identity one-hot, CLAUDE.md §15.3

	// Mode is which account balance this engine trades against — "paper" here; cmd/trader uses
	// "demo"/"real". All three are tracked simultaneously (CLAUDE.md §15.6), and only non-real
	// modes ever auto-reset a drained balance (§15.7).
	Mode string
	// AccountInitialUSD is the shared account's configured starting balance — what a drained
	// paper/demo account resets back to. Every token trades against this one pool rather than a
	// per-token slice of it (CLAUDE.md §15.6's 2026-08-28 revision).
	AccountInitialUSD decimal.Decimal
	// MaxPositionPct / MaxTotalExposurePct bound how much of account equity the RL agent may put
	// into one position, and into all open positions combined. Hard Go-side caps on the model's
	// proposal — the same "never trust the model as the safety boundary" pattern as §15.4's SL/TP
	// ratchet. Zero disables the respective cap.
	MaxPositionPct      decimal.Decimal
	MaxTotalExposurePct decimal.Decimal

	// RLSizing lets the RL agent set each new order's notional and leverage from its
	// TargetExposure/LeverageFrac action (CLAUDE.md §15.4), instead of every order being opened at
	// the fixed NotionalUSD and 1x. Off by default and independent of the SL/TP-adjust flag: until
	// it's on, the paper_orders log carries no leverage/exposure variance at all, which is exactly
	// the training signal §15.8's continued-live-learning phase needs to learn sizing from.
	// Requires Model and MaxLeverage to be set; falls back to the fixed sizing on any model error.
	RLSizing bool
	// RLSLTPAdjust enables the in-trade SL/TP adjustment pass (CLAUDE.md §15.4). Kept separate from
	// RLSizing so either can run without the other — they're independent decisions that happen to
	// share one model client.
	RLSLTPAdjust bool
	// RLDecisionBar selects which timeframe's strategy signals/price context feed a tick-driven RL
	// SL/TP-adjust decision (CLAUDE.md §15.3). Empty means "the shortest configured bar" — see
	// decisionBar. Must be one of Bars.
	RLDecisionBar string
	// MaxLeverage is the ceiling LeverageFrac maps onto ([1x, MaxLeverage]). This mirrors the
	// config's risk.max_leverage so paper orders can't record leverage the live risk manager would
	// reject outright (CLAUDE.md §5) — the risk manager remains the real boundary for live trading;
	// this is the paper-mode equivalent so the two produce comparable data.
	MaxLeverage decimal.Decimal

	// OrderEvents publishes a lightweight open/close notification for every paper order this
	// engine opens or closes, onto the internal event bus (CLAUDE.md §12) — consumed by cmd/api's
	// WebSocket bridge to push real-time position alerts to the panel. May be nil, in which case
	// publishing is skipped entirely (best-effort, never blocks the order open/close itself).
	OrderEvents port.MarketDataPublisher

	// candlesMu guards candles: each bar has its own consumer goroutine (see Run), so writes to
	// this map (even to distinct keys) must be synchronized — concurrent map writes are a fatal
	// Go runtime error, not just a race.
	candlesMu sync.Mutex
	candles   map[string][]domain.Candle // keyed by bar

	// rlAdjustMu/lastRLAdjustAt throttle the RL SL/TP-adjust pass to tick cadence (CLAUDE.md's
	// 2026-08-27 MidPrice-freshness audit): previously this only ran on candle close, so the model
	// could reason about a price up to one full bar interval stale while an open order's SL/TP was
	// actually at risk of being hit in real time. Now every tick is a candidate trigger, throttled
	// to RLAdjustInterval so a busy token doesn't call rl_service on every single tick.
	rlAdjustMu     sync.Mutex
	lastRLAdjustAt time.Time
}

// RLAdjustInterval is the minimum time between RL SL/TP-adjust passes for one instrument
// (CLAUDE.md §15.4/§15.9's freshness fix) — a fixed wall-clock throttle, not tied to tick count,
// so the effective call rate stays predictable regardless of how fast OKX's tickers channel is
// pushing updates. A few seconds of lag behind the live tick stream is an explicit, accepted
// tradeoff for bounding rl_service's inference load; it is not meant to track every tick.
const RLAdjustInterval = 2 * time.Second

// defaultPaperLeverage is what a paper order records when the RL sizing pass isn't active.
var defaultPaperLeverage = decimal.NewFromInt(1)

// barSeconds converts an OKX bar name ("1m", "15m", "1H", "4H", "1D", "1W") to its duration in
// seconds, for ordering timeframes shortest-to-longest. Unknown names sort last rather than
// erroring — this only picks a default decision context, and a bar nobody recognizes is a poor
// choice for it anyway.
//
// The unit is matched case-INSENSITIVELY on purpose. OKX's channel names are case-sensitive
// ("1H" and "1D" are capitalized, minutes are not), and a config written as "1h" is a real and
// easy mistake; sorting must not silently misbehave on it. Note that fixing the ordering here does
// NOT make a mis-cased bar work end-to-end — the ingestor still needs OKX's exact casing to
// subscribe at all, which is why config.ValidateBarNames rejects it at startup instead.
func barSeconds(bar string) int {
	if bar == "" {
		return 1 << 30
	}
	n := 0
	i := 0
	for ; i < len(bar) && bar[i] >= '0' && bar[i] <= '9'; i++ {
		n = n*10 + int(bar[i]-'0')
	}
	if n == 0 || i >= len(bar) {
		return 1 << 30
	}
	switch bar[i] {
	case 'm': // minutes — lowercase only; 'M' is months in OKX's scheme, handled below
		return n * 60
	case 'H', 'h':
		return n * 3600
	case 'D', 'd':
		return n * 86400
	case 'W', 'w':
		return n * 604800
	case 'M':
		return n * 2592000 // ~30d; only used for ordering, never for arithmetic on real timestamps
	default:
		return 1 << 30
	}
}

// decisionBar picks which timeframe's strategy signals and price context feed a TICK-driven RL
// decision (CLAUDE.md §15.3/§15.9). A tick doesn't belong to any one bar, so this has to be chosen
// rather than inferred.
//
// Configured explicitly via paper_trading.rl_decision_bar; otherwise the SHORTEST configured bar,
// because an in-trade SL/TP adjustment is a reaction to what price is doing right now and the
// shortest timeframe carries the freshest read of that. This used to be Bars[0] — array order,
// which silently meant "whichever bar happens to be listed first" and would quietly change meaning
// if the config list were reordered.
//
// Note this selects the DECISION context only. The observation still reports one timeframe block,
// but a multi-timeframe strategy contributing signals to it sees every bar (marketView), so
// higher-timeframe context still reaches the model through those signals.
func (e *PaperTrader) decisionBar() string {
	if e.RLDecisionBar != "" {
		return e.RLDecisionBar
	}
	shortest := ""
	for _, b := range e.Bars {
		if shortest == "" || barSeconds(b) < barSeconds(shortest) {
			shortest = b
		}
	}
	return shortest
}

// marketView snapshots every maintained timeframe under one lock, so a multi-timeframe strategy
// (CLAUDE.md §9) sees bars that are consistent with each other, and so no lock is held across a
// Strategy.Evaluate call. bar is the timeframe that just closed — the decision cadence.
func (e *PaperTrader) marketView(bar string) strategy.MarketView {
	e.candlesMu.Lock()
	bars := make(map[string][]domain.Candle, len(e.candles))
	for b, window := range e.candles {
		bars[b] = append([]domain.Candle(nil), window...)
	}
	e.candlesMu.Unlock()

	return strategy.MarketView{Bar: bar, Candles: bars[bar], Bars: bars}
}

// accountMode is the trading mode whose balance this engine moves. Defaults to "paper" so an
// engine constructed without an explicit Mode can never accidentally write to the demo or real
// account's balance (CLAUDE.md §15.7's real-mode carve-out).
func (e *PaperTrader) accountMode() string {
	if e.Mode == "" {
		return "paper"
	}
	return e.Mode
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
	if err := e.monitorOpenOrders(ctx, price, logger); err != nil {
		return err
	}

	// CLAUDE.md §15.4/§15.9: RL SL/TP-adjust now runs on the live tick stream (throttled to
	// RLAdjustInterval), not just at candle close — see the audit note on lastRLAdjustAt's
	// declaration for why this changed. Best-effort/never blocking, same as the old call site.
	if e.Model != nil && e.RLSLTPAdjust && e.shouldRunRLAdjust() {
		e.adjustOpenOrdersWithRL(ctx, e.decisionBar(), price, logger)
	}
	return nil
}

// shouldRunRLAdjust reports whether enough time has passed since the last RL SL/TP-adjust pass to
// run another one now, and if so, atomically claims the slot (updates lastRLAdjustAt) so
// concurrent ticks can't both pass the check and double-fire.
func (e *PaperTrader) shouldRunRLAdjust() bool {
	e.rlAdjustMu.Lock()
	defer e.rlAdjustMu.Unlock()
	if time.Since(e.lastRLAdjustAt) < RLAdjustInterval {
		return false
	}
	e.lastRLAdjustAt = time.Now()
	return true
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
	// RL SL/TP-adjust now runs on the tick stream instead of here (see handleTick) — CLAUDE.md
	// §15.4/§15.9's freshness fix: candle-close cadence meant the model could reason about a price
	// up to one full bar interval stale while an open order's SL/TP was actually at risk in real
	// time.
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

	// Snapshot every timeframe, not just the one that closed: a strategy assigned to 5m may consult
	// 1h/4h for trend context via MultiTimeframeStrategy (CLAUDE.md §9). Taken under one lock so all
	// bars are consistent with each other, and released before any Evaluate call.
	view := e.marketView(bar)

	for _, a := range e.Strategies {
		if a.Bar != bar {
			continue // only re-evaluate strategies assigned to the timeframe that just closed
		}
		s := a.Strategy
		signal, err := strategy.EvaluateWith(s, view)
		if err != nil {
			logger.Warn("strategy evaluation failed", "strategy", s.Name(), "instId", e.InstID, "bar", bar, "error", err)
			continue
		}
		metrics.StrategySignalsTotal.WithLabelValues(s.Name(), e.InstID, string(signal.Side)).Inc()
		if signal.Side == strategy.Hold {
			continue
		}

		order := buildPaperOrder(e.InstID, price, signal, e.NotionalUSD, a.StrategyID)

		// The same observation is used for both the RL sizing decision and the persisted
		// decision-time record below, so what's stored is exactly what the model was asked.
		obs := e.buildObservation(ctx, bar, price, logger)
		if notional, leverage, ok := e.rlSizing(ctx, obs, signal, open, logger); ok {
			order.Size, order.Leverage = notional, leverage
		}

		// CLAUDE.md §15.3/§15.8: persist the actual observation vector at decision time (not just
		// the realized outcome) so it can later feed live/continued RL training — the whole point
		// is training data that matches exactly what rlclient would have sent, not a reconstruction.
		// Best-effort: a marshal/build failure must never block opening the order itself.
		if raw, err := json.Marshal(obs); err == nil {
			order.FeaturesJSON = raw
		} else {
			logger.Warn("failed to marshal decision-time observation", "instId", e.InstID, "error", err)
		}
		id, err := e.Repo.OpenPaperOrder(ctx, order)
		if err != nil {
			logger.Error("failed to open paper order", "strategy", s.Name(), "instId", e.InstID, "error", err)
			continue
		}
		metrics.PaperOrdersOpenedTotal.WithLabelValues(s.Name(), e.InstID, string(signal.Side)).Inc()
		logger.Info("opened paper order", "id", id, "strategy", s.Name(), "instId", e.InstID, "bar", bar,
			"side", signal.Side, "entry", price, "confidence", signal.Confidence)
		e.publishOrderEvent(ctx, "opened", id, logger)
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
		e.publishOrderEvent(ctx, "closed", o.ID, logger)

		// CLAUDE.md §15.4/§15.6/§15.7: only a baseline order's outcome moves the shared account
		// balance — an rl_adjusted fork is tracking-only (its whole purpose is to be compared
		// against its baseline parent afterward, not to be treated as a second real bet).
		if o.Variant == "baseline" || o.Variant == "" {
			if e.AccountInitialUSD.IsPositive() {
				orderID := o.ID
				if acct, reset, err := e.Repo.ApplyRealizedPnL(ctx, e.accountMode(), pnl, &orderID, e.InstID); err != nil {
					logger.Error("failed to apply realized pnl to account", "instId", e.InstID, "error", err)
				} else if reset {
					// Recorded as a reason="reset" point in the equity timeline too, so a drain that
					// happens overnight is visible in the panel's chart afterward, not just here.
					logger.Warn("account balance drained, reset to initial", "mode", e.accountMode(),
						"instId", e.InstID, "initialUsd", acct.InitialUSD, "resetCount", acct.ResetCount)
				}
			}
		}
	}
	return nil
}

// PaperOrderEvent is the lightweight open/close notification published to
// "okx.paper-order-events" (CLAUDE.md §12) — just enough for cmd/api's WebSocket bridge to tell
// the panel "something changed," which then re-fetches full position detail via GET /api/positions
// rather than this event carrying the full port.PaperOrder itself.
type PaperOrderEvent struct {
	Type    string `json:"type"` // "opened" or "closed"
	OrderID int64  `json:"orderId"`
	InstID  string `json:"instId"`
}

// publishOrderEvent is best-effort: a publish failure must never block/undo the order
// open/close it's reporting on. No-op if OrderEvents wasn't configured.
func (e *PaperTrader) publishOrderEvent(ctx context.Context, eventType string, orderID int64, logger *slog.Logger) {
	if e.OrderEvents == nil {
		return
	}
	event := PaperOrderEvent{Type: eventType, OrderID: orderID, InstID: e.InstID}
	if err := e.OrderEvents.Publish(ctx, e.InstID, event); err != nil {
		logger.Warn("failed to publish paper order event", "type", eventType, "orderId", orderID, "instId", e.InstID, "error", err)
	}
}

func closeReason(o port.PaperOrder, price decimal.Decimal) (string, bool) {
	return SLTPTouchReason(o.Side, o.SLPx, o.TPPx, price)
}

// SLTPTouchReason is the shared SL/TP-touch comparison — genuine domain logic (CLAUDE.md §16.3's
// trial mechanics require the exact same touch semantics PaperTrader uses for real paper orders,
// so cmd/strategy-optimizer's trial checker calls this directly rather than duplicating it).
// side is "buy" or "sell"; slPx/tpPx may be nil (no touch check on that side). Returns ("sl" or
// "tp", true) on a touch, or ("", false) if neither has been hit yet at price.
func SLTPTouchReason(side string, slPx, tpPx *decimal.Decimal, price decimal.Decimal) (string, bool) {
	switch side {
	case "buy":
		if slPx != nil && price.LessThanOrEqual(*slPx) {
			return "sl", true
		}
		if tpPx != nil && price.GreaterThanOrEqual(*tpPx) {
			return "tp", true
		}
	case "sell":
		if slPx != nil && price.GreaterThanOrEqual(*slPx) {
			return "sl", true
		}
		if tpPx != nil && price.LessThanOrEqual(*tpPx) {
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
		// 1x is the un-sized default: the strategy layer has no view on leverage, so an order
		// opened without the RL sizing pass (PaperTrader.RLSizing) records the unlevered position
		// the signal itself implies. The caller overwrites Size/Leverage when RL sizing is on.
		Leverage: defaultPaperLeverage,
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
