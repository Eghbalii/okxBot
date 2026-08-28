package usecase

import (
	"context"
	"log/slog"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// swingWindow is how many recent candles back the distance-to-swing-high/low features look
// (CLAUDE.md §15.3) — short enough to reflect the current local structure, not the whole window.
const swingWindow = 20

// priceContextWindow is how many recent bar-over-bar returns are sent as PriceContext.ClosePctChanges.
const priceContextWindow = 10

// recentTradesWindow bounds how many of this token's most-recent closed baseline trades are sent
// as the observation's recent-performance tail (CLAUDE.md §15.3/§15.7).
const recentTradesWindow = 10

// adjustOpenOrdersWithRL is the CLAUDE.md §15.4 in-trade SL/TP adjustment pass: for every open
// baseline order on this token, ask the RL model for an action and, if it proposes a nonzero
// SL/TP adjustment, fork a linked copy carrying the ratcheted result rather than editing the
// original in place (CLAUDE.md §15.4's shadow-fork mechanic — the two are compared later, not
// merged). Best-effort: any error here is logged and skipped, never propagated. Called from
// handleTick, throttled to RLAdjustInterval (CLAUDE.md §15.9's freshness fix) — price is always
// the live tick price, not a candle close.
func (e *PaperTrader) adjustOpenOrdersWithRL(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) {
	open, err := e.Repo.ListOpenPaperOrders(ctx, e.InstID)
	if err != nil {
		logger.Warn("rl sl/tp adjust: list open orders failed", "instId", e.InstID, "error", err)
		return
	}

	var baseline []port.PaperOrder
	for _, o := range open {
		if o.Variant == "baseline" || o.Variant == "" {
			baseline = append(baseline, o)
		}
	}
	if len(baseline) == 0 {
		return
	}

	obs := e.buildObservation(ctx, bar, price, logger)

	for _, o := range baseline {
		obs.Position = decimal.NewFromInt(1)
		if o.Side == "sell" {
			obs.Position = decimal.NewFromInt(-1)
		}
		obs.UnrealizedPnLPct = unrealizedPnLPct(o, price)
		obs.DistToSLPct = distPct(o.SLPx, price)
		obs.DistToTPPct = distPct(o.TPPx, price)

		action, err := e.Model.Predict(ctx, obs)
		if err != nil {
			logger.Warn("rl sl/tp adjust: predict failed", "instId", e.InstID, "orderId", o.ID, "error", err)
			continue
		}
		if action.SLAdjustPct.IsZero() && action.TPAdjustPct.IsZero() {
			continue
		}

		newSL, newTP := RatchetSLTP(o, price, action.SLAdjustPct, action.TPAdjustPct)
		if samePriceOrNil(newSL, o.SLPx) && samePriceOrNil(newTP, o.TPPx) {
			continue // ratchet rejected the proposal entirely; nothing to fork
		}

		forkID, err := e.Repo.ForkPaperOrderWithSLTP(ctx, o.ID, newSL, newTP)
		if err != nil {
			logger.Warn("rl sl/tp adjust: fork failed", "instId", e.InstID, "orderId", o.ID, "error", err)
			continue
		}
		logger.Info("rl sl/tp adjustment forked", "instId", e.InstID, "parentId", o.ID, "forkId", forkID,
			"newSL", newSL, "newTP", newTP)
	}
}

// rlSizing asks the RL agent how much of the SHARED account to put behind a strategy signal that
// has already decided to trade (CLAUDE.md §15.4/§15.6): TargetExposure is a fraction of current
// account equity, LeverageFrac maps onto [1x, MaxLeverage]. Returns ok=false — leaving the caller's
// fixed sizing untouched — when RL sizing is disabled, unconfigured, the model errors, or the model
// asks for effectively no exposure.
//
// Two hard Go-side caps bound the result regardless of what the model proposes, the same
// "never trust the model as the safety boundary" pattern as §15.4's SL/TP ratchet and §5's risk
// manager:
//
//   - MaxPositionPct — no single position may exceed this fraction of equity, so one confident
//     early decision can't put most of the account into one trade.
//   - MaxTotalExposurePct — the sum of all currently-open positions' notionals is capped too,
//     since the per-position cap alone still permits N simultaneous positions at the cap.
//
// Deliberately magnitude-only: the strategy layer owns direction (CLAUDE.md §9/§16.1 — strategies
// decide *when* there's a tradeable signal, the agent decides *how much*), so a model that
// disagrees with the signal's side expresses that by sizing down toward zero, never by flipping
// the order's side out from under the strategy that generated it.
func (e *PaperTrader) rlSizing(
	ctx context.Context,
	obs domain.Observation,
	signal strategy.Signal,
	openOrders []port.PaperOrder,
	logger *slog.Logger,
) (notional, leverage decimal.Decimal, ok bool) {
	if !e.RLSizing || e.Model == nil || !e.MaxLeverage.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}

	equity := obs.AccountEquityUSD
	if !equity.IsPositive() {
		// A drained (or unreadable) account has nothing to size against. Falling back to the fixed
		// notional here would quietly ignore the drain, so decline instead and let the caller's
		// own budget check decide whether to trade at all.
		logger.Warn("rl sizing: no positive account equity to size against, falling back",
			"instId", e.InstID, "equity", equity)
		return decimal.Zero, decimal.Zero, false
	}

	action, err := e.Model.Predict(ctx, obs)
	if err != nil {
		logger.Warn("rl sizing: predict failed, falling back to fixed sizing",
			"instId", e.InstID, "error", err)
		return decimal.Zero, decimal.Zero, false
	}

	exposure := clampUnit(action.TargetExposure.Abs())
	if !exposure.IsPositive() {
		// The agent wants no exposure behind this signal. Sizing to zero would open a meaningless
		// order, so keep the fixed sizing and let the trade stand as the strategy proposed it —
		// declining to trade entirely is not this pass's decision to make (CLAUDE.md §9).
		return decimal.Zero, decimal.Zero, false
	}

	notional = equity.Mul(exposure)

	if e.MaxPositionPct.IsPositive() {
		if cap := equity.Mul(e.MaxPositionPct); notional.GreaterThan(cap) {
			logger.Info("rl sizing: position capped", "instId", e.InstID,
				"requested", notional, "cap", cap, "maxPositionPct", e.MaxPositionPct)
			notional = cap
		}
	}

	if e.MaxTotalExposurePct.IsPositive() {
		var openNotional decimal.Decimal
		for _, o := range openOrders {
			// Forks are tracking-only shadows of their baseline parent (CLAUDE.md §15.4), not
			// separate capital at risk — counting them would double-charge one signal's exposure.
			if o.Variant == "baseline" || o.Variant == "" {
				openNotional = openNotional.Add(o.Size)
			}
		}
		headroom := equity.Mul(e.MaxTotalExposurePct).Sub(openNotional)
		if headroom.LessThanOrEqual(decimal.Zero) {
			logger.Info("rl sizing: total exposure ceiling reached, falling back",
				"instId", e.InstID, "openNotional", openNotional,
				"maxTotalExposurePct", e.MaxTotalExposurePct)
			return decimal.Zero, decimal.Zero, false
		}
		if notional.GreaterThan(headroom) {
			logger.Info("rl sizing: position trimmed to remaining exposure headroom",
				"instId", e.InstID, "requested", notional, "headroom", headroom)
			notional = headroom
		}
	}

	if !notional.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}

	leverage = decimal.NewFromInt(1).Add(clampUnit(action.LeverageFrac).Mul(e.MaxLeverage.Sub(decimal.NewFromInt(1))))

	logger.Debug("rl sizing applied", "instId", e.InstID, "side", signal.Side,
		"equity", equity, "exposure", exposure, "notional", notional, "leverage", leverage)
	return notional, leverage, true
}

// clampUnit clamps d into [0, 1] — the range every fractional model output is defined over.
func clampUnit(d decimal.Decimal) decimal.Decimal {
	one := decimal.NewFromInt(1)
	switch {
	case d.IsNegative():
		return decimal.Zero
	case d.GreaterThan(one):
		return one
	default:
		return d
	}
}

// buildObservation assembles the CLAUDE.md §15.3 v3 observation for this token, shared across all
// of this token's open orders for one evaluation pass (position/PnL/dist-to-SL-TP fields are then
// overwritten per-order by the caller, since those are order-specific). The candle window/strategy
// signals/price-context (bar-scoped) reflect the most recent finalized candle as before; LastPrice
// and the per-order PnL/distance fields reflect whatever price the caller passes in — the live tick
// price when called from handleTick (CLAUDE.md §15.9), a candle close when called from
// evaluateStrategies' new-order path (still candle-driven by design, §9).
func (e *PaperTrader) buildObservation(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) domain.Observation {
	view := e.marketView(bar)
	window := view.Candles

	tb := domain.TimeframeBlock{Bar: bar, PriceContext: buildPriceContext(window)}
	for _, a := range e.Strategies {
		if a.Bar != bar {
			continue
		}
		sig, err := strategy.EvaluateWith(a.Strategy, view)
		if err != nil {
			continue // best-effort: a failing strategy just doesn't contribute a signal this round
		}
		tb.StrategySignals = append(tb.StrategySignals, domain.StrategySignal{
			StrategyID: a.StrategyID,
			Side:       string(sig.Side),
			Confidence: sig.Confidence,
			SLPct:      sig.SLPct,
			TPPct:      sig.TPPct,
		})
	}

	obs := domain.Observation{
		SchemaVersion:     domain.ObservationSchemaVersion,
		InstID:            e.InstID,
		ActiveTokens:      e.ActiveTokens,
		LastPrice:         price,
		Timeframes:        []domain.TimeframeBlock{tb},
		AccountInitialUSD: e.AccountInitialUSD,
		RecentTrades:      e.recentBaselineTrades(ctx, logger),
	}

	if acct, err := e.Repo.GetAccountEquity(ctx, e.accountMode(), e.AccountInitialUSD); err == nil {
		obs.AccountEquityUSD = acct.EquityUSD
	} else {
		logger.Warn("rl observation: get account equity failed", "mode", e.accountMode(), "error", err)
	}
	obs.OpenExposureUSD = e.openExposure(ctx, logger)

	return obs
}

// openExposure sums the notional of every open baseline position across ALL tokens (CLAUDE.md
// §15.6): with one shared account, the agent has to see how much of it is already committed
// elsewhere before asking for more. Forks are excluded — they shadow their baseline parent rather
// than putting separate capital at risk (§15.4). Best-effort: a read failure reports zero exposure
// rather than blocking the decision, and the Go-side caps in rlSizing still bound the result.
func (e *PaperTrader) openExposure(ctx context.Context, logger *slog.Logger) decimal.Decimal {
	openOnly := true
	positions, err := e.Repo.ListPositions(ctx, port.PositionFilter{Mode: e.accountMode(), Open: &openOnly})
	if err != nil {
		logger.Warn("rl observation: list open positions failed", "mode", e.accountMode(), "error", err)
		return decimal.Zero
	}

	var total decimal.Decimal
	for _, p := range positions {
		if p.Variant == "baseline" || p.Variant == "" {
			total = total.Add(p.Size)
		}
	}
	return total
}

// recentBaselineTrades fetches the most-recent closed baseline trades for this token, oldest-last
// input reversed to most-recent-last per RecentTrade's documented order (CLAUDE.md §15.3).
func (e *PaperTrader) recentBaselineTrades(ctx context.Context, logger *slog.Logger) []domain.RecentTrade {
	closedFlag := false
	positions, err := e.Repo.ListPositions(ctx, port.PositionFilter{
		Mode: "paper", InstID: e.InstID, Open: &closedFlag, SortBy: "closed_at", SortDesc: true,
	})
	if err != nil {
		logger.Warn("rl observation: list recent trades failed", "instId", e.InstID, "error", err)
		return nil
	}

	var out []domain.RecentTrade
	for _, p := range positions {
		if p.Variant != "baseline" && p.Variant != "" {
			continue
		}
		if p.RealizedPnL == nil || p.CloseReason == nil {
			continue
		}
		out = append(out, domain.RecentTrade{RealizedPnLUSD: *p.RealizedPnL, Win: *p.CloseReason == "tp"})
		if len(out) >= recentTradesWindow {
			break
		}
	}
	// positions came back most-recent-first (SortDesc); RecentTrade wants most-recent-last.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func buildPriceContext(window []domain.Candle) domain.PriceContext {
	pc := domain.PriceContext{}
	if len(window) < 2 {
		return pc
	}

	start := 0
	if len(window) > priceContextWindow+1 {
		start = len(window) - priceContextWindow - 1
	}
	for i := start + 1; i < len(window); i++ {
		prev := window[i-1].Close
		if prev.IsZero() {
			continue
		}
		pc.ClosePctChanges = append(pc.ClosePctChanges, window[i].Close.Sub(prev).Div(prev))
	}

	swingHigh, errH := strategy.Highest(window, minInt(swingWindow, len(window)))
	swingLow, errL := strategy.Lowest(window, minInt(swingWindow, len(window)))
	last := window[len(window)-1].Close
	if errH == nil && last.IsPositive() {
		pc.DistToSwingHighPct = swingHigh.Sub(last).Div(last)
	}
	if errL == nil && last.IsPositive() {
		pc.DistToSwingLowPct = swingLow.Sub(last).Div(last)
	}
	return pc
}

func unrealizedPnLPct(o port.PaperOrder, price decimal.Decimal) decimal.Decimal {
	if !o.EntryPx.IsPositive() {
		return decimal.Zero
	}
	direction := decimal.NewFromInt(1)
	if o.Side == "sell" {
		direction = decimal.NewFromInt(-1)
	}
	return direction.Mul(price.Sub(o.EntryPx)).Div(o.EntryPx)
}

func distPct(px *decimal.Decimal, price decimal.Decimal) decimal.Decimal {
	if px == nil || !price.IsPositive() {
		return decimal.Zero
	}
	return px.Sub(price).Div(price)
}

func samePriceOrNil(a, b *decimal.Decimal) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
