package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// Exchange-side stop-loss / take-profit for real positions (2026-09-09 request).
//
// WHY THIS EXISTS. Until this file, a real position's SL and TP were columns in real_orders that
// RealTrader's own tick monitor compared against the live price each tick. That works only while
// this process is alive, connected, and receiving ticks — so a crash, a deploy, an OOM, a stalled
// Kafka feed, or a network partition left real capital running with no protection at all, and
// nothing anywhere would have reported the position as unprotected. Real order 33 is what surfaced
// it: its stop existed in the database and never existed on the exchange.
//
// The protection now rests on OKX itself, as a conditional ("algo") order placed immediately after
// the entry fills. It survives everything above, because it does not depend on this process at all.
//
// THE DIVISION OF LABOUR, decided 2026-09-09:
//   - The exchange order is PRIMARY. It is what actually stops a losing trade.
//   - This service VERIFIES it (ensureProtection, from the reconciliation poll) rather than
//     assuming a once-successful placement stays valid forever — a protective order that has
//     vanished is re-placed.
//   - The in-process tick monitor stays as a BACKUP, not the primary. Its close path is unchanged.
//
// A position that cannot be protected is not kept: openReal flattens it. An unprotected real
// position is a worse outcome than a wasted round-trip fee, and the alternative — keeping it and
// hoping — is what this whole change exists to eliminate.

// closingSide returns the side that flattens a position entered on entrySide. A protective order
// is always the opposite side of the trade it protects, since its job is to close it.
func closingSide(entrySide string) string {
	if entrySide == "buy" {
		return "sell"
	}
	return "buy"
}

// protectionRequest builds the exchange-side SL/TP order for an open position, or reports ok=false
// when the position carries no levels worth resting (which openReal already refuses to allow, but
// ensureProtection can encounter on a legacy row opened before this mechanism existed).
//
// Both levels ride on ONE order deliberately: OKX treats a conditional order carrying both trigger
// prices as OCO, so whichever fires cancels the other. Two separate orders would leave the losing
// side resting after the winning one filled — a stale order that could later open a brand-new
// position in the opposite direction, on an account that believes it is flat.
func (e *RealTrader) protectionRequest(o port.RealOrder) (domain.AlgoOrderRequest, bool) {
	sz := o.Contracts
	if sz == nil || !sz.IsPositive() {
		return domain.AlgoOrderRequest{}, false
	}
	req := domain.AlgoOrderRequest{
		InstID: e.execInstID(),
		TdMode: e.TdMode,
		Side:   closingSide(o.Side),
		Sz:     *sz,
	}
	if e.PosMode == "long_short" {
		// The protective order's posSide is the POSITION's side, not its own (opposite) side —
		// verified against the real account by cmd/okx-apitest before this call was promoted into
		// the production client.
		req.PosSide = posSideFor(signedNotionalForSide(o.Side))
	}
	// Every price sent to the exchange must sit on the instrument's tick (2026-09-10). A level
	// derived from a percentage lands on an arbitrary number of decimals — 100.41424 against SOL's
	// 0.01 tick — and OKX rejects the whole request with a bare "code=1" that names nothing.
	inst := e.instrumentOrZero()
	if o.SLPx != nil && o.SLPx.IsPositive() {
		req.SLTriggerPx = inst.RoundPriceToTick(*o.SLPx)
	}
	if o.TPPx != nil && o.TPPx.IsPositive() {
		req.TPTriggerPx = inst.RoundPriceToTick(*o.TPPx)
	}
	if !req.SLTriggerPx.IsPositive() && !req.TPTriggerPx.IsPositive() {
		return domain.AlgoOrderRequest{}, false
	}
	return req, true
}

// placeProtection rests this position's SL/TP on the exchange and records the returned algoId, so
// a later adjustment can amend that exact order and a close can cancel it.
//
// Retries once on failure before giving up. A single transient error (a rate-limit blip, a dropped
// connection) is not evidence that protection is impossible, and the caller's response to failure
// is to flatten a position that just opened — an expensive answer to give to a hiccup.
func (e *RealTrader) placeProtection(ctx context.Context, o port.RealOrder, logger *slog.Logger) (string, error) {
	req, ok := e.protectionRequest(o)
	if !ok {
		return "", fmt.Errorf("order %d has no stop or target to rest on the exchange", o.ID)
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		algoID, err := e.Exchange.PlaceAlgoOrder(req)
		if err == nil {
			// o.ID == 0 means the position never persisted, so there is no row to record the
			// algoId on. The protective order is still placed and still protects the position;
			// only our ability to amend or cancel it later is lost, which the reconciliation
			// poll's untracked-position halt is what surfaces.
			if e.Repo != nil && o.ID != 0 {
				if setErr := e.Repo.SetRealOrderExchangeAlgoOrderID(ctx, o.ID, algoID); setErr != nil {
					// The order IS resting on the exchange; only our record of its id is missing.
					// Losing that id means a later adjustment cannot amend it and a close cannot
					// cancel it, so it is reported loudly — but the position is protected, which is
					// the property that matters, so this does not fail the placement.
					logger.Error("protective order placed but its algoId could not be recorded; "+
						"adjustments and cancel-on-close will not reach it",
						"id", o.ID, "instId", e.InstID, "algoId", algoID, "error", setErr)
				}
			}
			metrics.RealProtectionPlacedTotal.WithLabelValues(e.InstID).Inc()
			logger.Info("rested sl/tp on the exchange", "id", o.ID, "instId", e.InstID,
				"algoId", algoID, "sl", req.SLTriggerPx, "tp", req.TPTriggerPx)
			return algoID, nil
		}
		lastErr = err
		logger.Warn("failed to rest sl/tp on the exchange", "id", o.ID, "instId", e.InstID,
			"attempt", attempt, "error", err)
	}
	metrics.RealProtectionFailedTotal.WithLabelValues(e.InstID).Inc()
	return "", fmt.Errorf("place protective order: %w", lastErr)
}

// amendProtection moves the resting order's trigger prices to match levels this system has just
// changed — the exchange-side half of every SL/TP adjustment, whether the model made it or an
// operator did from the panel.
//
// Both trigger prices are always sent, not just the changed one. The resting order carries both
// sides, and OKX's amend leaves an omitted side untouched — so sending only the moved level would
// be correct, but sending both makes the exchange's state a direct function of this system's
// current levels rather than of the sequence of edits that got there, which is the property worth
// having when the two must not drift.
//
// A missing algoId is a real, reportable condition rather than a silent skip: it means this
// position's protection is not on the exchange, which is exactly what must never pass unnoticed.
func (e *RealTrader) amendProtection(ctx context.Context, o port.RealOrder, newSL, newTP *decimal.Decimal, logger *slog.Logger) error {
	if o.ExchangeAlgoOrderID == nil || *o.ExchangeAlgoOrderID == "" {
		return fmt.Errorf("order %d has no resting protective order to amend", o.ID)
	}
	req := domain.AlgoOrderAmend{
		InstID: e.execInstID(),
		AlgoID: *o.ExchangeAlgoOrderID,
	}
	// Rounded to the instrument's tick for the same reason as protectionRequest above.
	inst := e.instrumentOrZero()
	if newSL != nil && newSL.IsPositive() {
		req.SLTriggerPx = inst.RoundPriceToTick(*newSL)
	}
	if newTP != nil && newTP.IsPositive() {
		req.TPTriggerPx = inst.RoundPriceToTick(*newTP)
	}
	if !req.SLTriggerPx.IsPositive() && !req.TPTriggerPx.IsPositive() {
		return fmt.Errorf("order %d: refusing to amend a protective order to no levels at all", o.ID)
	}
	if err := e.Exchange.AmendAlgoOrder(req); err != nil {
		metrics.RealProtectionAmendFailedTotal.WithLabelValues(e.InstID).Inc()
		return fmt.Errorf("amend protective order: %w", err)
	}
	metrics.RealProtectionAmendedTotal.WithLabelValues(e.InstID).Inc()
	logger.Info("moved the exchange's resting sl/tp", "id", o.ID, "instId", e.InstID,
		"algoId", *o.ExchangeAlgoOrderID, "sl", req.SLTriggerPx, "tp", req.TPTriggerPx)
	return nil
}

// cancelProtection removes the resting order after its position has been closed by another route
// (a model early close, an operator's manual close, the staleness timeout, or the in-process
// backup monitor firing first).
//
// Best-effort by design: it runs AFTER the close is durably recorded, and a failure here must
// never make a completed close look failed. The cost of a leftover order is bounded — OKX cancels
// a conditional order whose position no longer exists — but it is still cancelled explicitly
// rather than relied on, because "the exchange will probably clean it up" is not something to
// build on when the failure mode is opening an unwanted position.
func (e *RealTrader) cancelProtection(ctx context.Context, o port.RealOrder, logger *slog.Logger) {
	if o.ExchangeAlgoOrderID == nil || *o.ExchangeAlgoOrderID == "" {
		return
	}
	if err := e.Exchange.CancelAlgoOrder(e.execInstID(), *o.ExchangeAlgoOrderID); err != nil {
		logger.Warn("failed to cancel the resting protective order after closing its position",
			"id", o.ID, "instId", e.InstID, "algoId", *o.ExchangeAlgoOrderID, "error", err)
		return
	}
	logger.Info("cancelled the resting protective order", "id", o.ID, "instId", e.InstID,
		"algoId", *o.ExchangeAlgoOrderID)
}

// ensureProtection is the verification half of "the exchange is primary": for every open real
// position, confirm a protective order is still actually resting there, and re-place it when it is
// not. Called from the reconciliation loop.
//
// Trusting a successful placement forever is the assumption this exists to avoid. An algo order
// can be cancelled from OKX's own UI, rejected after acceptance, or lost to a margin-mode change —
// none of which produce any signal in this process. Verification is what turns "we placed a stop
// once" into "there is a stop on the exchange right now".
//
// A position whose protective order has TRIGGERED is deliberately left alone: it is on its way to
// being flat, and the reconciliation poll's own position comparison is what closes the local row.
// Re-placing a stop for a position that is closing would rest an order against nothing.
func (e *RealTrader) ensureProtection(ctx context.Context, open []port.RealOrder, logger *slog.Logger) {
	for _, o := range open {
		// Only positions that actually hold exposure need protecting. A row still opening, or one
		// already closing, has either nothing to protect yet or nothing left to protect.
		if o.Status != "filled" && o.Status != "partial" {
			continue
		}

		if o.ExchangeAlgoOrderID != nil && *o.ExchangeAlgoOrderID != "" {
			status, err := e.Exchange.GetAlgoOrder(e.execInstID(), *o.ExchangeAlgoOrderID)
			if err != nil {
				// Unknown, not absent. Re-placing on a failed READ would risk a second protective
				// order resting alongside a perfectly good first one, which on a hedge-mode account
				// can close the position twice — the opposite of protection.
				logger.Warn("could not verify the resting protective order; leaving it in place",
					"id", o.ID, "instId", e.InstID, "algoId", *o.ExchangeAlgoOrderID, "error", err)
				continue
			}
			if status.IsLive() {
				continue
			}
			if status.State == "effective" {
				logger.Info("protective order has triggered; the reconciliation poll will close the row",
					"id", o.ID, "instId", e.InstID, "algoId", *o.ExchangeAlgoOrderID)
				continue
			}
			logger.Error("a real position's protective order is no longer on the exchange; re-placing it",
				"id", o.ID, "instId", e.InstID, "algoId", *o.ExchangeAlgoOrderID, "state", status.State)
			metrics.RealProtectionMissingTotal.WithLabelValues(e.InstID).Inc()
		} else {
			logger.Error("a real position has no protective order on the exchange; placing one",
				"id", o.ID, "instId", e.InstID)
			metrics.RealProtectionMissingTotal.WithLabelValues(e.InstID).Inc()
		}

		if _, err := e.placeProtection(ctx, o, logger); err != nil {
			// Left for the next poll rather than escalated to a close here. A close is the right
			// answer at OPEN time, when the position was just acquired deliberately and can be
			// undone cheaply; for a position that has been running, force-flattening it because one
			// API call failed would turn a transient exchange problem into a realized loss. The
			// in-process backup monitor still covers it in the meantime, and the error is loud.
			logger.Error("failed to restore a real position's protective order; the in-process monitor is the only cover until the next attempt",
				"id", o.ID, "instId", e.InstID, "error", err)
		}
	}
}

// closeFactsFromExchange answers "why did this position close, and at what price" by asking the
// exchange, for a position found already flat by the reconciliation poll.
//
// This exists because the answer used to be assumed. The stale-close path predates exchange-side
// SL/TP (§35) and was written when the only way a position could vanish from OKX without this
// system closing it was a human doing it in the OKX app — so it recorded conductor.CloseReasonManual
// and, having no live tick to hand, used the order's own ENTRY price as the close price.
//
// Once the stop lives on the exchange, that assumption is wrong in the most common case. Real
// orders 39 and 40 (2026-09-09) were closed by OKX's own stop-loss and recorded as manual closes at
// their entry price, with no close data at all — which also made the recorded loss ~15x smaller
// than the real one (-0.018 against an actual -0.265), because a close price equal to the entry
// price implies a trade that went nowhere.
//
// Falls back to the old behavior (manual, at entry price) only when the exchange cannot tell us
// otherwise: an order with no protective order recorded, an unreadable algo order, or one that did
// not fire. A guess is never substituted for an answer.
func (e *RealTrader) closeFactsFromExchange(o port.RealOrder, logger *slog.Logger) (
	reason string, closePx decimal.Decimal, exchangePnL, exchangeFee *decimal.Decimal,
) {
	reason, closePx = conductor.CloseReasonManual, o.EntryPx

	if o.ExchangeAlgoOrderID == nil || *o.ExchangeAlgoOrderID == "" {
		return reason, closePx, nil, nil
	}
	algo, err := e.Exchange.GetAlgoOrder(e.execInstID(), *o.ExchangeAlgoOrderID)
	if err != nil {
		logger.Warn("reconcile: could not read the protective order to determine why the position closed",
			"id", o.ID, "instId", e.InstID, "algoId", *o.ExchangeAlgoOrderID, "error", err)
		return reason, closePx, nil, nil
	}
	triggered, ok := algo.TriggeredReason()
	if !ok && algo.State == "effective" {
		// The order HAS fired but OKX has not filled in actualSide/ordId yet — observed on real
		// order 43 (2026-09-10), where the reconciliation poll read the algo order about a second
		// after the trigger and got state="effective" with an empty actualSide, then recorded the
		// close as manual. The same read moments later carried actualSide="sl".
		//
		// Retrying briefly is the whole fix: this is a close being recorded once, and getting the
		// reason and the real close price wrong is not worth avoiding a one-second wait on a path
		// that already only runs when a position has just disappeared from the exchange.
		for attempt := 0; attempt < 3 && !ok; attempt++ {
			time.Sleep(500 * time.Millisecond)
			if algo, err = e.Exchange.GetAlgoOrder(e.execInstID(), *o.ExchangeAlgoOrderID); err != nil {
				break
			}
			triggered, ok = algo.TriggeredReason()
		}
	}
	if !ok {
		// Either the protective order genuinely did not fire — a manual close in the OKX app, a
		// liquidation, something outside this system — or OKX never filled in which side fired.
		// The original assumption is right for the first case and the honest answer for the
		// second, which is exactly why it is kept rather than removed.
		if algo.State == "effective" {
			logger.Warn("protective order fired but the exchange never reported which side; recording as a manual close",
				"id", o.ID, "instId", e.InstID, "algoId", *o.ExchangeAlgoOrderID)
		}
		return reason, closePx, nil, nil
	}
	reason = triggered

	// The trigger created an ordinary order to flatten the position, and THAT order carries what
	// actually happened: fill price, realized PnL, fee. Without it the close price would still be
	// the entry price and the outcome would still be wrong, just labelled correctly.
	if algo.OrdID == "" {
		logger.Warn("reconcile: protective order fired but named no resulting order; close price unavailable",
			"id", o.ID, "instId", e.InstID, "reason", reason)
		return reason, closePx, nil, nil
	}
	status, err := e.Exchange.GetOrder(e.execInstID(), algo.OrdID)
	if err != nil {
		logger.Warn("reconcile: could not read the order the protective trigger created",
			"id", o.ID, "instId", e.InstID, "ordId", algo.OrdID, "error", err)
		return reason, closePx, nil, nil
	}
	if status.AvgPx.IsPositive() {
		closePx = status.AvgPx
	}
	exchangePnL, exchangeFee = exchangeCloseNumbers(status)
	logger.Info("reconcile: the exchange's own protective order closed this position",
		"id", o.ID, "instId", e.InstID, "reason", reason, "closePx", closePx,
		"exchangeOrdId", algo.OrdID, "exchangePnl", exchangePnL)
	return reason, closePx, exchangePnL, exchangeFee
}

// exchangeCloseFacts carries a close's numbers when the EXCHANGE closed the position and this
// system therefore has no flatten of its own to read them from. Nil fields mean OKX did not report
// that figure, which stays distinct from a real zero.
type exchangeCloseFacts struct {
	PnL *decimal.Decimal
	Fee *decimal.Decimal
}

// protectionAlreadyFired reports whether the exchange's own resting SL/TP order has already
// triggered for this position — i.e. whether OKX has closed it without us.
//
// Returns (fired, ok). ok=false means the question could not be answered: no protective order was
// ever recorded, or the read failed. Callers must treat that as "don't know" and fall back to
// their normal close, never as "not fired" — declining to flatten on an unreadable status would
// leave a real position open on nothing more than a failed API call.
//
// Added 2026-09-11 after every genuine exchange error on this deployment turned out to be the same
// race: the exchange's stop fires, then this process's tick monitor sees the same touch a moment
// later and asks OKX to close a position that is already gone (sCode=51169). The close was never
// at risk; the noise was, and it landed in the one channel that has to stay trustworthy.
func (e *RealTrader) protectionAlreadyFired(o port.RealOrder, logger *slog.Logger) (bool, bool) {
	if o.ExchangeAlgoOrderID == nil || *o.ExchangeAlgoOrderID == "" {
		return false, false
	}
	if e.Exchange == nil {
		return false, false
	}
	status, err := e.Exchange.GetAlgoOrder(e.execInstID(), *o.ExchangeAlgoOrderID)
	if err != nil {
		logger.Warn("could not read the protective order before closing; falling back to a normal flatten",
			"id", o.ID, "instId", e.InstID, "algoId", *o.ExchangeAlgoOrderID, "error", err)
		return false, false
	}
	// Still resting means it has NOT fired, so this system's own close is the one that has to act.
	// Anything else — effective, canceled, failed — means it is no longer guarding the position,
	// and only "effective" means it actually executed.
	return status.State == "effective", true
}
