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
// merged). Best-effort: any error here is logged and skipped, never propagated, since this must
// not block the strategy-evaluation/candle-persistence path that already succeeded this bar.
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

// buildObservation assembles the CLAUDE.md §15.3 v3 observation for this token, shared across all
// of this token's open orders for one candle-close evaluation (position/PnL/dist-to-SL-TP fields
// are then overwritten per-order by the caller, since those are order-specific).
func (e *PaperTrader) buildObservation(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) domain.Observation {
	e.candlesMu.Lock()
	window := append([]domain.Candle(nil), e.candles[bar]...)
	e.candlesMu.Unlock()

	tb := domain.TimeframeBlock{Bar: bar, PriceContext: buildPriceContext(window)}
	for _, a := range e.Strategies {
		if a.Bar != bar {
			continue
		}
		sig, err := a.Strategy.Evaluate(window)
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
		SchemaVersion:  domain.ObservationSchemaVersion,
		InstID:         e.InstID,
		ActiveTokens:   e.ActiveTokens,
		MidPrice:       price,
		Timeframes:     []domain.TimeframeBlock{tb},
		TokenBudgetUSD: e.TokenBudgetUSD,
		RecentTrades:   e.recentBaselineTrades(ctx, logger),
	}

	if tb2, err := e.Repo.GetTokenBudget(ctx, e.InstID, e.TokenBudgetUSD); err == nil {
		obs.TokenEquityUSD = tb2.EquityUSD
	} else {
		logger.Warn("rl observation: get token budget failed", "instId", e.InstID, "error", err)
	}

	return obs
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
