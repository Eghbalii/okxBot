package usecase

import (
	"context"
	"log/slog"
	"time"

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
		// Position-specific fields are per-order; the rest of the observation is shared across this
		// pass, so only these are overwritten per iteration.
		obs.OrderID = o.ID
		obs.PositionState = positionStateOf(o, price)

		action, err := e.Model.Predict(ctx, obs)
		if err != nil {
			logger.Warn("rl sl/tp adjust: predict failed", "instId", e.InstID, "orderId", o.ID, "error", err)
			continue
		}
		// The action head decides whether to touch this position at all (CLAUDE.md §15.11). A model
		// meaning "leave it alone" must not fork just because its continuous price outputs happen
		// to differ from the current levels, which for a continuous output is essentially always.
		if action.Action != domain.ActionUpdate {
			continue
		}

		// The model now sets LEVELS rather than proposing percentage nudges (§15.11), so convert to
		// the adjustment the ratchet expects: how far each level moves as a fraction of live price.
		slAdjust := levelAdjustPct(o.SLPx, action.SLPx, price)
		tpAdjust := levelAdjustPct(o.TPPx, action.TPPx, price)
		if slAdjust.IsZero() && tpAdjust.IsZero() {
			continue
		}

		newSL, newTP := RatchetSLTP(o, price, slAdjust, tpAdjust)
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
// has already decided to trade (CLAUDE.md §15.6/§15.11): SizePct is a fraction of current account
// equity, LeverageFrac maps onto [1x, MaxLeverage]. Returns ok=false — leaving the caller's
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

	// A `skip` is the model declining this signal outright (CLAUDE.md §15.11) — a decision it can
	// now express, where before sizing to zero was indistinguishable from "no opinion".
	if action.Action == domain.ActionSkip {
		logger.Info("rl sizing: model declined the signal", "instId", e.InstID, "side", signal.Side)
		return decimal.Zero, decimal.Zero, false
	}

	exposure := clampUnit(action.SizePct.Abs())
	if !exposure.IsPositive() {
		// No exposure requested but no explicit skip either; keep the fixed sizing rather than
		// opening a meaningless zero-size order.
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
		// Strategies may express SL/TP as levels or as percentages; resolve to levels here so the
		// observation always carries prices (CLAUDE.md §15.11).
		resolved := sig.ResolveLevels(price)
		tb.StrategySignals = append(tb.StrategySignals, domain.StrategySignal{
			StrategyID: a.StrategyID,
			Side:       string(sig.Side),
			Confidence: sig.Confidence,
			EntryPx:    resolved.EntryPx,
			SLPx:       resolved.SLPx,
			TPPx:       resolved.TPPx,
			Kind:       a.Kind,
			Bar:        a.Bar,
		})
	}

	obs := domain.Observation{
		SchemaVersion:     domain.ObservationSchemaVersion,
		InstID:            e.InstID,
		ActiveTokens:      e.ActiveTokens,
		LastPrice:         price,
		Timeframes:        []domain.TimeframeBlock{tb},
		AccountInitialUSD: e.AccountInitialUSD,
		// The lifecycle category and per-call Signal are set by the caller, which knows which
		// decision it is asking for (CLAUDE.md §15.11). CategoryUpdate is the safe default: it is
		// what a price-driven call is, and it never claims a strategy spoke when none did.
		Category: domain.CategoryUpdate,
	}

	if acct, err := e.Repo.GetAccountEquity(ctx, e.accountMode(), e.AccountInitialUSD); err == nil {
		obs.AccountEquityUSD = acct.EquityUSD
	} else {
		logger.Warn("rl observation: get account equity failed", "mode", e.accountMode(), "error", err)
	}
	obs.OpenExposureUSD = e.openExposure(ctx, logger)

	return obs
}

// levelAdjustPct expresses "move this level from current to proposed" as a fraction of the live
// price, which is the form RatchetSLTP takes. The model emits levels (CLAUDE.md §15.11) while the
// ratchet reasons in relative moves, so this is the seam between the two.
//
// Returns zero when either level is missing or the price is unusable, so a nil SL/TP simply means
// "nothing proposed" rather than a spurious move away from zero.
func levelAdjustPct(current *decimal.Decimal, proposed, price decimal.Decimal) decimal.Decimal {
	if current == nil || !proposed.IsPositive() || !price.IsPositive() {
		return decimal.Zero
	}
	return proposed.Sub(*current).Div(price)
}

// positionStateOf describes an open order for the observation (CLAUDE.md §15.10). Prices are sent
// as-is; rl_service converts them to fractions of the live price at vectorization time, so one
// shared policy generalizes across instruments at wildly different price scales.
func positionStateOf(o port.PaperOrder, price decimal.Decimal) domain.PositionState {
	side := decimal.NewFromInt(1)
	if o.Side == "sell" {
		side = decimal.NewFromInt(-1)
	}
	ps := domain.PositionState{
		PositionOpen:     true,
		Side:             side,
		SizeUSD:          o.Size,
		Leverage:         o.Leverage,
		IsFork:           o.Variant == "rl_adjusted",
		UnrealizedPnLPct: unrealizedPnLPct(o, price),
		PnLMaxPct:        o.PnLMaxPct,
		PnLMinPct:        o.PnLMinPct,
		DistToSLPct:      distPct(o.SLPx, price),
		DistToTPPct:      distPct(o.TPPx, price),
	}
	if !o.OpenedAt.IsZero() {
		ps.AgeSeconds = int64(time.Since(o.OpenedAt).Seconds())
	}
	return ps
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

func buildPriceContext(window []domain.Candle) domain.PriceContext {
	pc := domain.PriceContext{}
	if len(window) == 0 {
		return pc
	}

	// The last entry is the LIVE FORMING candle (handleCandle replaces rather than appends while a
	// bar is open), so its OHLC is current rather than up to a full bar stale — CLAUDE.md §15.11.
	live := window[len(window)-1]
	pc.Open, pc.High, pc.Low, pc.Close = live.Open, live.High, live.Low, live.Close

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
