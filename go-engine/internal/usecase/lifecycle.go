package usecase

import (
	"context"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// This file is PaperTrader's half of the signal lifecycle (CLAUDE.md §15.10/§15.12). The cadence
// and category rules themselves are pure and live in internal/usecase/conductor; everything here is
// the IO around them — building the right observation, calling the model, and acting on the answer.
//
// Why this exists at all: before it, buildObservation sent Category=CategoryUpdate as a hardcoded
// default and nothing ever emitted a terminal call, so rl_service/learner.py — which pairs a
// decision with the realized PnL that arrives on the close call hours later — received no rewards
// whatsoever in production. The model could be asked questions but never told how any of its
// answers turned out.

// openDecision asks the model whether to take a strategy signal that just fired while this token
// has no open position, and how to shape it if so (CLAUDE.md §15.12). Returns the sizing and levels
// to apply, or ok=false when the signal should not be traded.
//
// The category is buy or sell from the SIGNAL's side, never the model's: direction belongs to the
// strategy layer (§9/§16.1). The model expresses disagreement by answering `skip`, not by flipping
// the side out from under the strategy that produced it.
func (e *PaperTrader) openDecision(
	ctx context.Context,
	obs domain.Observation,
	sig strategy.Signal,
	openOrders []port.PaperOrder,
	price decimal.Decimal,
	logger *slog.Logger,
) (decision openDecisionResult, ok bool) {
	if e.Model == nil || !e.RLSizing {
		return openDecisionResult{}, false
	}

	category := conductor.OpenCategory(string(sig.Side))
	if category == "" {
		return openDecisionResult{}, false
	}
	obs.Category = category

	action, err := e.Model.Predict(ctx, obs)
	if err != nil {
		metrics.ModelOpenDecisionsTotal.WithLabelValues(e.InstID, "error").Inc()
		logger.Warn("lifecycle: open predict failed, falling back to fixed sizing",
			"instId", e.InstID, "category", category, "error", err)
		return openDecisionResult{}, false
	}

	if action.Action == domain.ActionSkip {
		metrics.ModelOpenDecisionsTotal.WithLabelValues(e.InstID, "skip").Inc()
		logger.Info("lifecycle: model declined the signal",
			"instId", e.InstID, "category", category, "side", sig.Side)
		return openDecisionResult{Skip: true}, true
	}

	// An untrained policy routinely answers a buy/sell call with an update-category action
	// ("none"), which is not a decision about opening at all. Treating that as an implicit "open"
	// and then falling through to sizing hid the real state: the model WAS being consulted on every
	// signal, always answered unusably, and the fallback to fixed sizing left no trace anywhere —
	// no log line, no metric — so the whole path looked like it had never run.
	if action.Action != domain.ActionOpen {
		metrics.ModelOpenDecisionsTotal.WithLabelValues(e.InstID, "unusable").Inc()
		logger.Info("lifecycle: model gave no open/skip answer, falling back to fixed sizing",
			"instId", e.InstID, "category", category, "action", action.Action)
		return openDecisionResult{}, false
	}

	notional, leverage, sized := e.sizeFromAction(action, obs, openOrders, logger)
	if !sized {
		// sizeFromAction declines silently on a zero/unusable size_pct, which an untrained policy
		// emits constantly. Counted and logged here rather than there so every reason an open
		// decision did not reach the order is visible from one metric.
		metrics.ModelOpenDecisionsTotal.WithLabelValues(e.InstID, "unsized").Inc()
		logger.Info("lifecycle: model action not sizable, falling back to fixed sizing",
			"instId", e.InstID, "category", category, "sizePct", action.SizePct)
		return openDecisionResult{}, false
	}
	metrics.ModelOpenDecisionsTotal.WithLabelValues(e.InstID, "open").Inc()

	// Clamp where the model wants its levels BEFORE they are written to the order. The ratchet
	// governs how they may move afterward and says nothing about the initial placement — an absurd
	// first stop would already have done its damage by the time any update runs.
	levels := e.conductorClamps().Apply(string(sig.Side), price, conductor.Levels{
		SLPx: nonZeroPx(action.SLPx),
		TPPx: nonZeroPx(action.TPPx),
	})

	return openDecisionResult{
		Notional: notional,
		Leverage: leverage,
		SLPx:     levels.SLPx,
		TPPx:     levels.TPPx,
	}, true
}

// openDecisionResult is what the model decided about opening one signal. Skip and the sizing fields
// are mutually exclusive: a skip carries nothing to apply.
type openDecisionResult struct {
	Skip     bool
	Notional decimal.Decimal
	Leverage decimal.Decimal
	// SLPx/TPPx are the model's clamped levels, nil when it set none — in which case the caller
	// keeps whatever the strategy proposed rather than opening a position with no protection.
	SLPx *decimal.Decimal
	TPPx *decimal.Decimal
}

// runUpdates is the in-trade half of the lifecycle: for every open order due an update, ask the
// model what to do with it (CLAUDE.md §15.12). Called from handleTick on the live tick price, so
// the decision reasons about what price is doing right now rather than a candle close up to a full
// bar stale (§15.9's freshness audit).
//
// Best-effort throughout: an error on one order is logged and skipped, never propagated — a model
// or database problem must not stop the engine from monitoring SL/TP on the others.
func (e *PaperTrader) runUpdates(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) {
	open, err := e.Repo.ListOpenPaperOrders(ctx, e.InstID)
	if err != nil {
		logger.Warn("lifecycle: list open orders failed", "instId", e.InstID, "error", err)
		return
	}
	if len(open) == 0 {
		return
	}

	obs := e.buildObservation(ctx, bar, price, logger)
	obs.Category = domain.CategoryUpdate
	now := time.Now()

	for _, o := range open {
		// Forks are open positions and do generate their own updates (CLAUDE.md §15.12), but a fork
		// is never itself forked again — otherwise each adjustment spawns a new branch and the tree
		// grows without bound, with every leaf drawing model calls.
		pnl := unrealizedPnLPct(o, price)
		if !e.conductor().ShouldUpdate(o.ID, pnl, now) {
			continue
		}
		metrics.ControllerUpdatesTotal.WithLabelValues(e.InstID).Inc()

		obs.OrderID = o.ID
		obs.PositionState = positionStateOf(o, price)
		// Carry the last signal for this order's timeframe forward (CLAUDE.md §15.12): a 1H opinion
		// stays meaningful for the whole hour, and dropping it the moment its candle closed would
		// hide it from every update in between. Nil when no strategy has spoken — `present=false`
		// is what tells the model this was price-driven, and inventing a signal there would be a
		// lie it learns from.
		obs.Signal = e.carriedSignalFor(bar)

		action, err := e.Model.Predict(ctx, obs)
		if err != nil {
			logger.Warn("lifecycle: update predict failed", "instId", e.InstID, "orderId", o.ID, "error", err)
			continue
		}

		switch action.Action {
		case domain.ActionClose:
			metrics.ModelUpdateDecisionsTotal.WithLabelValues(e.InstID, "close").Inc()
			e.closeEarly(ctx, o, price, logger)
		case domain.ActionUpdate:
			metrics.ModelUpdateDecisionsTotal.WithLabelValues(e.InstID, "update").Inc()
			e.applyAdjustment(ctx, o, action, price, logger)
		default:
			// ActionNone, or anything the model emitted that has no meaning for an update — leave
			// the position alone. Treating an unrecognized action as "do nothing" is the safe
			// default; acting on one would be acting on a decision nobody defined.
			metrics.ModelUpdateDecisionsTotal.WithLabelValues(e.InstID, "none").Inc()
		}
	}
}

// applyAdjustment turns the model's proposed SL/TP levels into a shadow fork (CLAUDE.md §15.4):
// the original order is never edited, so what the un-adjusted trade would have done stays
// observable. Both run to completion and are compared afterward.
func (e *PaperTrader) applyAdjustment(ctx context.Context, o port.PaperOrder, action *domain.Action, price decimal.Decimal, logger *slog.Logger) {
	if o.Variant == "rl_adjusted" {
		return // no fork-of-a-fork; see runUpdates
	}

	// The model sets levels (§15.11) while the ratchet reasons in relative moves, so convert here.
	slAdjust := levelAdjustPct(o.SLPx, action.SLPx, price)
	tpAdjust := levelAdjustPct(o.TPPx, action.TPPx, price)
	if slAdjust.IsZero() && tpAdjust.IsZero() {
		return
	}

	newSL, newTP := RatchetSLTP(o, price, slAdjust, tpAdjust)
	if samePriceOrNil(newSL, o.SLPx) && samePriceOrNil(newTP, o.TPPx) {
		return // the ratchet rejected the proposal entirely; nothing to fork
	}

	forkID, err := e.Repo.ForkPaperOrderWithSLTP(ctx, o.ID, newSL, newTP)
	if err != nil {
		logger.Warn("lifecycle: fork failed", "instId", e.InstID, "orderId", o.ID, "error", err)
		return
	}
	logger.Info("lifecycle: sl/tp adjustment forked", "instId", e.InstID,
		"parentId", o.ID, "forkId", forkID, "newSL", newSL, "newTP", newTP)
}

// closeEarly closes a position at the live price because the model asked to (CLAUDE.md §15.12).
// Gated on RLEarlyClose: this is the one lifecycle action that destroys the counterfactual — an
// early-closed trade can never show what it would have done — so it stays opt-in.
func (e *PaperTrader) closeEarly(ctx context.Context, o port.PaperOrder, price decimal.Decimal, logger *slog.Logger) {
	if !e.RLEarlyClose {
		return
	}
	pnl := realizedPnL(o, price)
	if err := e.closeOrder(ctx, o, price, conductor.CloseReasonRLEarly, pnl, logger); err != nil {
		logger.Error("lifecycle: early close failed", "instId", e.InstID, "orderId", o.ID, "error", err)
	}
}

// reportTerminal delivers a closed trade's realized outcome to the model (CLAUDE.md §15.10 — the
// close event IS the reward). rl_service/learner.py holds each earlier decision as pending, keyed
// by order id, and pairs it with the PnL that arrives here; without this call nothing the model
// decided is ever scored.
//
// The response is discarded by design: this call informs rather than asks. Entry/SL/TP are
// deliberately NOT zeroed — the outcome has to stay attached to the decision that produced it, or
// the model cannot learn which stop placement caused which result.
//
// Best-effort: a failure must never block closing the order, but it does mean that trade never
// trains anything, which is why it logs at warn rather than debug.
func (e *PaperTrader) reportTerminal(ctx context.Context, o port.PaperOrder, closePx, pnl decimal.Decimal, closeReason string, logger *slog.Logger) {
	if e.Model == nil {
		return
	}
	category := conductor.TerminalCategory(closeReason)
	if category == "" {
		return // e.g. a manual close: not a decision the model made, so not something to train on
	}

	obs := e.buildObservation(ctx, e.decisionBar(), closePx, logger)
	obs.Category = category
	obs.OrderID = o.ID
	obs.Signal = e.carriedSignalFor(e.decisionBar())

	ps := positionStateOf(o, closePx)
	ps.RealizedPnLUSD = pnl
	obs.PositionState = ps

	if _, err := e.Model.Predict(ctx, obs); err != nil {
		logger.Warn("lifecycle: terminal report failed; this trade will not train the model",
			"instId", e.InstID, "orderId", o.ID, "category", category, "error", err)
	}
}

// carriedSignalFor returns the retained signal for a bar, if the conductor has one.
func (e *PaperTrader) carriedSignalFor(bar string) *domain.StrategySignal {
	sig, ok := e.conductor().CarriedSignal(e.InstID, bar)
	if !ok {
		return nil
	}
	return &sig
}

// conductor lazily builds the lifecycle state machine from PaperTrader's config. Lazy so a
// PaperTrader constructed as a struct literal (every test, and every cmd/ wiring today) doesn't
// need a separate initialization step to be usable.
func (e *PaperTrader) conductor() *conductor.Conductor {
	e.conductorOnce.Do(func() {
		e.lifecycle = conductor.New(conductor.Config{
			UpdatePnLThresholdPct: e.RLUpdatePnLThresholdPct,
			UpdateMaxInterval:     e.RLUpdateMaxInterval,
			AllowEarlyClose:       e.RLEarlyClose,
			Clamps:                e.RLClamps,
		})
	})
	return e.lifecycle
}

func (e *PaperTrader) conductorClamps() conductor.Clamps {
	return e.RLClamps
}

// nonZeroPx treats a zero price as "the model set no level", since decimal.Decimal has no nil and
// zero is not a meaningful price for any instrument.
func nonZeroPx(px decimal.Decimal) *decimal.Decimal {
	if !px.IsPositive() {
		return nil
	}
	return &px
}
