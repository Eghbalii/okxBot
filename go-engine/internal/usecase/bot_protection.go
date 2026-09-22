package usecase

import (
	"context"
	"errors"
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
// WHY THIS EXISTS. Until this file, a real position's SL and TP were columns in bot_orders that
// BotTrader's own tick monitor compared against the live price each tick. That works only while
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
// A position that cannot be protected is not kept: openBot flattens it. An unprotected real
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

// baseProtectionRequest builds the shared fields of a protective algo order (instId, tdMode,
// closing side, posSide, size) common to both the SL leg and the TP leg. Reports ok=false when
// the position carries no contract count to close (which openBot already refuses to allow, but
// ensureProtection can encounter on a legacy row opened before this mechanism existed).
func (e *BotTrader) baseProtectionRequest(o port.BotOrder) (domain.AlgoOrderRequest, bool) {
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
	return req, true
}

// slProtectionRequest/tpProtectionRequest build the SL-only / TP-only algo order for an open
// position. Reports ok=false when that particular side has no level to rest.
//
// SPLIT INTO TWO SEPARATE ORDERS (2026-09-22, revising the original single-OCO design below):
// the combined form — one conditional order carrying both slTriggerPx and tpTriggerPx as OCO —
// was found, live against this account's X-Perp instruments, to silently drop the TP side: the
// resting order came back from OKX with the SL trigger stored and the TP trigger empty, even
// though both were sent correctly and both decoded correctly on the Go side before the request
// left this process. Every real position opened while the combined form was in use — including
// automated ones — was protected on the downside only.
//
// ORIGINAL REASONING, KEPT because the risk it describes is real and the split reintroduces it:
// "Both levels ride on ONE order deliberately: OKX treats a conditional order carrying both
// trigger prices as OCO, so whichever fires cancels the other. Two separate orders would leave
// the losing side resting after the winning one filled — a stale order that could later open a
// brand-new position in the opposite direction, on an account that believes it is flat." This is
// now handled by ensureProtection's own stranded-order check (reconciliation, every ~5s):
// whichever side is found to have triggered or vanished causes the OTHER side to be cancelled
// explicitly, closing the same window this design note originally worried about, just after the
// fact rather than never opening it.
func (e *BotTrader) slProtectionRequest(o port.BotOrder) (domain.AlgoOrderRequest, bool) {
	req, ok := e.baseProtectionRequest(o)
	if !ok || o.SLPx == nil || !o.SLPx.IsPositive() {
		return domain.AlgoOrderRequest{}, false
	}
	// Every price sent to the exchange must sit on the instrument's tick (2026-09-10). A level
	// derived from a percentage lands on an arbitrary number of decimals — 100.41424 against SOL's
	// 0.01 tick — and OKX rejects the whole request with a bare "code=1" that names nothing.
	req.SLTriggerPx = e.instrumentOrZero().RoundPriceToTick(*o.SLPx)
	return req, true
}

func (e *BotTrader) tpProtectionRequest(o port.BotOrder) (domain.AlgoOrderRequest, bool) {
	req, ok := e.baseProtectionRequest(o)
	if !ok || o.TPPx == nil || !o.TPPx.IsPositive() {
		return domain.AlgoOrderRequest{}, false
	}
	req.TPTriggerPx = e.instrumentOrZero().RoundPriceToTick(*o.TPPx)
	return req, true
}

// placeProtection rests this position's SL and TP on the exchange as two SEPARATE algo orders
// (2026-09-22, see slProtectionRequest's own doc comment for why) and records both returned
// algoIds, so a later adjustment can amend the right one and a close can cancel both.
//
// SL is placed first, deliberately — per explicit operator instruction, it matters more, so if
// only one side can be placed right now (a transient failure on the second call), the position is
// still protected against the loss direction rather than left fully bare while a retry is
// attempted. TP failing after SL succeeds is reported loudly but does NOT undo the SL placement or
// fail the whole call — ensureProtection's next reconciliation pass will notice the missing TP
// side and place it, the same self-healing path a TP that vanishes later already goes through.
//
// Retries once per side on failure before giving up on that side. A single transient error (a
// rate-limit blip, a dropped connection) is not evidence that protection is impossible.
//
// wantSL/wantTP let a caller place only the side(s) actually missing — ensureProtection's own
// call site, where the OTHER side may already be live and re-placing it would rest a second order
// on top of a perfectly good one. The open-time call site (openBot) wants both, since nothing
// exists yet there.
func (e *BotTrader) placeProtection(ctx context.Context, o port.BotOrder, wantSL, wantTP bool, logger *slog.Logger) (slAlgoID, tpAlgoID string, err error) {
	var slReq, tpReq domain.AlgoOrderRequest
	var slOK, tpOK bool
	if wantSL {
		slReq, slOK = e.slProtectionRequest(o)
	}
	if wantTP {
		tpReq, tpOK = e.tpProtectionRequest(o)
	}
	if !slOK && !tpOK {
		return "", "", fmt.Errorf("order %d has no stop or target to rest on the exchange", o.ID)
	}

	if slOK {
		slAlgoID, err = e.placeOneProtectionLeg(ctx, o, slReq, "sl", logger)
		if err != nil {
			metrics.BotProtectionFailedTotal.WithLabelValues(e.InstID).Inc()
			return "", "", fmt.Errorf("place stop-loss order: %w", err)
		}
		if e.Repo != nil && o.ID != 0 {
			if setErr := e.Repo.SetBotOrderExchangeAlgoOrderID(ctx, o.ID, slAlgoID); setErr != nil {
				logger.Error("stop-loss order placed but its algoId could not be recorded; "+
					"adjustments and cancel-on-close will not reach it",
					"id", o.ID, "instId", e.InstID, "algoId", slAlgoID, "error", setErr)
			}
		}
	}

	if tpOK {
		tpAlgoID, err = e.placeOneProtectionLeg(ctx, o, tpReq, "tp", logger)
		if err != nil {
			// SL (if placed above) is real and stays. TP not landing is reported and left for the
			// reconciliation loop to retry, per this function's own doc comment.
			metrics.BotProtectionTPFailedTotal.WithLabelValues(e.InstID).Inc()
			logger.Error("stop-loss placed but take-profit could not be rested; the reconciliation loop will retry it",
				"id", o.ID, "instId", e.InstID, "error", err)
			return slAlgoID, "", nil
		}
		if e.Repo != nil && o.ID != 0 {
			if setErr := e.Repo.SetBotOrderExchangeTPAlgoOrderID(ctx, o.ID, tpAlgoID); setErr != nil {
				logger.Error("take-profit order placed but its algoId could not be recorded; "+
					"adjustments and cancel-on-close will not reach it",
					"id", o.ID, "instId", e.InstID, "algoId", tpAlgoID, "error", setErr)
			}
		}
	}

	metrics.BotProtectionPlacedTotal.WithLabelValues(e.InstID).Inc()
	logger.Info("rested sl/tp on the exchange as separate orders", "id", o.ID, "instId", e.InstID,
		"slAlgoId", slAlgoID, "tpAlgoId", tpAlgoID)
	return slAlgoID, tpAlgoID, nil
}

// placeOneProtectionLeg places a single-sided algo order (SL-only or TP-only), retrying once on a
// transient failure.
func (e *BotTrader) placeOneProtectionLeg(ctx context.Context, o port.BotOrder, req domain.AlgoOrderRequest, side string, logger *slog.Logger) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		algoID, err := e.Exchange.PlaceAlgoOrder(req)
		if err == nil {
			return algoID, nil
		}
		lastErr = err
		logger.Warn("failed to rest a protective order leg on the exchange", "id", o.ID,
			"instId", e.InstID, "side", side, "attempt", attempt, "error", err)
	}
	return "", lastErr
}

// amendProtection moves the SL and/or TP resting orders' trigger prices to match levels this
// system has just changed — the exchange-side half of every SL/TP adjustment, whether the model
// made it or an operator did from the panel.
//
// SL and TP are two SEPARATE resting orders (2026-09-22, see slProtectionRequest's own doc
// comment), so each is amended independently, only when that side actually has a resting order
// AND a new level for it. A missing algoId for a side that has a new level is a real, reportable
// condition rather than a silent skip — it means that side's protection is not on the exchange,
// which is exactly what must never pass unnoticed. A side with no new level proposed is left
// untouched, not amended to nothing.
func (e *BotTrader) amendProtection(ctx context.Context, o port.BotOrder, newSL, newTP *decimal.Decimal, logger *slog.Logger) error {
	inst := e.instrumentOrZero()
	var errs []error

	if newSL != nil && newSL.IsPositive() {
		if o.ExchangeAlgoOrderID == nil || *o.ExchangeAlgoOrderID == "" {
			errs = append(errs, fmt.Errorf("order %d has no resting stop-loss order to amend", o.ID))
		} else {
			req := domain.AlgoOrderAmend{InstID: e.execInstID(), AlgoID: *o.ExchangeAlgoOrderID,
				SLTriggerPx: inst.RoundPriceToTick(*newSL)}
			if err := e.Exchange.AmendAlgoOrder(req); err != nil {
				metrics.BotProtectionAmendFailedTotal.WithLabelValues(e.InstID).Inc()
				errs = append(errs, fmt.Errorf("amend stop-loss order: %w", err))
			} else {
				metrics.BotProtectionAmendedTotal.WithLabelValues(e.InstID).Inc()
				logger.Info("moved the exchange's resting sl", "id", o.ID, "instId", e.InstID,
					"algoId", *o.ExchangeAlgoOrderID, "sl", req.SLTriggerPx)
			}
		}
	}

	if newTP != nil && newTP.IsPositive() {
		if o.ExchangeTPAlgoOrderID == nil || *o.ExchangeTPAlgoOrderID == "" {
			errs = append(errs, fmt.Errorf("order %d has no resting take-profit order to amend", o.ID))
		} else {
			req := domain.AlgoOrderAmend{InstID: e.execInstID(), AlgoID: *o.ExchangeTPAlgoOrderID,
				TPTriggerPx: inst.RoundPriceToTick(*newTP)}
			if err := e.Exchange.AmendAlgoOrder(req); err != nil {
				metrics.BotProtectionAmendFailedTotal.WithLabelValues(e.InstID).Inc()
				errs = append(errs, fmt.Errorf("amend take-profit order: %w", err))
			} else {
				metrics.BotProtectionAmendedTotal.WithLabelValues(e.InstID).Inc()
				logger.Info("moved the exchange's resting tp", "id", o.ID, "instId", e.InstID,
					"algoId", *o.ExchangeTPAlgoOrderID, "tp", req.TPTriggerPx)
			}
		}
	}

	return errors.Join(errs...)
}

// cancelProtection removes both resting orders (SL and TP, if present) after their position has
// been closed by another route (a model early close, an operator's manual close, the staleness
// timeout, or the in-process backup monitor firing first) — or after the reconciliation loop finds
// one side has triggered/vanished and the other must not be left stranded (ensureProtection).
//
// Best-effort by design: it runs AFTER the close is durably recorded, and a failure here must
// never make a completed close look failed. The cost of a leftover order is bounded — OKX cancels
// a conditional order whose position no longer exists — but it is still cancelled explicitly
// rather than relied on, because "the exchange will probably clean it up" is not something to
// build on when the failure mode is opening an unwanted position.
func (e *BotTrader) cancelProtection(ctx context.Context, o port.BotOrder, logger *slog.Logger) {
	e.cancelProtectionLeg(o.ID, o.ExchangeAlgoOrderID, "sl", logger)
	e.cancelProtectionLeg(o.ID, o.ExchangeTPAlgoOrderID, "tp", logger)
}

func (e *BotTrader) cancelProtectionLeg(orderID int64, algoID *string, side string, logger *slog.Logger) {
	if algoID == nil || *algoID == "" {
		return
	}
	if err := e.Exchange.CancelAlgoOrder(e.execInstID(), *algoID); err != nil {
		logger.Warn("failed to cancel a resting protective order leg after closing its position",
			"id", orderID, "instId", e.InstID, "side", side, "algoId", *algoID, "error", err)
		return
	}
	logger.Info("cancelled a resting protective order leg", "id", orderID, "instId", e.InstID,
		"side", side, "algoId", *algoID)
}

// legState is what ensureProtection learns about one resting algo order leg (SL or TP) on a
// single verification read.
type legState int

const (
	legStateMissing   legState = iota // no algoId recorded at all, or the read found nothing live and untriggered
	legStateLive                      // confirmed resting on the exchange right now
	legStateTriggered                 // confirmed fired — the position is on its way to flat
	legStateUnknown                   // the read itself failed; treat as "don't know", never as absent
)

// checkProtectionLeg reads one algo order's current state from the exchange. algoID == nil/""
// reports legStateMissing without a network call.
func (e *BotTrader) checkProtectionLeg(orderID int64, algoID *string, side string, logger *slog.Logger) legState {
	if algoID == nil || *algoID == "" {
		return legStateMissing
	}
	status, err := e.Exchange.GetAlgoOrder(e.execInstID(), *algoID)
	if err != nil {
		logger.Warn("could not verify a resting protective order leg; leaving it in place",
			"id", orderID, "instId", e.InstID, "side", side, "algoId", *algoID, "error", err)
		return legStateUnknown
	}
	if status.IsLive() {
		return legStateLive
	}
	if status.State == "effective" {
		return legStateTriggered
	}
	logger.Error("a real position's protective order leg is no longer on the exchange",
		"id", orderID, "instId", e.InstID, "side", side, "algoId", *algoID, "state", status.State)
	return legStateMissing
}

// ensureProtection is the verification half of "the exchange is primary": for every open real
// position, confirm SL and TP are each still actually resting there (two SEPARATE orders,
// 2026-09-22 — see slProtectionRequest's own doc comment), re-placing whichever side is missing
// and cancelling whichever side is left stranded after its sibling triggers. Called from the
// reconciliation loop.
//
// Trusting a successful placement forever is the assumption this exists to avoid. An algo order
// can be cancelled from OKX's own UI, rejected after acceptance, or lost to a margin-mode change —
// none of which produce any signal in this process. Verification is what turns "we placed a stop
// once" into "there is a stop on the exchange right now".
//
// A position where EITHER leg has TRIGGERED is left alone on the closing side and the OTHER side
// is cancelled if still live — the stranded-order risk slProtectionRequest's own doc comment
// describes (a leftover order that could later fire against an unrelated future position in
// net_mode). The reconciliation poll's own position comparison is what closes the local row; this
// function only prevents a stale sibling order from outliving that close.
func (e *BotTrader) ensureProtection(ctx context.Context, open []port.BotOrder, logger *slog.Logger) {
	for _, o := range open {
		// Only positions that actually hold exposure need protecting. A row still opening, or one
		// already closing, has either nothing to protect yet or nothing left to protect.
		if o.Status != "filled" && o.Status != "partial" {
			continue
		}

		slState := e.checkProtectionLeg(o.ID, o.ExchangeAlgoOrderID, "sl", logger)
		tpState := e.checkProtectionLeg(o.ID, o.ExchangeTPAlgoOrderID, "tp", logger)

		// Either side having fired means the position is on its way to flat. Cancel whichever
		// sibling is still live so it cannot outlive this position — never re-place into a closing
		// position, which slProtectionRequest's own doc comment names as exactly the risk splitting
		// SL and TP reintroduced.
		if slState == legStateTriggered || tpState == legStateTriggered {
			if slState == legStateTriggered {
				logger.Info("stop-loss has triggered; the reconciliation poll will close the row",
					"id", o.ID, "instId", e.InstID)
				if tpState == legStateLive {
					e.cancelProtectionLeg(o.ID, o.ExchangeTPAlgoOrderID, "tp", logger)
				}
			}
			if tpState == legStateTriggered {
				logger.Info("take-profit has triggered; the reconciliation poll will close the row",
					"id", o.ID, "instId", e.InstID)
				if slState == legStateLive {
					e.cancelProtectionLeg(o.ID, o.ExchangeAlgoOrderID, "sl", logger)
				}
			}
			continue
		}

		// A read that failed (legStateUnknown) is "don't know", never "absent" — re-placing on top
		// of an order that is actually still fine would risk two live orders on one leg, which on
		// this account's hedge-mode-capable posture can close the position twice.
		needsSL := slState == legStateMissing && o.SLPx != nil && o.SLPx.IsPositive()
		needsTP := tpState == legStateMissing && o.TPPx != nil && o.TPPx.IsPositive()
		if !needsSL && !needsTP {
			continue
		}
		if needsSL {
			metrics.BotProtectionMissingTotal.WithLabelValues(e.InstID).Inc()
		}
		if needsTP {
			metrics.BotProtectionMissingTotal.WithLabelValues(e.InstID).Inc()
		}
		logger.Error("a real position is missing one or more protective order legs; re-placing",
			"id", o.ID, "instId", e.InstID, "needsSL", needsSL, "needsTP", needsTP)

		if _, _, err := e.placeProtection(ctx, o, needsSL, needsTP, logger); err != nil {
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
func (e *BotTrader) closeFactsFromExchange(o port.BotOrder, logger *slog.Logger) (
	reason string, closePx decimal.Decimal, exchangePnL, exchangeFee *decimal.Decimal,
) {
	reason, closePx = conductor.CloseReasonManual, o.EntryPx

	// SL and TP are two SEPARATE resting orders (2026-09-22) — check both, since only one of them
	// (whichever fired) can tell us why the position closed. Checked in this order because a
	// caller reaching this function already knows the position is flat, and if BOTH somehow read
	// as triggered (a genuine race is possible but very narrow), the stop-loss reading wins: it is
	// the number that matters more for the record, matching this whole feature's own SL-priority
	// instruction.
	if reason2, closePx2, pnl2, fee2, ok := e.closeFactsFromLeg(o, o.ExchangeAlgoOrderID, "sl", logger); ok {
		return reason2, closePx2, pnl2, fee2
	}
	if reason2, closePx2, pnl2, fee2, ok := e.closeFactsFromLeg(o, o.ExchangeTPAlgoOrderID, "tp", logger); ok {
		return reason2, closePx2, pnl2, fee2
	}
	return reason, closePx, nil, nil
}

// closeFactsFromLeg checks ONE resting algo order leg for whether it is the one that closed the
// position, returning ok=false when this leg has nothing to report (never recorded, unreadable,
// or simply didn't fire) — closeFactsFromExchange tries the other leg in that case.
func (e *BotTrader) closeFactsFromLeg(o port.BotOrder, algoID *string, side string, logger *slog.Logger) (
	reason string, closePx decimal.Decimal, exchangePnL, exchangeFee *decimal.Decimal, ok bool,
) {
	if algoID == nil || *algoID == "" {
		return "", decimal.Decimal{}, nil, nil, false
	}
	algo, err := e.Exchange.GetAlgoOrder(e.execInstID(), *algoID)
	if err != nil {
		logger.Warn("reconcile: could not read a protective order leg to determine why the position closed",
			"id", o.ID, "instId", e.InstID, "side", side, "algoId", *algoID, "error", err)
		return "", decimal.Decimal{}, nil, nil, false
	}
	triggered, triggerOK := algo.TriggeredReason()
	if !triggerOK && algo.State == "effective" {
		// The order HAS fired but OKX has not filled in actualSide/ordId yet — observed on real
		// order 43 (2026-09-10), where the reconciliation poll read the algo order about a second
		// after the trigger and got state="effective" with an empty actualSide, then recorded the
		// close as manual. The same read moments later carried actualSide="sl".
		//
		// Retrying briefly is the whole fix: this is a close being recorded once, and getting the
		// reason and the real close price wrong is not worth avoiding a one-second wait on a path
		// that already only runs when a position has just disappeared from the exchange.
		for attempt := 0; attempt < 3 && !triggerOK; attempt++ {
			time.Sleep(500 * time.Millisecond)
			if algo, err = e.Exchange.GetAlgoOrder(e.execInstID(), *algoID); err != nil {
				break
			}
			triggered, triggerOK = algo.TriggeredReason()
		}
	}
	if !triggerOK {
		// Either this leg genuinely did not fire — the OTHER leg triggered instead, or a manual
		// close in the OKX app, a liquidation, something outside this system — or OKX never filled
		// in which side fired. closeFactsFromExchange's caller decides what "neither leg answered"
		// means; this function only reports what THIS leg knows.
		if algo.State == "effective" {
			logger.Warn("a protective order leg fired but the exchange never reported which side; trying the other leg",
				"id", o.ID, "instId", e.InstID, "side", side, "algoId", *algoID)
		}
		return "", decimal.Decimal{}, nil, nil, false
	}
	reason = triggered
	closePx = o.EntryPx

	// The trigger created an ordinary order to flatten the position, and THAT order carries what
	// actually happened: fill price, realized PnL, fee. Without it the close price would still be
	// the entry price and the outcome would still be wrong, just labelled correctly.
	if algo.OrdID == "" {
		logger.Warn("reconcile: protective order leg fired but named no resulting order; close price unavailable",
			"id", o.ID, "instId", e.InstID, "side", side, "reason", reason)
		return reason, closePx, nil, nil, true
	}
	status, err := e.Exchange.GetOrder(e.execInstID(), algo.OrdID)
	if err != nil {
		logger.Warn("reconcile: could not read the order a protective trigger created",
			"id", o.ID, "instId", e.InstID, "side", side, "ordId", algo.OrdID, "error", err)
		return reason, closePx, nil, nil, true
	}
	if status.AvgPx.IsPositive() {
		closePx = status.AvgPx
	}
	exchangePnL, exchangeFee = exchangeCloseNumbers(status)
	logger.Info("reconcile: the exchange's own protective order closed this position",
		"id", o.ID, "instId", e.InstID, "side", side, "reason", reason, "closePx", closePx,
		"exchangeOrdId", algo.OrdID, "exchangePnl", exchangePnL)
	return reason, closePx, exchangePnL, exchangeFee, true
}

// exchangeCloseFacts carries a close's numbers when the EXCHANGE closed the position and this
// system therefore has no flatten of its own to read them from. Nil fields mean OKX did not report
// that figure, which stays distinct from a real zero.
type exchangeCloseFacts struct {
	PnL *decimal.Decimal
	Fee *decimal.Decimal
}

// protectionAlreadyFired reports whether the exchange's own resting SL or TP order (two SEPARATE
// orders, 2026-09-22) has already triggered for this position — i.e. whether OKX has closed it
// without us. Fired if EITHER leg has triggered.
//
// Returns (fired, ok). ok=false means the question could not be answered for EITHER leg: no
// protective order was ever recorded for either side, or both reads failed. Callers must treat
// that as "don't know" and fall back to their normal close, never as "not fired" — declining to
// flatten on an unreadable status would leave a real position open on nothing more than a failed
// API call. A leg that answers definitively "not fired" while the other leg's read failed still
// yields ok=true (fired=false is a real answer from the leg that DID answer), since between two
// legs at least one usable answer is enough to know the position has not closed via protection.
//
// Added 2026-09-11 after every genuine exchange error on this deployment turned out to be the same
// race: the exchange's stop fires, then this process's tick monitor sees the same touch a moment
// later and asks OKX to close a position that is already gone (sCode=51169). The close was never
// at risk; the noise was, and it landed in the one channel that has to stay trustworthy.
func (e *BotTrader) protectionAlreadyFired(o port.BotOrder, logger *slog.Logger) (bool, bool) {
	if e.Exchange == nil {
		return false, false
	}
	slFired, slOK := e.legAlreadyFired(o.ID, o.ExchangeAlgoOrderID, "sl", logger)
	tpFired, tpOK := e.legAlreadyFired(o.ID, o.ExchangeTPAlgoOrderID, "tp", logger)
	if slFired || tpFired {
		return true, true
	}
	if slOK || tpOK {
		return false, true
	}
	return false, false
}

func (e *BotTrader) legAlreadyFired(orderID int64, algoID *string, side string, logger *slog.Logger) (bool, bool) {
	if algoID == nil || *algoID == "" {
		return false, false
	}
	status, err := e.Exchange.GetAlgoOrder(e.execInstID(), *algoID)
	if err != nil {
		logger.Warn("could not read a protective order leg before closing; trying the other leg",
			"id", orderID, "instId", e.InstID, "side", side, "algoId", *algoID, "error", err)
		return false, false
	}
	// Still resting means it has NOT fired. Anything else — effective, canceled, failed — means it
	// is no longer guarding the position, and only "effective" means it actually executed.
	return status.State == "effective", true
}
