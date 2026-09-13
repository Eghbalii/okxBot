package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/metrics"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/risk"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// RealTrader brings cmd/trader onto the same strategy-signal + conductor lifecycle
// usecase.PaperTrader already runs (CLAUDE.md §27.3), replacing the old flat delta-notional
// rebalance loop (trade.go's Trader) — but placing real orders on the exchange instead of
// bookkeeping-only virtual ones. Deliberately NOT PaperTrader-with-a-flag: the two engines share
// their pure IO skeleton (tickfeed.go) and their pure sizing/adjustment math
// (sizeFromModelAction/computeAdjustedLevels, extracted for exactly this reuse), but keep separate
// types so a bug in real-money code can never leak into paper trading's already-proven,
// heavily-tested path, and vice versa.
//
// The "no fork" mechanic (CLAUDE.md §15.4's shadow-fork A/B comparison is paper-trading-only):
// real trading holds at most one open position per token per side, and the model edits its SL/TP
// in place. Corrected 2026-09-03 (see the plan doc's §3a): that edit is watched by RealTrader's
// own in-process tick monitor, the SAME mechanism PaperTrader already uses — never a resting
// conditional/algo order on OKX. A periodic reconciliation poll (reconcile, see below) checks
// GetPositions/GetBalance against this process's own bookkeeping to catch drift (a manual close on
// OKX's own UI, a liquidation, a missed fill) rather than trusting local state blindly.
//
// Futures/perpetual-swap ("SWAP") endpoints only, matching every other exchange call in this
// codebase — this type introduces no new instrument type or exchange endpoint category.
type RealTrader struct {
	InstID          string
	Bars            []string
	CandleWindow    int
	Strategies      []StrategyAssignment
	TickConsumer    port.MarketDataConsumer
	CandleConsumers map[string]port.MarketDataConsumer
	Repo            port.Repository
	Exchange        port.ExchangeClient
	Model           port.ModelClient
	RiskManager     *risk.Manager
	Logger          *slog.Logger

	// ExecInstID is the instId actually placed/canceled/queried on the exchange — NOT necessarily
	// InstID. Found live 2026-09-04: this project's market data (candles/ticks/paper_orders/the RL
	// observation's token identity) is collected against the classic, deeply liquid SWAP
	// instruments (e.g. BTC-USDT-SWAP), but a given real account may only have usable margin on a
	// DIFFERENT OKX product for the same underlying — this account's is BTC-USD_UM_XPERP-<date>
	// ("X-Perp", instType FUTURES, settled in USD/USDC/USDG under Multi-currency margin mode).
	// Prices track within ~0.1% of the SWAP market (verified live across all 10 configured tokens),
	// so InstID's collected candles/observation remain valid training/decision input — only the
	// literal exchange call needs to target a possibly-different instId. Falls back to InstID when
	// empty, so a deployment whose account CAN trade the SWAP instrument directly needs no mapping
	// at all. ExecInstType/ExecCtValCcy answer instType/CtValCcy for GetPositions/GetInstrument;
	// SettleCcy answers GetBalance's ccy — all three must agree with whichever product ExecInstID
	// actually names, since a SWAP-shaped default (instType=SWAP, ccy=USDT) silently returns
	// nothing (not an error) against an account whose usable balance/positions live elsewhere.
	ExecInstID   string
	ExecInstType string // e.g. "SWAP" or "FUTURES"; defaults to "SWAP" when empty
	SettleCcy    string // e.g. "USDT" or "USDC"; defaults to "USDT" when empty

	// Mode is "demo" or "real" (CLAUDE.md §15.6) — never "paper". Selects which account_equity row
	// this engine's equity timeline is recorded under and which mode's rows ListPositions/
	// ListOpenPaperOrders-equivalent queries are scoped to (see openPositions/allOpenPositions
	// below) — critical, since paper trading may be running concurrently against the SAME InstID
	// and paper_orders has no per-mode partition beyond the Mode column itself.
	Mode string
	// TdMode/PosMode are OKX's margin/position mode config (CLAUDE.md §27.2) — same fields Trader
	// already carries, reused as-is.
	TdMode  string // "cross" or "isolated"
	PosMode string // "net" or "long_short" (hedge mode)

	ActiveTokens      []string
	AccountInitialUSD decimal.Decimal

	// SafeMoneyUSD is subtracted from the exchange's reported balance before it is ever used for
	// sizing or recorded as this mode's equity (CLAUDE.md real-trading readiness plan, 2026-09-04
	// operator decision) — a risk-reduction reserve the operator wants to stay untouched even if
	// every open position were liquidated (isolated margin bounds a position's own loss to its own
	// margin, never the whole account, so a reserve set aside this way is genuinely safe from
	// liquidation, not just from this engine's own sizing decisions). Every real balance figure
	// this engine produces (observation sizing, the equity timeline the panel reads) is
	// exchange-balance-minus-SafeMoneyUSD, floored at zero so a balance below the reserve never
	// reports as negative equity. Zero (the default) preserves today's behavior of using the full
	// reported balance.
	SafeMoneyUSD decimal.Decimal

	// MaxLeverage/MaxPositionPct/MaxTotalExposurePct feed sizeFromModelAction (the shared free
	// function, CLAUDE.md §27's plan commit 2) — same equity-fraction caps PaperTrader.RLSizing
	// uses, expressed against the exchange's own reported equity here (see buildObservation) rather
	// than a bookkeeping row this engine owns.
	MaxLeverage         decimal.Decimal
	MaxPositionPct      decimal.Decimal
	MaxTotalExposurePct decimal.Decimal

	// RLUpdatePnLThresholdPct/RLUpdateMaxInterval/RLEarlyClose/RLClamps/MaxOpenDuration configure
	// the conductor exactly like PaperTrader's equivalents (CLAUDE.md §15.12).
	RLUpdatePnLThresholdPct decimal.Decimal
	RLUpdateMaxInterval     time.Duration
	RLEarlyClose            bool
	RLClamps                conductor.Clamps
	MaxOpenDuration         time.Duration

	// ReconcileInterval is how often reconcile() polls GetPositions/GetBalance against this
	// process's own bookkeeping (CLAUDE.md §27.3/§27.6). Fixed at 1 minute by explicit operator
	// decision (2026-09-03) — not a tunable meant to be lowered; see ReconcileInterval's own
	// comment on why this is deliberately not a tight loop. Zero falls back to the constant below.
	ReconcileInterval time.Duration

	// ReconciledExternally tells Run not to start this engine's own reconciliation loop because a
	// ReconcileDriver is polling the account once on the whole roster's behalf. See AccountSnapshot
	// for why per-engine polling was the wrong shape.
	ReconciledExternally bool

	// FillTimeout bounds how long a placed market order (open or the flattening close order) is
	// given to fill before it's CANCELED and given up on (CLAUDE.md §27.5) — no automatic retry or
	// re-pricing; the next real signal on its own normal cadence is what tries again. Futures market
	// orders against a liquid perpetual are expected to fill essentially immediately, so this exists
	// to bound the rare case where one doesn't, not as the primary fill path. Zero falls back to
	// DefaultFillTimeout.
	FillTimeout time.Duration

	OrderEvents port.MarketDataPublisher

	// TradingPaused/OpensDisabled/DisableLong/DisableShort mirror PaperTrader's own panel
	// control-box fields exactly (CLAUDE.md real-trading readiness plan, 2026-09-04) — until this
	// was added, cmd/trader never read paper_trading_config at all, so the Real tab's Pause/Stop/
	// disable-long/disable-short/active-strategies/active-tokens controls had zero effect on real
	// trading despite appearing to save successfully. Existing open positions are always
	// unaffected by any of these — only evaluateStrategies' new-open path is gated, monitoring/
	// closing keeps running unconditionally, same reasoning as PaperTrader's own fields.
	TradingPaused bool
	// OpensDisabled is read through opensDisabled() rather than directly: the affordability service
	// (2026-09-08) can disable a token while this engine is already running, so a value captured
	// once at construction goes stale. Guarded by opensMu since that service writes it from its own
	// goroutine while the candle consumers read it.
	OpensDisabled bool
	opensMu       sync.RWMutex
	DisableLong   bool
	DisableShort  bool

	candlesMu sync.Mutex
	candles   map[string][]domain.Candle

	openMu sync.Mutex
	// openInFlight is set while an order is being placed on the exchange but is not yet visible in
	// the database, and reconcile treats an "untracked" remote position as untracked ONLY when it
	// is clear.
	//
	// Without it, opening a position halts all real trading (2026-09-13, CLAUDE.md §48). The window
	// is inherent rather than avoidable: Exchange.PlaceOrder is a blocking network call, the
	// exchange fills the position DURING it, and the local row cannot be written until the call
	// returns with an order id. Measured in production, that gap was ~1s — reconcile flagged DOGE
	// as untracked at 05:25:05.661 and the row was written at 05:25:06.675.
	//
	// The private WebSocket (§35.4) is what makes this reliably reproducible rather than rare: the
	// fill itself pushes an account-change event, which triggers a reconcile pass immediately, so
	// the poll lands inside the very window the fill opened.
	//
	// Guarded by openMu, which the open path already holds across the whole read-then-open sequence
	// (§16.9), so setting and clearing it needs no second lock.
	openInFlight bool
	// reconcileMu serializes reconciliation passes. Before the private WebSocket, reconcile had a
	// single caller on a fixed ticker and could not overlap with itself; now a pushed position
	// event can trigger a pass while the periodic one is mid-flight. Both read the local and remote
	// state and then act on the difference, so two concurrent passes could each observe the same
	// stale-open position and each close it — the same read-then-act race openMu guards on the open
	// path (CLAUDE.md §16.9).
	reconcileMu sync.Mutex

	// rlAdjustMu/lastRLAdjustAt throttle runUpdates to RLAdjustInterval cadence, mirroring
	// PaperTrader's identical throttle exactly (CLAUDE.md §15.9's MidPrice-freshness fix) — found
	// missing 2026-09-05 during the first real-trading activation: RealTrader.handleTick called
	// runUpdates (which queries real_orders via openPositions()) on EVERY tick with no throttle at
	// all, unlike PaperTrader's shouldRunRLAdjust gate. At OKX's live tick rate across 10
	// instruments this measured as a genuinely elevated, continuous Postgres query load (~65% CPU
	// on the timescaledb container) — not a slow query (0.14ms execution, correctly indexed), a
	// frequency problem.
	rlAdjustMu     sync.Mutex
	lastRLAdjustAt time.Time

	conductorOnce sync.Once
	lifecycle     *conductor.Conductor

	instrumentOnce sync.Once
	instrument     domain.Instrument
	instrumentErr  error
}

// shouldRunRLAdjust mirrors PaperTrader.shouldRunRLAdjust exactly: reports whether RLAdjustInterval
// has elapsed since the last update pass, and if so atomically claims the slot so concurrent ticks
// can't both pass the check and double-fire.
func (e *RealTrader) shouldRunRLAdjust() bool {
	e.rlAdjustMu.Lock()
	defer e.rlAdjustMu.Unlock()
	if time.Since(e.lastRLAdjustAt) < RLAdjustInterval {
		return false
	}
	e.lastRLAdjustAt = time.Now()
	return true
}

// asPaperOrderView converts a RealOrder into the port.PaperOrder shape the shared pure math
// functions (closeReason/realizedPnL/computeAdjustedLevels/positionStateOf/unrealizedPnLPct/
// RatchetSLTP, all in papertrade.go/rl_sltp_adjust.go/sltp_ratchet.go) are typed against —
// CLAUDE.md real-trading readiness plan, 2026-09-04's real_orders table split. Every field those
// functions actually read (Side/EntryPx/SLPx/TPPx/Size/Leverage/OpenedAt/PnLMaxPct/PnLMinPct) has
// an identical counterpart on RealOrder; Variant is fixed to "baseline" (a real order is never a
// shadow fork, §27.3) so positionStateOf's IsFork always reads false, matching reality. This is
// purely an in-memory adapter for reusing already-proven math — it is never itself persisted, and
// paper-trading's own code path is untouched by its existence.
func asPaperOrderView(o port.RealOrder) port.PaperOrder {
	return port.PaperOrder{
		ID:         o.ID,
		InstID:     o.InstID,
		StrategyID: o.StrategyID,
		Bar:        o.Bar,
		Side:       o.Side,
		EntryPx:    o.EntryPx,
		SLPx:       o.SLPx,
		TPPx:       o.TPPx,
		Size:       o.Size,
		Leverage:   o.Leverage,
		OpenedAt:   o.OpenedAt,
		PnLMaxPct:  o.PnLMaxPct,
		PnLMinPct:  o.PnLMinPct,
		Variant:    "baseline",
	}
}

// execInstID is the instId actually sent to the exchange — ExecInstID when set, else InstID
// (CLAUDE.md §27's real-account finding: an account may only have usable margin on a different
// OKX product than the one this engine's market data/observation identity uses).
func (e *RealTrader) execInstID() string {
	if e.ExecInstID != "" {
		return e.ExecInstID
	}
	return e.InstID
}

// execInstType is the instType used for GetPositions/GetInstrument calls — "SWAP" when
// ExecInstType is unset, matching this codebase's pre-2026-09-04 assumption for every deployment
// whose account trades the classic SWAP product directly.
func (e *RealTrader) execInstType() string {
	if e.ExecInstType != "" {
		return e.ExecInstType
	}
	return "SWAP"
}

// settleCcy is the currency used for GetBalance calls — "USDT" when SettleCcy is unset, matching
// this codebase's pre-2026-09-04 assumption.
func (e *RealTrader) settleCcy() string {
	if e.SettleCcy != "" {
		return e.SettleCcy
	}
	return "USDT"
}

// instrumentMeta fetches and caches execInstID's contract-shape metadata (CtVal/LotSz/MinSz) on
// first use — a real network call only once per process lifetime, not once per order, since an
// instrument's contract shape does not change while the process runs. A fetch failure is cached
// too (instrumentOnce fires exactly once regardless of outcome) rather than retried on every
// order: a real order must not silently fall back to an unconverted size if this call is broken,
// so callers treat a returned error as fatal to that order rather than proceeding with a guess.
func (e *RealTrader) instrumentMeta() (domain.Instrument, error) {
	e.instrumentOnce.Do(func() {
		e.instrument, e.instrumentErr = e.Exchange.GetInstrument(e.execInstType(), e.execInstID())
	})
	return e.instrument, e.instrumentErr
}

// sizeToContracts converts a desired notional (USD) at the given price into a valid contract
// count for execInstID: notional/price gives the base-unit size (e.g. BTC), divided by CtVal to
// get contracts, then rounded down to the nearest LotSz multiple — OKX rejects a size that isn't
// an exact multiple (51121, found live 2026-09-04). Rounding DOWN, never up, so a real order can
// never request more notional than what was actually sized/approved by the risk manager upstream.
// A CtVal of zero (unset/misconfigured instrument metadata) is treated as 1 — the pre-2026-09-04
// implicit assumption — rather than dividing by zero.
func sizeToContracts(marginUSD, leverage, price decimal.Decimal, inst domain.Instrument) decimal.Decimal {
	ctVal := inst.CtVal
	if !ctVal.IsPositive() {
		ctVal = decimal.NewFromInt(1)
	}
	// Margin x leverage is the POSITION's notional — the whole point of leverage is that $2 of
	// margin at 10x controls $20 of the instrument. Sizing off margin alone (the original bug,
	// found 2026-09-09) opened every real position at 1/leverage of its intended size: order 5
	// asked for $1.87 of margin at 9.44x, i.e. a $17.66 position, and got $1.03 — one contract
	// instead of seventeen.
	//
	// Three symptoms traced back to this one line, which is why it looked like several bugs:
	// realized PnL came in far under what the local formula predicted (the formula was right, the
	// position was 17x too small); expensive tokens fell below their own minimum contract value and
	// were declined at sizing time; and the affordability service then disabled five of them for a
	// budget ceiling that was never real.
	if !leverage.IsPositive() {
		leverage = decimal.NewFromInt(1)
	}
	notionalUSD := marginUSD.Mul(leverage)
	baseUnits := notionalUSD.Div(price)
	contracts := baseUnits.Div(ctVal)
	if inst.LotSz.IsPositive() {
		lots := contracts.Div(inst.LotSz).Floor()
		contracts = lots.Mul(inst.LotSz)
	}
	return contracts
}

// rawOrderFetcher is implemented by exchange clients that can return OKX's untouched order
// payload. Declared as an OPTIONAL capability rather than added to port.ExchangeClient so the
// several existing implementations (and every test fake) don't all have to grow a method only the
// record-capture path uses — a client without it simply captures nothing, which degrades the audit
// trail rather than the trade.
type rawOrderFetcher interface {
	GetOrderRaw(instID, ordID string) (json.RawMessage, error)
}

// captureExchangeRecord stores OKX's own record for one leg of an order ("open" or "close"), so
// the panel can show the full exchange-side truth later without a live API call (2026-09-09
// request). Called once per leg, at the point the leg has reached a terminal state and its record
// is final.
//
// Entirely best-effort: every failure path here logs and returns. An order's record is an audit
// nicety, and losing it must never affect the position it describes — which is also why this runs
// AFTER the close is durably recorded, never before.
func (e *RealTrader) captureExchangeRecord(ctx context.Context, orderID int64, leg, exchangeOrdID string, logger *slog.Logger) {
	if e.Repo == nil || orderID == 0 || exchangeOrdID == "" {
		return
	}
	fetcher, ok := e.Exchange.(rawOrderFetcher)
	if !ok {
		return
	}
	raw, err := fetcher.GetOrderRaw(e.execInstID(), exchangeOrdID)
	if err != nil {
		logger.Warn("could not capture exchange order record", "id", orderID, "leg", leg, "ordId", exchangeOrdID, "error", err)
		return
	}
	if err := e.Repo.SetRealOrderExchangeRaw(ctx, orderID, leg, raw); err != nil {
		logger.Warn("could not store exchange order record", "id", orderID, "leg", leg, "error", err)
	}
}

// marginFromFill converts the exchange's own filled contract count back into the margin figure
// this system stores as an order's Size — the inverse of sizeToContracts (2026-09-09 request:
// "if the size changes after the position opens, the database must be updated with the exchange's
// own position data at that moment").
//
// contracts x CtVal x fillPrice is the position's real notional; dividing by leverage gives the
// margin backing it. Returns zero when anything needed is missing or non-positive, so a caller can
// treat that as "no better number available" and keep what it had rather than overwriting a good
// value with a bad one.
func marginFromFill(status domain.OrderStatus, fillPx, leverage decimal.Decimal, inst domain.Instrument) decimal.Decimal {
	if !status.AccFillSz.IsPositive() || !fillPx.IsPositive() {
		return decimal.Zero
	}
	ctVal := inst.CtVal
	if !ctVal.IsPositive() {
		ctVal = decimal.NewFromInt(1)
	}
	if !leverage.IsPositive() {
		leverage = decimal.NewFromInt(1)
	}
	return status.AccFillSz.Mul(ctVal).Mul(fillPx).Div(leverage)
}

// instrumentOrZero returns the cached instrument metadata, or a zero value if it could not be
// fetched.
// Unlike instrumentMeta this never errors: its callers are correcting a stored figure after an
// order is already live, where a metadata failure should leave the existing value alone rather
// than abort anything.
func (e *RealTrader) instrumentOrZero() domain.Instrument {
	inst, err := e.instrumentMeta()
	if err != nil {
		return domain.Instrument{}
	}
	return inst
}

// DefaultReconcileInterval is the reconciliation poll's cadence when RealTrader.ReconcileInterval
// is unset.
//
// 20 seconds. It was set to 5 on 2026-09-09 (operator request) and raised on 2026-09-10 after that
// cadence produced OKX "50011 Too Many Requests" on /account/positions and /account/balance: this
// poll runs PER INSTRUMENT, so a 10-token roster issues 20 account-class calls every interval, and
// both endpoints are account-wide — every one of those calls fetches the same data.
//
// The rate limiting was not harmless. A reconciliation pass that cannot read positions cannot
// detect drift, and one that cannot read the protective order falls back to recording a close as
// manual — which is how real order 43 was mis-recorded minutes after the code to prevent exactly
// that had shipped.
//
// 20s is the compromise, not the ideal. The right fix is one account-wide poll shared across
// instruments rather than N identical ones; that is a larger change than a constant.
// The poll's job grew: it no longer only catches bookkeeping drift (a manual close on OKX's own
// UI, a liquidation), it also verifies that every open position's protective order is still
// resting on the exchange and re-places it when it is not. A minute of running unprotected is a
// long time on a leveraged position, and this is now the mechanism that bounds it.
//
// The rate-limit cost is real but modest and deliberately accounted for: two account-class calls
// (GetPositions, GetBalance) plus one per open position with a protective order to verify, against
// the gateway's account-class budget — and the trader is the gateway's priority consumer, so this
// cannot starve order placement (CLAUDE.md §27.1).
//
// The private WebSocket (positions/orders/account push) is the better primary for this and is
// wired separately; this poll remains as the backup that does not depend on a socket staying up.
const DefaultReconcileInterval = 20 * time.Second

func (e *RealTrader) reconcileInterval() time.Duration {
	if e.ReconcileInterval > 0 {
		return e.ReconcileInterval
	}
	return DefaultReconcileInterval
}

// DefaultFillTimeout is RealTrader.FillTimeout's fallback when unset — 60s, matching
// config.FillTimeout.OrderFillTimeoutSec's own default (CLAUDE.md §27.5).
const DefaultFillTimeout = 60 * time.Second

// fillPollInterval is how often waitForFill re-polls GetOrder while waiting — short relative to
// FillTimeout since a market order against a liquid perpetual is expected to fill within one or
// two polls; this isn't the reconciliation poll's "don't hammer the exchange" concern; it's a
// short, bounded wait for a single order's own outcome.
const fillPollInterval = 500 * time.Millisecond

func (e *RealTrader) fillTimeout() time.Duration {
	if e.FillTimeout > 0 {
		return e.FillTimeout
	}
	return DefaultFillTimeout
}

// waitForFill polls Exchange.GetOrder for ordID until it reaches a terminal state (filled or
// canceled) or fillTimeout() elapses, whichever comes first (CLAUDE.md §27.5). On timeout it
// CANCELS the order and returns the status as last observed — deliberately no retry or re-price;
// the caller (openReal/closeRealWith) is responsible for deciding what an unfilled/partially-
// filled result means for its own path. A GetOrder error mid-poll is logged and treated as "not
// yet terminal" rather than aborting the wait outright — a single flaky status read must not
// abandon an order that may still be filling normally.
// WaitForFillForTesting exports waitForFill for cmd/okx-apitest (CLAUDE.md's 2026-09-04 API-key
// diagnostic) to call the real production fill-timeout/cancel path directly, without pulling in
// the rest of RealTrader's strategy/conductor/Kafka machinery — the diagnostic must never risk
// opening a position on its own initiative. Behavior is identical to waitForFill; this is purely
// a visibility export, not a separate implementation.
func (e *RealTrader) WaitForFillForTesting(ctx context.Context, ordID string, logger *slog.Logger) (domain.OrderStatus, error) {
	return e.waitForFill(ctx, ordID, logger)
}

func (e *RealTrader) waitForFill(ctx context.Context, ordID string, logger *slog.Logger) (domain.OrderStatus, error) {
	deadline := time.Now().Add(e.fillTimeout())
	var last domain.OrderStatus
	for {
		status, err := e.Exchange.GetOrder(e.execInstID(), ordID)
		if err != nil {
			logger.Warn("fill-timeout: get order status failed, will retry", "instId", e.execInstID(), "ordId", ordID, "error", err)
		} else {
			last = status
			if status.IsTerminal() {
				return last, nil
			}
		}

		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(fillPollInterval):
		}
	}

	logger.Warn("fill-timeout: order not filled within timeout, canceling",
		"instId", e.execInstID(), "ordId", ordID, "timeout", e.fillTimeout(), "lastState", last.State)
	if err := e.Exchange.CancelOrder(e.execInstID(), ordID); err != nil {
		logger.Error("fill-timeout: cancel failed", "instId", e.execInstID(), "ordId", ordID, "error", err)
		return last, fmt.Errorf("order %s not filled within %s and cancel failed: %w", ordID, e.fillTimeout(), err)
	}
	return last, nil
}

// tradableEquityFor resolves how much of a raw exchange balance this engine may size against,
// preferring the operator's stored trading cap over the config-level SafeMoneyUSD reserve
// (2026-09-08). Both express the same split — tradable vs. held back — but from opposite sides,
// and the cap is the one the operator actually sets from the panel, so it has to win where both
// exist. Without this, zeroing safe_money_usd (now redundant) would silently let sizing draw
// against the FULL exchange balance rather than the chosen slice.
//
// The cap is read through the same repository row RecordExchangeBalance maintains, so the number
// sizing uses and the number the panel shows as Total Equity cannot disagree. A read failure falls
// back to the SafeMoneyUSD path rather than to the full balance: degrading toward the more
// conservative of the two is the only safe direction when the intended limit is unknown.
func (e *RealTrader) tradableEquityFor(ctx context.Context, rawBalance decimal.Decimal) decimal.Decimal {
	if e.Repo != nil {
		if ae, err := e.Repo.GetAccountEquity(ctx, e.accountMode(), rawBalance); err == nil && ae.TradingCapUSD != nil {
			// The reserve, not the cap itself: realized PnL accrues to the tradable slice, so a
			// balance that has grown since the cap was set must grow the tradable figure too
			// (CLAUDE.md: $20 of $40, then +$5, is $25 tradable — not $20 forever).
			reserve := ae.AccountBalanceUSD.Sub(ae.EquityUSD)
			tradable := rawBalance.Sub(reserve)
			if tradable.IsNegative() {
				return decimal.Zero
			}
			return tradable
		}
	}
	return e.tradableEquity(rawBalance)
}

// tradableEquity applies SafeMoneyUSD's reserve to a raw exchange balance — the fallback for an
// account with no explicit trading cap set. Floored at zero: a balance the reserve exceeds must
// never report as negative equity (which would read as the account being drained, not merely
// under the reserve).
func (e *RealTrader) tradableEquity(rawBalance decimal.Decimal) decimal.Decimal {
	if !e.SafeMoneyUSD.IsPositive() {
		return rawBalance
	}
	tradable := rawBalance.Sub(e.SafeMoneyUSD)
	if tradable.IsNegative() {
		return decimal.Zero
	}
	return tradable
}

// evenShareOfAccount mirrors PaperTrader.evenShareOfAccount: the fraction of the account one token
// is expected to take when equity is split evenly across the active roster, fed to the model as
// MaxPositionPct (observation schema v7). Derived from the live roster length rather than a config
// constant so enabling or disabling a token reshapes the budget on its own.
func (e *RealTrader) evenShareOfAccount() decimal.Decimal {
	count := len(e.ActiveTokens)
	if count <= 0 {
		count = 1
	}
	return decimal.NewFromInt(1).Div(decimal.NewFromInt(int64(count)))
}

// tighterPct returns the smaller of two position-size ceilings, ignoring either that is unset
// (non-positive). Used so the even-share budget and account.max_position_pct compose as two
// independent bounds rather than one replacing the other: a safety cap must only ever be able to
// tighten, never to loosen (the same rule §19.3 applies to the SL-distance bounds).
func tighterPct(a, b decimal.Decimal) decimal.Decimal {
	switch {
	case !a.IsPositive():
		return b
	case !b.IsPositive():
		return a
	case a.LessThan(b):
		return a
	default:
		return b
	}
}

// opensDisabled reports whether this token is currently excluded from opening new positions.
// Existing positions are unaffected either way — monitorOpenPositions keeps watching them.
func (e *RealTrader) opensDisabled() bool {
	e.opensMu.RLock()
	defer e.opensMu.RUnlock()
	return e.OpensDisabled
}

// SetOpensDisabled updates the per-token open gate on a RUNNING engine. Called by the
// affordability service when the account can no longer fund a minimum lot of this instrument (or
// can again), so a roster change takes effect immediately rather than at the next restart.
//
// Before this existed the flag was captured once at construction, so a token disabled seconds
// after startup kept opening positions until someone restarted the process — observed live with
// PUMP and PEPE, which were auto-disabled 1.5s after trader came up and went on generating open
// attempts for the next 20 minutes.
func (e *RealTrader) SetOpensDisabled(disabled bool) {
	e.opensMu.Lock()
	defer e.opensMu.Unlock()
	e.OpensDisabled = disabled
}

func (e *RealTrader) accountMode() string {
	if e.Mode == "" {
		return "real"
	}
	return e.Mode
}

func (e *RealTrader) decisionBar() string {
	return decisionBarFor("", e.Bars)
}

func (e *RealTrader) marketView(bar string) strategy.MarketView {
	return snapshotCandles(&e.candlesMu, e.candles, bar)
}

func (e *RealTrader) seedCandlesFromRepo(ctx context.Context, logger *slog.Logger) {
	seedCandlesFromRepo(ctx, &e.candlesMu, e.candles, e.Repo, e.InstID, e.Bars, e.CandleWindow, logger)
}

func (e *RealTrader) conductor() *conductor.Conductor {
	e.conductorOnce.Do(func() {
		e.lifecycle = conductor.New(conductor.Config{
			UpdatePnLThresholdPct: e.RLUpdatePnLThresholdPct,
			UpdateMaxInterval:     e.RLUpdateMaxInterval,
			AllowEarlyClose:       e.RLEarlyClose,
			Clamps:                e.RLClamps,
			MaxOpenDuration:       e.MaxOpenDuration,
		})
	})
	return e.lifecycle
}

func (e *RealTrader) conductorClamps() conductor.Clamps {
	return e.RLClamps
}

// Run consumes ticks/candles from the event bus and drives the reconciliation poll, until ctx is
// cancelled. Mirrors PaperTrader.Run's shape (CLAUDE.md §27's plan, tickfeed.go).
func (e *RealTrader) Run(ctx context.Context) error {
	logger := e.Logger
	if logger == nil {
		logger = slog.Default()
	}

	e.candlesMu.Lock()
	e.candles = make(map[string][]domain.Candle, len(e.Bars))
	e.candlesMu.Unlock()

	e.seedCandlesFromRepo(ctx, logger)

	errCh := make(chan error, 2+len(e.CandleConsumers))
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
	// Skipped when a ReconcileDriver owns this engine's reconciliation (2026-09-10): the driver
	// runs ONE account-wide pass for the whole roster rather than each engine polling the same
	// account-scoped endpoints independently. Left in place otherwise so a RealTrader run on its
	// own is still reconciled — dropping the loop outright would make an engine's safety depend on
	// a caller remembering to wire a driver.
	if !e.ReconciledExternally {
		go func() {
			errCh <- e.runReconcileLoop(ctx, logger)
		}()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (e *RealTrader) handleTick(ctx context.Context, data []byte, logger *slog.Logger) error {
	price, ok, err := decodeTick(data, e.InstID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := e.monitorOpenPositions(ctx, price, logger); err != nil {
		return err
	}
	// Throttled to RLAdjustInterval (CLAUDE.md §15.9's freshness fix, mirrored from PaperTrader) —
	// runUpdates queries real_orders and, when a position is actually due for a decision, calls the
	// model; running it on every single tick against 10 live instruments produced a continuous,
	// unnecessary Postgres query load with no benefit (found 2026-09-05 during first activation).
	if e.shouldRunRLAdjust() {
		e.runUpdates(ctx, e.decisionBar(), price, logger)
	}
	return nil
}

func (e *RealTrader) handleCandle(ctx context.Context, bar string, data []byte, logger *slog.Logger) error {
	dc, ok, err := decodeCandle(data, e.InstID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	c := dc.Candle
	applyCandle(&e.candlesMu, e.candles, bar, c, e.CandleWindow)
	if !dc.Confirmed {
		return nil
	}
	// Unlike PaperTrader, RealTrader is never the only writer of a bar's candles — cmd/paper-trader
	// (or another RealTrader instance sharing this instrument) already persists them. Re-saving here
	// would just be a redundant upsert on the same (inst_id, bar, ts) key, so this deliberately does
	// NOT call Repo.SaveCandle.
	return e.evaluateStrategies(ctx, bar, c.Close, logger)
}

// openPositions returns this instrument's currently-open real positions from real_orders — its
// own table (CLAUDE.md real-trading readiness plan, 2026-09-04), never paper_orders/ListPositions,
// so paper trading running concurrently on the same instrument can never be mistaken for a real
// position. ListRealPositions' f.Open=true filter already excludes still-pending orders (an
// in-flight fill is not yet a position, per port.RealOrder.Status's doc comment).
func (e *RealTrader) openPositions(ctx context.Context) ([]port.RealOrder, error) {
	open := true
	return e.Repo.ListRealPositions(ctx, port.PositionFilter{InstID: e.InstID, Open: &open})
}

// hasOpenPosition reports whether any of open is a real position — always true for a non-empty
// ListRealPositions(Open:true) result, but kept as a named check (mirroring PaperTrader's
// hasOpenBaseline) so the "is this token occupied" question reads the same way at both call sites.
func hasOpenPosition(open []port.RealOrder) bool {
	return len(open) > 0
}

func (e *RealTrader) evaluateStrategies(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) error {
	if halted, reason := e.RiskManager.Halted(); halted {
		logger.Warn("real trading halted, skipping new opens", "instId", e.InstID, "reason", reason)
		return nil
	}

	// Panel control-box gate, mirroring PaperTrader.evaluateStrategies exactly (CLAUDE.md
	// real-trading readiness plan, 2026-09-04): pause/stop and per-token disable both mean "open
	// nothing new here" — existing open positions are untouched, monitorOpenPositions keeps
	// monitoring/closing them regardless of either flag.
	if e.TradingPaused || e.opensDisabled() {
		return nil
	}

	// Serializes the whole read-open-then-maybe-open sequence against the other bars' consumer
	// goroutines — same race PaperTrader.openMu guards against (CLAUDE.md §16.9).
	e.openMu.Lock()
	defer e.openMu.Unlock()

	// Suppress reconcile's untracked-position halt for the duration, since an order placed here is
	// live on the exchange before its local row exists (§48). Cleared on every return path,
	// including an early return or a panic, so a bug in the open path can never leave the check
	// permanently disabled — which would be strictly worse than the halt it prevents.
	e.setOpenInFlight(true)
	defer e.setOpenInFlight(false)

	open, err := e.openPositions(ctx)
	if err != nil {
		return fmt.Errorf("list open real positions: %w", err)
	}

	view := e.marketView(bar)

	for _, a := range e.Strategies {
		if a.Bar != bar {
			continue
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
		// Panel control-box long/short toggle, mirroring PaperTrader (CLAUDE.md real-trading
		// readiness plan, 2026-09-04): the strategy still evaluates and its signal is still
		// counted in the metric above, this only gates whether it's acted on.
		if (e.DisableLong && signal.Side == strategy.Buy) || (e.DisableShort && signal.Side == strategy.Sell) {
			continue
		}

		// One open position per token per side (CLAUDE.md §27.3): in net PosMode this means at most
		// one position of ANY side, so a signal while one is already open is never a flip — it's
		// routed to runUpdates as an `update` instead, exactly like PaperTrader's own
		// hasOpenBaseline gate. Hedge (long_short) mode's per-side gating is left for when the
		// open/close paths actually need to key by side; net mode is what's configured today.
		if hasOpenPosition(open) {
			continue
		}

		resolved := signal.ResolveLevels(price)
		e.conductor().RetainSignal(e.InstID, bar, domain.StrategySignal{
			StrategyID: a.StrategyID,
			Side:       string(signal.Side),
			Confidence: signal.Confidence,
			EntryPx:    resolved.EntryPx,
			SLPx:       resolved.SLPx,
			TPPx:       resolved.TPPx,
			Kind:       a.Kind,
			Bar:        a.Bar,
		})

		obs := e.buildObservation(ctx, bar, price, logger)
		obs.Category = conductor.OpenCategory(string(signal.Side))
		obs.Signal = e.carriedSignalFor(bar)

		// resolved, not the raw signal: buildPaperOrder inside openReal derives levels from
		// SLPct/TPPct only, so a strategy reporting a STRUCTURAL price instead (7 of the 14 do —
		// CLAUDE.md §16.8: a stop below a swing low, a target at a fair-value gap) had its level
		// silently discarded on every real order. ResolveLevels carries both forms, filling in
		// whichever was not set, so passing it preserves a structural level all the way to the
		// order while leaving percentage-based strategies unchanged.
		opened, err := e.openReal(ctx, obs, resolved, open, price, a, bar, logger)
		if err != nil {
			logger.Error("failed to open real order", "strategy", s.Name(), "instId", e.InstID, "error", err)
			continue
		}
		if opened == nil {
			continue // model declined, or the signal produced no usable levels
		}
		open = append(open, *opened)
	}
	return nil
}

// openReal asks the model whether to take signal, sizes/clamps the result, places the real order,
// and persists the row. Returns nil, nil when the signal was declined (model skip, or no usable
// stop) rather than an error — declining is a normal outcome, not a failure.
func (e *RealTrader) openReal(
	ctx context.Context,
	obs domain.Observation,
	signal strategy.Signal,
	openOrders []port.RealOrder,
	price decimal.Decimal,
	a StrategyAssignment,
	bar string,
	logger *slog.Logger,
) (*port.RealOrder, error) {
	category := conductor.OpenCategory(string(signal.Side))
	if category == "" || e.Model == nil {
		return nil, nil
	}
	obs.Category = category

	action, err := e.Model.Predict(ctx, obs)
	if err != nil {
		logger.Warn("real open: predict failed", "instId", e.InstID, "error", err)
		return nil, nil
	}
	if action.Action == domain.ActionSkip {
		logger.Info("real open: model declined the signal", "instId", e.InstID, "side", signal.Side)
		return nil, nil
	}
	if action.Action != domain.ActionOpen {
		logger.Info("real open: model gave no open/skip answer, declining to trade without one",
			"instId", e.InstID, "action", action.Action)
		return nil, nil
	}

	// The per-position ceiling is the even share of the account across the active roster
	// (equity/tokenCount), NOT account.max_position_pct alone (2026-09-08 request): splitting the
	// budget evenly is what keeps one token from consuming several tokens' worth of risk, which a
	// flat 25% ceiling on a 10-token roster does not — it would let four positions commit the
	// entire account.
	//
	// Whichever is TIGHTER wins, so account.max_position_pct keeps working as the hard backstop it
	// was written to be (§15.6) and can only ever make the budget smaller, never larger. On a
	// roster of 1-3 tokens the even share is the looser of the two and the config cap binds; from
	// 4 tokens up the even share binds. This also makes the ceiling the model is TOLD about
	// (buildObservation's MaxPositionPct, already the even share) the same number it is actually
	// held to — before this they disagreed, advising 1/10 while permitting 1/4.
	cfg := sizingConfig{
		InstID:              e.InstID,
		MaxLeverage:         e.MaxLeverage,
		MaxPositionPct:      tighterPct(e.evenShareOfAccount(), e.MaxPositionPct),
		MaxTotalExposurePct: e.MaxTotalExposurePct,
	}
	openOrdersView := make([]port.PaperOrder, len(openOrders))
	for i, o := range openOrders {
		openOrdersView[i] = asPaperOrderView(o)
	}
	notional, leverage, sized := sizeFromModelAction(cfg, action, obs, openOrdersView, logger)
	if !sized {
		logger.Info("real open: model action not sizable, declining", "instId", e.InstID)
		return nil, nil
	}

	paperShaped := buildPaperOrder(e.InstID, price, signal, notional, a.StrategyID, bar)
	order := port.RealOrder{
		InstID:     paperShaped.InstID,
		StrategyID: paperShaped.StrategyID,
		Bar:        paperShaped.Bar,
		Side:       paperShaped.Side,
		EntryPx:    paperShaped.EntryPx,
		SLPx:       paperShaped.SLPx,
		TPPx:       paperShaped.TPPx,
		Size:       paperShaped.Size,
		Leverage:   leverage,
		Status:     "pending",
	}
	// The model overrides the strategy's levels only where it actually produced one. Each side is
	// taken independently and a zero is NOT an override: an untrained-ish policy routinely emits a
	// stop but no target (observed on the first real order, id 3 — it opened with a stop from the
	// model and no take-profit at all, because nothing downstream re-supplies a missing target the
	// way EnsureStop re-supplies a missing stop). Falling back per-side keeps the strategy's own
	// target in that case instead of dropping it on the floor.
	if levels := nonZeroLevels(action.SLPx, action.TPPx); levels.SLPx != nil {
		order.SLPx = levels.SLPx
	}
	if levels := nonZeroLevels(action.SLPx, action.TPPx); levels.TPPx != nil {
		order.TPPx = levels.TPPx
	}

	// Same validation pass as PaperTrader.evaluateStrategies, on EVERY open regardless of what
	// shaped the levels (CLAUDE.md §16.9 — a safety check reachable only through an optional
	// subsystem is not a safety check). Runs BEFORE the risk-manager gate: clamps answer "is this
	// stop/target sane relative to entry," the risk manager answers "does this violate an
	// account-wide hard limit regardless of what any upstream layer decided" (§5 of the plan doc).
	clampedLevels := e.conductorClamps().Apply(order.Side, price, order.Leverage, conductor.Levels{SLPx: order.SLPx, TPPx: order.TPPx})
	// EnsureStop runs AFTER Apply, not before (fixed 2026-09-08). Apply DROPS a level on the wrong
	// side of entry, and a strategy's stop is routinely on the wrong side by the time the order
	// actually opens: the signal is computed on a closed candle and the live price has moved since,
	// so a long whose price has slipped below the signal's stop arrives with a stop ABOVE entry.
	// EnsureStop only fills a level that is nil, so running it first left that stale stop in place,
	// Apply then dropped it, and the open was refused with "no stop-loss" — observed rejecting
	// every PUMP signal for 20 minutes straight while the strategy was emitting a perfectly good
	// stop each time.
	//
	// Running it after means Apply's drop is what EnsureStop then repairs, which is the order these
	// two were always meant to compose in: Apply decides what is usable, EnsureStop guarantees a
	// stop exists.
	clampedLevels = e.conductorClamps().EnsureStop(order.Side, price, order.Leverage, clampedLevels)
	if clampedLevels.SLPx == nil {
		logger.Error("refusing to open a real position with no stop-loss", "instId", e.InstID, "side", order.Side)
		return nil, nil
	}
	// A position with no target never takes profit on its own: RealTrader watches SL/TP in-process
	// (§3a) and simply has nothing to watch for on the winning side, so the trade can only ever end
	// at its stop, at the 6h timeout, or by hand. That is strictly worse than a wrong-but-present
	// target, so a missing one is derived from the stop's own distance via MinTPSLRatio — the same
	// risk:reward the clamp already enforces when both levels exist. Mirrors EnsureStop's
	// "fill it in rather than refuse" posture; refusing here would instead silently disable every
	// signal whose model answer omitted a target.
	clampedLevels = e.conductorClamps().EnsureTarget(order.Side, price, clampedLevels)
	order.SLPx, order.TPPx = clampedLevels.SLPx, clampedLevels.TPPx
	if order.TPPx == nil {
		logger.Warn("opening a real position with no take-profit", "instId", e.InstID, "side", order.Side)
	}

	approved, err := e.RiskManager.Approve(risk.ProposedAction{
		Leverage:             order.Leverage,
		PositionNotionalUSD:  signedNotional(order.Side, order.Size),
		LiquidationBufferPct: liquidationBufferEstimate(order.Leverage),
	})
	if err != nil {
		logger.Warn("real open rejected by risk manager", "instId", e.InstID, "error", err)
		return nil, nil
	}
	order.Leverage = approved.Leverage
	order.Size = approved.PositionNotionalUSD.Abs()
	if order.Size.IsZero() {
		return nil, nil
	}

	if err := e.setLeverageIfNeeded(order.Leverage, order.Side, logger); err != nil {
		return nil, fmt.Errorf("set leverage: %w", err)
	}

	inst, err := e.instrumentMeta()
	if err != nil {
		return nil, fmt.Errorf("fetch instrument metadata: %w", err)
	}
	sz := sizeToContracts(order.Size, order.Leverage, price, inst)
	if sz.IsZero() || (inst.MinSz.IsPositive() && sz.LessThan(inst.MinSz)) {
		logger.Info("real open: sized order below instrument minimum, declining",
			"instId", e.execInstID(), "sz", sz, "minSz", inst.MinSz)
		return nil, nil
	}
	req := domain.OrderRequest{InstID: e.execInstID(), TdMode: e.TdMode, Side: order.Side, OrdType: "market", Sz: sz}
	if e.PosMode == "long_short" {
		req.PosSide = posSideFor(signedNotional(order.Side, order.Size))
	}
	result, err := e.Exchange.PlaceOrder(req)
	if err != nil {
		return nil, fmt.Errorf("place order: %w", err)
	}
	if result != nil && result.SCode != "0" {
		return nil, fmt.Errorf("order rejected: sCode=%s sMsg=%s", result.SCode, result.SMsg)
	}
	if result != nil && result.OrdID != "" {
		ordID := result.OrdID
		order.ExchangeOrderID = &ordID
	}

	// Insert the pending row IMMEDIATELY after the exchange accepts the order, before waiting for
	// its fill — CLAUDE.md real-trading readiness plan, 2026-09-04. This is the one behavior change
	// with real product consequence: the order is now visible on the panel (status="pending") for
	// the whole in-flight window, not only after waitForFill resolves. An insert failure here is
	// logged and does NOT block the order — it is already live on the exchange, and losing
	// visibility into it must never mean losing track of it entirely (the reconciliation poll would
	// still catch a truly untracked position).
	var localID int64
	persisted := false
	id, err := e.Repo.OpenRealOrder(ctx, order)
	if err != nil {
		logger.Error("failed to persist pending real order; continuing since the exchange order is already live",
			"instId", e.InstID, "exchangeOrderId", order.ExchangeOrderID, "error", err)
	} else {
		localID = id
		persisted = true
		order.ID = localID
	}

	// CLAUDE.md §27.5: confirm the fill rather than trusting PlaceOrder's acceptance response alone
	// — a market order against a liquid perpetual is expected to fill essentially immediately, but
	// this must not be assumed.
	var finalStatus string
	if order.ExchangeOrderID != nil {
		// Mark the open IN FLIGHT before waiting on it, so a trader watching the panel sees the
		// position appear as "opening" the moment the model asks for it, rather than only once it
		// resolves seconds later (2026-09-08 request). Best-effort: this is visibility, and losing
		// it must not abort an open the exchange has already accepted.
		if persisted {
			if err := e.Repo.UpdateRealOrderStatus(ctx, localID, "opening", nil, nil, nil); err != nil {
				logger.Warn("failed to mark real order opening", "id", localID, "error", err)
			}
		}
		status, err := e.waitForFill(ctx, *order.ExchangeOrderID, logger)
		if err != nil {
			// Record it on the row so the panel can raise it to a human: the order IS live on the
			// exchange and its fill is simply unconfirmed, which is exactly the state someone needs
			// to look at rather than find in a log later.
			if persisted {
				if setErr := e.Repo.SetRealOrderError(ctx, localID, fmt.Sprintf("wait for fill: %v", err)); setErr != nil {
					logger.Warn("failed to record open error", "id", localID, "error", setErr)
				}
			}
			return nil, fmt.Errorf("wait for fill: %w", err)
		}
		switch {
		case status.IsFilled():
			finalStatus = "filled"
			if status.AvgPx.IsPositive() {
				order.EntryPx = status.AvgPx
			}
			// Re-derive margin from what the exchange ACTUALLY filled, not what was requested
			// (2026-09-09 request). sizeToContracts floors to a whole lot, so the filled position is
			// almost never exactly the requested notional — order 5 asked for $17.66 and 17
			// contracts is $17.50. Storing the request rather than the fill is what made the local
			// PnL disagree with the exchange's own even on a clean full fill, and it is the number
			// every downstream consumer (PnL, exposure caps, the model's observation) reads as "how
			// big is this position".
			if filled := marginFromFill(status, order.EntryPx, order.Leverage, e.instrumentOrZero()); filled.IsPositive() {
				order.Size = filled
			}
			// The contract count the exchange actually filled. This — not a size re-derived from a
			// price — is what the flatten closes.
			if status.AccFillSz.IsPositive() {
				filledSz := status.AccFillSz
				order.Contracts = &filledSz
			}
		case status.AccFillSz.IsPositive():
			// Partially filled within the timeout window: a real, smaller-than-intended position
			// exists on the exchange (canceled by waitForFill's timeout path for the remainder), so
			// record what actually filled rather than the originally requested size — never assume
			// the unfilled remainder will complete after the order was just canceled.
			finalStatus = "partial"
			logger.Warn("real open: order partially filled before timeout/cancel",
				"instId", e.InstID, "ordId", *order.ExchangeOrderID,
				"requestedSz", sz, "filledSz", status.AccFillSz)
			if status.AvgPx.IsPositive() {
				order.EntryPx = status.AvgPx
			}
			// Derived from the contracts actually filled, the same way the full-fill branch does,
			// rather than scaling the requested margin by a fill ratio — both reach the same number
			// on a clean fill, but deriving from contracts is correct even when the request itself
			// was rounded, and keeps one definition of "size" instead of two.
			if filled := marginFromFill(status, order.EntryPx, order.Leverage, e.instrumentOrZero()); filled.IsPositive() {
				order.Size = filled
			} else {
				order.Size = order.Size.Mul(status.AccFillSz).Div(sz)
			}
			filledSz := status.AccFillSz
			order.Contracts = &filledSz
		default:
			// Never filled at all before the timeout — canceled. The pending row STAYS (per the
			// plan's design: a timed-out attempt is still visible, not silently dropped) with
			// status="canceled"; no position was opened, so the caller returns nil, nil exactly as
			// it did before this table existed.
			finalStatus = "canceled"
			logger.Info("real open: order canceled unfilled, no position opened",
				"instId", e.InstID, "ordId", *order.ExchangeOrderID)
		}
	} else {
		// No exchange order id at all (PlaceOrder returned no OrdID) — nothing to wait on; treat as
		// filled immediately, matching the pre-fill-confirmation behavior for this edge case.
		finalStatus = "filled"
	}

	if raw, err := json.Marshal(obs); err == nil {
		order.FeaturesJSON = raw
	} else {
		logger.Warn("failed to marshal decision-time observation", "instId", e.InstID, "error", err)
	}

	if persisted {
		var entryPxPtr, sizePtr, contractsPtr *decimal.Decimal
		if finalStatus == "filled" || finalStatus == "partial" {
			entryPxPtr, sizePtr = &order.EntryPx, &order.Size
			contractsPtr = order.Contracts
		}
		if err := e.Repo.UpdateRealOrderStatus(ctx, localID, finalStatus, entryPxPtr, sizePtr, contractsPtr); err != nil {
			logger.Warn("failed to update real order status", "id", localID, "instId", e.InstID, "status", finalStatus, "error", err)
		}
		if err := e.Repo.SetRealOrderFeatures(ctx, localID, order.FeaturesJSON); err != nil {
			logger.Warn("failed to set real order features", "id", localID, "instId", e.InstID, "error", err)
		}
		// The open order has reached a terminal state here, so its exchange record is final —
		// capture it once now rather than re-fetching it on every later view (2026-09-09).
		if order.ExchangeOrderID != nil {
			e.captureExchangeRecord(ctx, localID, "open", *order.ExchangeOrderID, logger)
		}
	}
	order.Status = finalStatus

	if finalStatus == "canceled" {
		return nil, nil
	}

	// Rest the stop and target on the EXCHANGE before treating this position as open (2026-09-09
	// request: "we should set sl/tp on exchange always"). Until this existed the levels lived only
	// in real_orders, watched by this process's own tick monitor — so any interruption of this
	// service left real capital running unprotected, which is what real order 33 exposed.
	//
	// A position that cannot be protected is CLOSED again immediately rather than kept. That costs
	// a round-trip fee on a rare failure; holding an unprotected real position costs an unbounded
	// loss, and "keep it and hope the retry works" is exactly the posture this change removes.
	// Note this runs even when the DB insert failed (persisted == false). The position is LIVE on
	// the exchange either way, and a live position is exactly what must not go unprotected — losing
	// our own record of it is a bookkeeping problem, running it without a stop is a capital one.
	// placeProtection skips only the algoId write in that case, which openReal has nowhere to store
	// anyway.
	{
		algoID, protErr := e.placeProtection(ctx, order, logger)
		if protErr != nil {
			metrics.RealUnprotectedClosedTotal.WithLabelValues(e.InstID).Inc()
			logger.Error("could not rest sl/tp on the exchange for a just-opened real position; closing it immediately",
				"id", order.ID, "instId", e.InstID, "error", protErr)
			if closeErr := e.closeReal(ctx, order, price, conductor.CloseReasonManual, logger); closeErr != nil {
				// Now genuinely dangerous: an unprotected position that also would not flatten.
				// Halt so nothing new is opened alongside it and a human is drawn to it — the same
				// escalation reconcile uses for an untracked position.
				logger.Error("FAILED TO CLOSE AN UNPROTECTED REAL POSITION; halting real trading",
					"id", order.ID, "instId", e.InstID, "error", closeErr)
				if e.RiskManager != nil {
					e.RiskManager.Halt(fmt.Sprintf("unprotected real position %d could not be closed: %v", order.ID, closeErr))
				}
			}
			return nil, nil
		}
		order.ExchangeAlgoOrderID = &algoID
	}

	metrics.PaperOrdersOpenedTotal.WithLabelValues(a.Kind, e.InstID, order.Side).Inc()
	logger.Info("opened real order", "id", order.ID, "instId", e.InstID, "side", order.Side,
		"entry", order.EntryPx, "size", order.Size, "leverage", order.Leverage, "exchangeOrderId", order.ExchangeOrderID)
	e.publishOrderEvent(ctx, "opened", order.ID, logger)

	return &order, nil
}

// setLeverageIfNeeded calls SetLeverage unconditionally on open — RealTrader has no cheap prior
// leverage to compare against the way trade.go's Trader does (it reads the exchange's current
// position leverage every poll; RealTrader only calls GetPositions from the reconciliation poll,
// not on every open) — an extra SetLeverage call when the value happens to already match is a
// harmless no-op on OKX's side, not worth threading additional state to avoid.
func (e *RealTrader) setLeverageIfNeeded(leverage decimal.Decimal, side string, logger *slog.Logger) error {
	if !leverage.IsPositive() {
		return nil
	}
	req := domain.LeverageChange{InstID: e.execInstID(), Lever: leverage, MgnMode: e.TdMode}
	if e.PosMode == "long_short" {
		req.PosSide = posSideFor(signedNotionalForSide(side))
	}
	return e.Exchange.SetLeverage(req)
}

func signedNotional(side string, notional decimal.Decimal) decimal.Decimal {
	if side == "sell" {
		return notional.Neg()
	}
	return notional
}

func signedNotionalForSide(side string) decimal.Decimal {
	if side == "sell" {
		return decimal.NewFromInt(-1)
	}
	return decimal.NewFromInt(1)
}

// liquidationBufferEstimate mirrors trade.go's execute() conservative 100/leverage approximation
// (CLAUDE.md §27.2) — ignores maintenance margin, same accepted simplification as the existing live
// path. RealTrader has no prior position's LiqPx/MarkPx to cross-check against at OPEN time (that
// cross-check only makes sense once a position exists, per §27.2's own note) — the reconciliation
// poll (reconcile) is where an already-open position's real liquidation distance gets checked
// against what OKX reports.
func liquidationBufferEstimate(leverage decimal.Decimal) decimal.Decimal {
	if !leverage.IsPositive() {
		return decimal.Zero
	}
	return decimal.NewFromInt(100).Div(leverage)
}

func nonZeroLevels(slPx, tpPx decimal.Decimal) conductor.Levels {
	return conductor.Levels{SLPx: nonZeroPx(slPx), TPPx: nonZeroPx(tpPx)}
}

// runUpdates is RealTrader's in-trade half of the lifecycle (CLAUDE.md §15.12), mirroring
// PaperTrader.runUpdates. Purely local per §3a's correction: no exchange call for an SL/TP move,
// since RealTrader watches SL/TP itself rather than resting a conditional order on OKX.
func (e *RealTrader) runUpdates(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) {
	open, err := e.openPositions(ctx)
	if err != nil {
		logger.Warn("real updates: list open positions failed", "instId", e.InstID, "error", err)
		return
	}
	if len(open) == 0 {
		return
	}
	if e.Model == nil {
		return
	}

	obs := e.buildObservation(ctx, bar, price, logger)
	obs.Category = domain.CategoryUpdate
	now := time.Now()

	for _, o := range open {
		// An operator's manual SL/TP edit (handleAdjustPosition) locks this order out of the model's
		// update loop entirely (explicit request, 2026-09-06): the model is never even asked about
		// it again, so it can neither move the levels a second time nor close the position early
		// (rl_early_close). A manual correction must stick, not be silently overwritten or
		// second-guessed by the next call.
		if o.ManualOverride {
			continue
		}
		// A position still being opened has no resting protective order yet, so any level the model
		// proposed could not be pushed to the exchange and applyRealAdjustment would refuse it
		// anyway (2026-09-09). Skipping here avoids spending a model call on a decision that cannot
		// be carried out, and avoids logging an alarming "no resting order to amend" for what is
		// just an order that has not finished filling.
		if o.Status != "filled" && o.Status != "partial" {
			continue
		}

		pnl := unrealizedPnLPct(asPaperOrderView(o), price)
		if !e.conductor().ShouldUpdate(o.ID, pnl, now) {
			continue
		}

		obs.OrderID = o.ID
		obs.PositionState = positionStateOf(asPaperOrderView(o), price)
		obs.Signal = e.carriedSignalFor(bar)

		action, err := e.Model.Predict(ctx, obs)
		if err != nil {
			logger.Warn("real updates: predict failed", "instId", e.InstID, "orderId", o.ID, "error", err)
			continue
		}

		switch action.Action {
		case domain.ActionClose:
			e.closeEarly(ctx, o, price, logger)
		case domain.ActionUpdate:
			e.applyRealAdjustment(ctx, o, action, price, logger)
		}
	}
}

// applyRealAdjustment is RealTrader's no-fork SL/TP edit: computeAdjustedLevels (the shared free
// function, plan commit 2) is the SAME ratchet-checked computation PaperTrader.applyAdjustment
// uses, but the IO here writes to real_orders/real_order_adjustments (CLAUDE.md real-trading
// readiness plan, 2026-09-04's table split). No exchange call: the new levels take effect on this
// engine's own next tick via monitorOpenPositions.
func (e *RealTrader) applyRealAdjustment(ctx context.Context, o port.RealOrder, action *domain.Action, price decimal.Decimal, logger *slog.Logger) {
	newSL, newTP, changed := computeAdjustedLevels(asPaperOrderView(o), action, price)
	if !changed {
		return
	}
	// The EXCHANGE is amended first, and a failure aborts the whole adjustment (2026-09-09).
	// Ordering matters: if the local row were written first and the amend then failed, the exchange
	// would still be holding the OLD stop while this system believed the new one was in force —
	// a silent divergence, and the more dangerous direction of the two, since the level actually
	// protecting real money would be the one nobody was looking at. Leaving both at the old level
	// is a coherent state; the next adjustment simply tries again.
	if err := e.amendProtection(ctx, o, newSL, newTP, logger); err != nil {
		logger.Error("real updates: could not move the exchange's resting sl/tp; leaving levels unchanged",
			"instId", e.InstID, "orderId", o.ID, "error", err)
		return
	}
	if err := e.Repo.UpdateRealOrderSLTP(ctx, o.ID, newSL, newTP, false); err != nil {
		logger.Warn("real updates: sl/tp update failed", "instId", e.InstID, "orderId", o.ID, "error", err)
		return
	}
	if !samePriceOrNil(newSL, o.SLPx) {
		if err := e.Repo.RecordRealOrderAdjustment(ctx, o.ID, "sl", o.SLPx, newSL, "model"); err != nil {
			logger.Warn("real updates: record sl adjustment failed", "instId", e.InstID, "orderId", o.ID, "error", err)
		}
	}
	if !samePriceOrNil(newTP, o.TPPx) {
		if err := e.Repo.RecordRealOrderAdjustment(ctx, o.ID, "tp", o.TPPx, newTP, "model"); err != nil {
			logger.Warn("real updates: record tp adjustment failed", "instId", e.InstID, "orderId", o.ID, "error", err)
		}
	}
	logger.Info("real sl/tp adjustment applied", "instId", e.InstID, "orderId", o.ID, "newSL", newSL, "newTP", newTP)
}

// closeEarly closes a real position at market because the model asked to (CLAUDE.md §15.12), gated
// on RLEarlyClose — the one lifecycle action that destroys the counterfactual, and on a real
// account one that also spends a real fee to do it.
//
// For real trading that gate is trading.allow_rl_early_close, its OWN switch rather than the
// paper_trading flag every other RL setting is shared with (2026-09-08 request), so enabling early
// close for paper research cannot silently enable it against real capital.
//
// An ignored request is LOGGED and COUNTED rather than dropped in silence: the model still made
// the decision, and "how often does it want out early, and was it right?" is exactly the evidence
// needed to decide whether to ever turn this on. The position simply runs to its own SL/TP or
// timeout instead.
func (e *RealTrader) closeEarly(ctx context.Context, o port.RealOrder, price decimal.Decimal, logger *slog.Logger) {
	if !e.RLEarlyClose {
		metrics.RealEarlyCloseIgnoredTotal.WithLabelValues(e.InstID).Inc()
		logger.Info("real updates: model asked to close early, ignored (trading.allow_rl_early_close is off)",
			"instId", e.InstID, "orderId", o.ID, "price", price)
		return
	}
	if err := e.closeReal(ctx, o, price, conductor.CloseReasonRLEarly, logger); err != nil {
		logger.Error("real updates: early close failed", "instId", e.InstID, "orderId", o.ID, "error", err)
	}
}

// monitorOpenPositions is RealTrader's SL/TP-touch and timeout check — the SAME in-process
// tick-driven mechanism PaperTrader.monitorOpenOrders already uses (CLAUDE.md §27.3's correction:
// no OKX conditional/algo order, this process watches its own open positions on every tick).
// trackPnLExtremes advances an open real position's peak/trough unrealized PnL, mirroring
// PaperTrader.trackPnLExtremes exactly (CLAUDE.md §15.11) against real_orders instead of
// paper_orders. Only writes when a new extreme is actually reached, so a position sitting still
// does not generate a database write on every tick.
//
// The distinction these columns carry is real training signal as well as display: a trade that
// reached 90% of its target and gave it all back is a completely different lesson from one that
// drifted sideways, and current PnL alone cannot tell them apart.
func (e *RealTrader) trackPnLExtremes(ctx context.Context, o port.RealOrder, price decimal.Decimal, logger *slog.Logger) {
	upl := unrealizedPnLPct(asPaperOrderView(o), price)
	if !upl.GreaterThan(o.PnLMaxPct) && !upl.LessThan(o.PnLMinPct) {
		return
	}
	if err := e.Repo.UpdateRealOrderPnLExtremes(ctx, o.ID, upl, upl); err != nil {
		logger.Warn("failed to update real pnl extremes", "instId", e.InstID, "orderId", o.ID, "error", err)
	}
}

func (e *RealTrader) monitorOpenPositions(ctx context.Context, price decimal.Decimal, logger *slog.Logger) error {
	open, err := e.openPositions(ctx)
	if err != nil {
		return fmt.Errorf("list open real positions: %w", err)
	}

	now := time.Now()
	for _, o := range open {
		// Track how far this position has travelled in each direction BEFORE checking for a close,
		// mirroring PaperTrader.monitorOpenOrders (CLAUDE.md §15.11): a trade that ran deep into
		// profit and round-tripped must still show that peak even on the tick that closes it.
		// Best-effort — this is model input and panel display, never a reason to block a close.
		//
		// Missing entirely until 2026-09-08: UpdateRealOrderPnLExtremes was implemented in
		// internal/postgres and declared on the port, but nothing ever called it, so the panel's
		// Max/Min columns sat at 0 for every real position no matter how far it moved.
		e.trackPnLExtremes(ctx, o, price, logger)

		// A manual close request from the panel wins over everything else, same priority order as
		// PaperTrader.monitorOpenOrders (CLAUDE.md real-trading readiness plan, 2026-09-04 — Close
		// button wiring): the operator explicitly asked to exit right now, checked before a
		// coincidental SL/TP touch on the same tick decides the reason instead.
		reason, hit := conductor.CloseReasonManual, o.ManualCloseRequested
		if !hit {
			reason, hit = closeReason(asPaperOrderView(o), price)
		}
		if !hit && e.conductor().IsTimedOut(o.OpenedAt, now) {
			reason, hit = conductor.CloseReasonTimeout, true
		}
		if !hit {
			continue
		}
		// The exchange holds the real stop (CLAUDE.md §35), and its own order fires the moment the
		// trigger is reached — usually before this monitor sees the same tick. Flattening anyway
		// asks OKX to close a position it already closed, which it rejects with
		// sCode=51169 "you don't have any positions in this direction ... to reduce or close".
		//
		// That is what every genuine exchange error on this deployment has been: 9 of 9, all
		// close_reason sl/tp, all on orders that had a protective order resting. The close itself
		// was never in danger — the exchange had already done it — but each one recorded a scary
		// last_error on a trade that completed exactly as intended, which is noise in the one
		// channel that must stay trustworthy.
		//
		// So on a LOCAL SL/TP touch, ask the exchange first. Only the SL/TP reasons are checked:
		// a manual close or a timeout is this system deciding to exit, and no resting order is
		// going to have done that for us.
		if reason == conductor.CloseReasonSL || reason == conductor.CloseReasonTP {
			if fired, ok := e.protectionAlreadyFired(o, logger); ok && fired {
				logger.Info("exchange's own protective order already closed this position; recording it rather than sending a duplicate flatten",
					"id", o.ID, "instId", e.InstID, "reason", reason)
				exReason, closePx, exPnL, exFee := e.closeFactsFromExchange(o, logger)
				if exReason != "" {
					reason = exReason
				}
				if !closePx.IsPositive() {
					closePx = price
				}
				facts := &exchangeCloseFacts{PnL: exPnL, Fee: exFee}
				// skipExchange: there is nothing left to flatten.
				if err := e.closeRealWith(ctx, o, closePx, reason, true, facts, logger); err != nil {
					logger.Error("failed to record an exchange-closed position", "id", o.ID, "instId", e.InstID, "error", err)
				}
				continue
			}
		}
		if err := e.closeReal(ctx, o, price, reason, logger); err != nil {
			logger.Error("failed to close real order", "id", o.ID, "instId", e.InstID, "error", err)
		}
	}
	return nil
}

// closeReal is RealTrader's single close path (mirrors PaperTrader.closeOrder — every close, SL/TP
// touch, timeout, or model-driven early close, goes through here so nothing can skip the terminal
// model call). Exchange-first: the flattening market order is placed BEFORE the DB is marked
// closed, so a DB failure never leaves the system believing a still-open real position is closed
// (CLAUDE.md §27.3). If the exchange already reports the position flat (a reconciliation-poll-
// detected close, see reconcile), skipExchange lets the flattening order be skipped since there is
// nothing left to close on OKX's side — the DB/reward/audit consequences are identical either way.
func (e *RealTrader) closeReal(ctx context.Context, o port.RealOrder, price decimal.Decimal, reason string, logger *slog.Logger) error {
	return e.closeRealWith(ctx, o, price, reason, false, nil, logger)
}

// recordCloseError persists a failed close's reason on the order and returns the error unchanged,
// so every failure path in closeRealWith both surfaces to the panel and still propagates to its
// caller. Persisting is best-effort: losing the record must not swallow the underlying error.
//
// The order's status is deliberately NOT reverted here. A failed close leaves the row in
// 'closing', which is what makes it visibly stuck and gets a human to look at it — quietly
// restoring 'filled' would make a position that may or may not still exist on the exchange look
// perfectly normal, which is precisely the failure mode this whole confirmation flow exists for.
func (e *RealTrader) recordCloseError(ctx context.Context, id int64, cause error, logger *slog.Logger) error {
	if e.Repo != nil {
		if err := e.Repo.SetRealOrderError(ctx, id, cause.Error()); err != nil {
			logger.Warn("failed to record close error on real order", "id", id, "error", err)
		}
	}
	logger.Error("real close failed", "id", id, "instId", e.InstID, "error", cause)
	return cause
}

// exchangeCloseNumbers extracts OKX's own accounting for a flatten from the order status it
// already returned — realized PnL and fee, both nil when the exchange reported nothing, since a
// missing figure must stay distinguishable from a real zero (the panel falls back to the locally
// computed value only when nil).
//
// Read from the ORDER, not from /account/positions: a fully-closed position disappears from that
// endpoint the moment it closes, so by the time a flatten is confirmed there is nothing left there
// to read. The order remains queryable and carries the figures that actually settled.
func exchangeCloseNumbers(status domain.OrderStatus) (pnl, fee *decimal.Decimal) {
	if !status.Pnl.IsZero() {
		v := status.Pnl
		pnl = &v
	}
	if !status.Fee.IsZero() {
		v := status.Fee
		fee = &v
	}
	return pnl, fee
}

// netRealizedPnL is what a real position actually earned or lost, preferring the exchange's own
// figures over a local calculation whenever OKX reported them (2026-09-10).
//
// The two sides measure DIFFERENT things, which is the whole bug this fixes. OKX's `pnl` on the
// flattening order is GROSS — the price move alone, with the fee reported separately in `fee` (a
// negative number) — while the local realizedPnL subtracts its OWN ESTIMATED fee from its own gross.
// Storing one in the column and the other alongside it made them look like a cross-check that
// disagreed, when in fact neither was the net figure the panel and the model's reward both need.
//
// Verified against every closed real order on 2026-09-10: OKX's gross pnl equals the pure price
// math (close - entry)/entry * size * leverage to the last digit on all of them, so the exchange
// and this codebase agree completely about the price move. Every discrepancy came from the fee: the
// live-closed rows carried a locally ESTIMATED fee instead of the real one, and the rows backfilled
// by hand in §37.3/§38.3 carried OKX's gross with no fee subtracted at all — order 37 was stored as
// a 0.0159 LOSS when it was really a 0.0017 gain before fees and a 0.0071 loss after them, i.e. the
// stored number had both the wrong magnitude and, against gross, the wrong sign.
//
// The exchange's numbers win because they are derived from the true fill price and the fee actually
// charged. The local calculation stays as the fallback for a close OKX did not report on — a
// skipExchange close whose algo order could not be read, most often — where an estimated fee is
// still much closer to the truth than no fee at all.
func netRealizedPnL(o port.PaperOrder, closePx decimal.Decimal, exchangePnL, exchangeFee *decimal.Decimal) decimal.Decimal {
	if exchangePnL == nil {
		return realizedPnL(o, closePx)
	}
	net := *exchangePnL
	if exchangeFee != nil {
		// Added, not subtracted: OKX reports the fee as a negative charge, so adding it reduces the
		// result. Subtracting would CREDIT the fee and overstate every trade by twice its cost.
		net = net.Add(*exchangeFee)
	}
	return net
}

func (e *RealTrader) closeRealWith(ctx context.Context, o port.RealOrder, price decimal.Decimal, reason string, skipExchange bool, facts *exchangeCloseFacts, logger *slog.Logger) error {
	// exchangeClosePx/exchangeFee/exchangePnL are the EXCHANGE's own numbers, left nil when it did
	// not report them (a skipExchange close, or a status response missing the field). nil is
	// deliberately distinct from zero: it means "OKX did not tell us", and the panel falls back to
	// the locally computed figure only in that case.
	var exchangeClosePx, exchangeFee, exchangePnL *decimal.Decimal
	// The flattening order's id, kept so its exchange record can be captured after the close is
	// durably recorded — never before, since an audit record must not be able to affect the close.
	var closeOrdID string

	// A caller that already learned the exchange's numbers passes them in (reconcile, for a
	// position the exchange's own stop-loss closed — there is no flatten of ours to read them
	// from). price is then the exchange's real fill price, so exchangeClosePx is that same number
	// rather than nil: the panel must show a close price that came from OKX, not one this system
	// inferred.
	if facts != nil {
		exchangePnL, exchangeFee = facts.PnL, facts.Fee
		if price.IsPositive() {
			px := price
			exchangeClosePx = &px
		}
	}

	if !skipExchange {
		side := "sell"
		if o.Side == "sell" {
			side = "buy"
		}
		// No close-price selection here any more: the flatten closes a contract COUNT, which no
		// price enters into. Choosing a price was only ever input to the size derivation that
		// caused the under-close.
		inst, err := e.instrumentMeta()
		if err != nil {
			return e.recordCloseError(ctx, o.ID, fmt.Errorf("fetch instrument metadata: %w", err), logger)
		}
		// Close exactly the contracts the exchange filled on the open. Re-deriving a count from the
		// stored margin is what broke here (2026-09-09): the open is sized at the ENTRY price and
		// the flatten was sizing at the CURRENT one, so a position whose price had moved favourably
		// bought fewer contracts for the same margin and the flatten under-closed by one — BTC
		// closed 1 of 2, ETH 5 of 6, DOGE 20 of 21. Each left a live remainder on the exchange
		// behind a row that recorded a complete close, and the untracked positions that produced
		// halted real trading for three hours.
		//
		// Falls back to the derivation only for rows opened before the count was recorded, since
		// those genuinely have nothing better — and at the ENTRY price, which is at least the price
		// the position was actually sized at.
		sz := o.Contracts
		if sz == nil || !sz.IsPositive() {
			derived := sizeToContracts(o.Size, o.Leverage, o.EntryPx, inst)
			sz = &derived
		}
		req := domain.OrderRequest{InstID: e.execInstID(), TdMode: e.TdMode, Side: side, OrdType: "market", Sz: *sz}
		if e.PosMode == "long_short" {
			req.PosSide = posSideFor(signedNotionalForSide(o.Side))
		}
		result, err := e.Exchange.PlaceOrder(req)
		if err != nil {
			return e.recordCloseError(ctx, o.ID, fmt.Errorf("flatten position: %w", err), logger)
		}
		if result != nil && result.SCode != "0" {
			return e.recordCloseError(ctx, o.ID,
				fmt.Errorf("flatten order rejected: sCode=%s sMsg=%s", result.SCode, result.SMsg), logger)
		}

		// Mark the close IN FLIGHT before waiting on it. This is what makes an in-progress or a
		// stuck close visible to a trader watching the panel rather than a row that looks idle —
		// and it records the flattening order's id, without which a close cannot be audited
		// against OKX afterwards at all (real order 3 had no such record).
		if result != nil {
			closeOrdID = result.OrdID
			if err := e.Repo.SetRealOrderClosing(ctx, o.ID, result.OrdID); err != nil {
				logger.Warn("failed to mark real order closing", "id", o.ID, "error", err)
			}
		}

		// CLAUDE.md §27.5: confirm the flatten actually filled before marking the DB row closed —
		// an unfilled or partially-filled flatten leaves real exposure still open on the exchange,
		// and closing the DB row in that case would make the system believe a position is flat when
		// it isn't. A partial fill here is deliberately NOT split into a smaller closed row (unlike
		// a partial OPEN fill, which records the smaller size actually acquired): a partially-
		// flattened position is still one open position with a reduced size, which the next tick's
		// ordinary SL/TP/timeout check and the reconciliation poll both already handle correctly
		// without new bookkeeping — this only needs to not lie about it being closed.
		if result != nil && result.OrdID != "" {
			status, err := e.waitForFill(ctx, result.OrdID, logger)
			if err != nil {
				return e.recordCloseError(ctx, o.ID, fmt.Errorf("wait for flatten fill: %w", err), logger)
			}
			if !status.IsFilled() {
				return e.recordCloseError(ctx, o.ID, fmt.Errorf(
					"flatten order for %d not fully filled (state=%s, filled=%s/%s); "+
						"position may still be open on the exchange, not marking closed",
					o.ID, status.State, status.AccFillSz, status.Sz), logger)
			}
			// The exchange's own fill price is what the position actually closed at — preferred
			// over the tick price that merely TRIGGERED the close, which is what produced order
			// 3's nonsense close_px of 102 against a market trading at 103.9.
			if status.AvgPx.IsPositive() {
				avg := status.AvgPx
				exchangeClosePx = &avg
				price = avg
			}
			// OKX's own realized PnL and fee for this flatten, preferred over a local calculation
			// that cannot see fees, funding, or the true fill price (2026-09-08 request).
			exchangePnL, exchangeFee = exchangeCloseNumbers(status)
		}
	}

	pnl := netRealizedPnL(asPaperOrderView(o), price, exchangePnL, exchangeFee)
	if err := e.Repo.CloseRealOrderConfirmed(ctx, o.ID, price, reason, pnl, exchangePnL, exchangeFee, exchangeClosePx); err != nil {
		// Another path closed it first (a second reconciliation pass, the tick monitor racing
		// reconcile, a second process after a restart). The position IS closed and this caller
		// simply was not the one that closed it, so it stops here rather than going on to deliver
		// a duplicate reward to the model or publish a duplicate close event — both of which
		// really happened on real order 38, closed twice 3 seconds apart.
		if errors.Is(err, port.ErrOrderAlreadyClosed) {
			logger.Info("real order was already closed by another path; nothing to do",
				"id", o.ID, "instId", e.InstID, "reason", reason)
			return nil
		}
		return err
	}
	// After the close is durably recorded: the flatten has filled, so its record is final.
	e.captureExchangeRecord(ctx, o.ID, "close", closeOrdID, logger)
	// Remove the resting protective order now that it guards nothing (2026-09-09). Runs AFTER the
	// close is recorded and is best-effort: a leftover conditional order must never make a
	// completed close look failed. It matters even though OKX prunes orphaned conditionals itself,
	// because the failure mode of a stale one — triggering and OPENING a position on an account
	// that believes it is flat — is not something to leave to cleanup semantics.
	//
	// Cancelled on EVERY close, including an SL/TP one. A stop touch detected by the in-process
	// backup monitor does not mean the exchange's own order fired — the backup exists precisely for
	// the case where it did not — so skipping the cancel on reason "sl"/"tp" would be reasoning
	// from this system's view of why the position closed rather than from the exchange's. Cancelling
	// an order that has already triggered is a harmless no-op; leaving a live one behind is not.
	e.cancelProtection(ctx, o, logger)
	metrics.PaperOrdersClosedTotal.WithLabelValues(e.InstID, reason).Inc()
	metrics.PaperOrdersRealizedPnL.WithLabelValues(e.InstID).Add(pnl.InexactFloat64())
	logger.Info("closed real order", "id", o.ID, "instId", e.InstID, "reason", reason, "closePx", price, "pnl", pnl)
	e.publishOrderEvent(ctx, "closed", o.ID, logger)

	e.conductor().Forget(o.ID)
	e.reportTerminalReal(ctx, o, price, pnl, reason, logger)

	// NOTE: no ApplyRealizedPnL here, deliberately (2026-09-08). In real mode the exchange's own
	// reported balance is ground truth and already reflects this trade's PnL the moment it closes;
	// the reconciliation poll's RecordExchangeBalance observes that change and records it as a
	// reason="trade" history point. Adding pnl to the stored balance here as well would count the
	// same profit or loss TWICE against a real account.
	//
	// This call used to exist and always failed, on a foreign key from account_equity_history
	// .order_id to paper_orders(id) that real order ids can never satisfy (migration 000019 moved
	// real orders to their own table). The failure was logged and swallowed, so the double-count it
	// would otherwise have produced never actually happened — the constraint was accidentally
	// holding the account correct. Removing the call is what makes that correctness intentional
	// rather than a side effect of a broken write, and migration 000026 then drops the now-pointless
	// constraint so the audit log can carry a real order_id at all.
	return nil
}

// reportTerminalReal delivers a closed real trade's outcome to the model — same terminal-call
// contract as PaperTrader.reportTerminal (CLAUDE.md §15.10: the close event IS the reward).
func (e *RealTrader) reportTerminalReal(ctx context.Context, o port.RealOrder, closePx, pnl decimal.Decimal, closeReason string, logger *slog.Logger) {
	if e.Model == nil {
		return
	}
	category := conductor.TerminalCategory(closeReason)
	if category == "" {
		return
	}
	obs := e.buildObservation(ctx, e.decisionBar(), closePx, logger)
	obs.Category = category
	obs.OrderID = o.ID
	obs.Signal = e.carriedSignalFor(e.decisionBar())
	ps := positionStateOf(asPaperOrderView(o), closePx)
	ps.RealizedPnLUSD = pnl
	obs.PositionState = ps
	if _, err := e.Model.Predict(ctx, obs); err != nil {
		logger.Warn("real updates: terminal report failed; this trade will not train the model",
			"instId", e.InstID, "orderId", o.ID, "category", category, "error", err)
	}
}

func (e *RealTrader) carriedSignalFor(bar string) *domain.StrategySignal {
	sig, ok := e.conductor().CarriedSignal(e.InstID, bar)
	if !ok {
		return nil
	}
	return &sig
}

func (e *RealTrader) publishOrderEvent(ctx context.Context, eventType string, orderID int64, logger *slog.Logger) {
	if e.OrderEvents == nil {
		return
	}
	event := PaperOrderEvent{Type: eventType, OrderID: orderID, InstID: e.InstID}
	if err := e.OrderEvents.Publish(ctx, e.InstID, event); err != nil {
		logger.Warn("failed to publish real order event", "type", eventType, "orderId", orderID, "instId", e.InstID, "error", err)
	}
}

// buildObservation assembles the observation for this token, reusing the same shape PaperTrader
// sends — CLAUDE.md §27's plan §6: AccountEquityUSD/OpenExposureUSD are what differ from paper's
// version (ground-truth exchange values here, not this engine's own bookkeeping), everything else
// (candle window, strategy signals, price context, token identity) is identical logic.
func (e *RealTrader) buildObservation(ctx context.Context, bar string, price decimal.Decimal, logger *slog.Logger) domain.Observation {
	view := e.marketView(bar)
	window := view.Candles

	tb := domain.TimeframeBlock{Bar: bar, PriceContext: buildPriceContext(window)}
	for _, a := range e.Strategies {
		if a.Bar != bar {
			continue
		}
		sig, err := strategy.EvaluateWith(a.Strategy, view)
		if err != nil {
			continue
		}
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
		// The risk budget the model must size within (schema v7) — the per-token even share of the
		// account, matching PaperTrader.evenShareOfAccount so one policy serving both modes reads
		// the same meaning from the field. account.max_position_pct still applies afterwards in
		// sizeFromModelAction as the hard ceiling.
		MaxPositionPct: e.evenShareOfAccount(),
		MaxLeverage:    e.MaxLeverage,
		Category:       domain.CategoryUpdate,
	}

	// The TOTAL is ground truth from the exchange, not GetAccountEquity's bookkeeping row — real
	// trading does not own that number the way paper trading owns its shared account (CLAUDE.md
	// §27's plan §6: the one spot flagged as easy to get wrong by careless reuse). What the model
	// is allowed to SIZE against is a slice of it, and the reserve is subtracted here so sizing can
	// never draw against capital held back.
	balances, err := e.Exchange.GetBalance(e.settleCcy())
	if err != nil {
		logger.Warn("real observation: get balance failed", "instId", e.InstID, "error", err)
	} else if len(balances) > 0 {
		obs.AccountEquityUSD = e.tradableEquityFor(ctx, balances[0].Eq)
	}
	obs.OpenExposureUSD = e.openExposureReal(ctx, logger)

	return obs
}

// openExposureReal sums the notional of every open real position across ALL tokens, mirroring
// PaperTrader.openExposure but reading real_orders (CLAUDE.md real-trading readiness plan,
// 2026-09-04) rather than paper_orders — real trading has no per-mode filter to apply here since
// every real_orders row already belongs to real trading by construction.
func (e *RealTrader) openExposureReal(ctx context.Context, logger *slog.Logger) decimal.Decimal {
	openOnly := true
	positions, err := e.Repo.ListRealPositions(ctx, port.PositionFilter{Open: &openOnly})
	if err != nil {
		logger.Warn("real observation: list open positions failed", "mode", e.accountMode(), "error", err)
		return decimal.Zero
	}
	var total decimal.Decimal
	for _, p := range positions {
		total = total.Add(p.Size)
	}
	return total
}

// runReconcileLoop periodically compares this process's own open real positions against OKX's
// authoritative GetPositions/GetBalance response (CLAUDE.md §27.3/§27.6), on a fixed 1-minute
// cadence (reconcileInterval) — separate from and much slower than the tick-driven SL/TP monitor,
// since this poll exists only to catch drift, not to drive trading.
func (e *RealTrader) runReconcileLoop(ctx context.Context, logger *slog.Logger) error {
	ticker := time.NewTicker(e.reconcileInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			e.reconcile(ctx, logger)
		}
	}
}

// ReconcileNow runs one reconciliation pass for this instrument immediately, fetching its own
// account snapshot.
//
// The private WebSocket push no longer comes through here — it goes to
// ReconcileDriver.ReconcileInstrument (2026-09-10), which fetches ONE snapshot rather than letting
// a per-engine call re-read account-wide data. The routing principle is unchanged and still the
// reason both paths converge on ReconcileWith: there is ONE definition of how this system responds
// to a position change, and a second would be free to drift from it in a way only visible when the
// two disagreed about a real position.
//
// Kept for an engine running without a driver, and used directly by tests. Safe to call
// concurrently with the periodic loop: reconcile re-reads both sides before acting, so a redundant
// pass is a no-op rather than a double-close.
func (e *RealTrader) ReconcileNow(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = e.Logger
	}
	if logger == nil {
		logger = slog.Default()
	}
	e.reconcile(ctx, logger)
}

// reconcile fetches an account snapshot of its own and runs one pass for this instrument.
//
// Prefer ReconcileWith when a snapshot is already in hand: GetPositions/GetBalance are ACCOUNT-wide
// (they take no instrument and return the same response to every engine), so a per-engine fetch
// multiplies one call by the size of the roster. See AccountSnapshot.
func (e *RealTrader) reconcile(ctx context.Context, logger *slog.Logger) {
	snap, err := FetchAccountSnapshot(e.Exchange, e.execInstType(), e.settleCcy())
	if err != nil {
		logger.Warn("reconcile: account snapshot failed", "instId", e.InstID, "error", err)
		return
	}
	e.ReconcileWith(ctx, snap, logger)
}

// ReconcileWith runs one reconciliation pass for this instrument against an already-fetched
// account snapshot (2026-09-10). Splitting the fetch out is what lets one pass serve the whole
// roster: the per-instrument work below (the drift comparison, the protective-order verification,
// the missed-SL/TP catch) genuinely differs per engine, but the two account-wide reads it used to
// issue did not.
//
// Equity recording is deliberately NOT done here — it writes one shared account row, so the caller
// does it once per snapshot rather than once per engine. See recordEquityReal.
func (e *RealTrader) ReconcileWith(ctx context.Context, snap AccountSnapshot, logger *slog.Logger) {
	if logger == nil {
		logger = e.Logger
	}
	if logger == nil {
		logger = slog.Default()
	}

	// Held for the whole pass, not just the read: the decision made here is derived from both
	// sides' state, so releasing between reading and acting would leave exactly the window a
	// second caller could act on the same difference.
	e.reconcileMu.Lock()
	defer e.reconcileMu.Unlock()

	remote := snap.PositionFor(e.execInstID())

	local, err := e.openPositions(ctx)
	if err != nil {
		logger.Warn("reconcile: list local open positions failed", "instId", e.InstID, "error", err)
		return
	}

	// Verify every open position still has a live protective order on the exchange, and re-place
	// any that has gone missing (2026-09-09). The exchange holds the stop, so this is what keeps
	// that trust honest — an algo order can be cancelled from OKX's own UI or lost to a margin-mode
	// change, neither of which produces any signal in this process. Runs before the drift
	// comparison below so a position about to be reported as drifted is still checked.
	e.ensureProtection(ctx, local, logger)

	switch {
	case remote == nil && len(local) > 0:
		// OKX shows flat but we still think a position is open — a liquidation, a manual close on
		// OKX's own UI/app, or anything else that happened outside this system. Route through the
		// normal close path (skipExchange: nothing left to flatten) so the DB/reward/audit
		// consequences are identical to a tick-driven close, just discovered a poll interval late.
		logger.Warn("reconcile: exchange reports flat but local state shows an open position; closing locally",
			"instId", e.InstID, "localOrders", len(local))
		for _, o := range local {
			// Ask the exchange why this position closed and at what price, instead of assuming a
			// manual close at the entry price (2026-09-09) — see closeFactsFromExchange.
			reason, closePx, exPnL, exFee := e.closeFactsFromExchange(o, logger)
			facts := &exchangeCloseFacts{PnL: exPnL, Fee: exFee}
			if err := e.closeRealWith(ctx, o, closePx, reason, true, facts, logger); err != nil {
				logger.Error("reconcile: failed to close locally-stale position", "id", o.ID, "error", err)
			}
		}
	case remote != nil && len(local) == 0:
		// An open in flight explains this completely: the exchange has filled the position but the
		// local row is not written yet. Halting here stops ALL real trading across every instrument
		// because of a position this system is in the middle of opening deliberately (§48).
		//
		// Skipping is safe rather than a hole in the check: the open path always writes its row (or
		// logs loudly if it cannot), and the very next reconcile pass — one second later on the
		// 5s poll, or immediately on the next pushed event — re-evaluates with the row present. A
		// genuinely untracked position therefore still halts, just one pass later.
		if e.shouldDeferUntrackedHalt(logger, remote) {
			break
		}
		logger.Error("reconcile: exchange reports an open position this system has no record of",
			"instId", e.InstID, "remoteSize", remote.Pos, "remoteSide", remote.PosSide)
		e.RiskManager.Halt(fmt.Sprintf("reconcile: untracked open position on %s (exchange reports %s %s)",
			e.InstID, remote.Pos.String(), remote.PosSide))
	case remote != nil && len(local) > 0:
		// Both sides agree a position exists; check it's the SAME position. Compare notional as the
		// simplest available cross-check (size in contracts vs. this system's own USD notional isn't
		// directly comparable without the instrument's contract multiplier, which isn't wired in yet
		// per trade.go's own long-standing note) — a sign/side mismatch is the concrete, checkable
		// case worth alerting on now.
		localSide := local[0].Side
		remoteSide := "buy"
		if remote.PosSide == "short" || remote.Pos.IsNegative() {
			remoteSide = "sell"
		}
		if localSide != remoteSide {
			logger.Error("reconcile: side mismatch between local record and exchange",
				"instId", e.InstID, "localSide", localSide, "remoteSide", remoteSide, "remotePosSide", remote.PosSide)
			e.RiskManager.Halt(fmt.Sprintf("reconcile: side mismatch on %s (local %s, exchange %s)",
				e.InstID, localSide, remoteSide))
			break
		}

		// A position that agrees with the exchange can still be one this engine has stopped
		// watching. SL/TP execution is tick-driven and in-process (§27.3), so it stops entirely
		// whenever the process does — and the position keeps running on the exchange with real
		// money behind it and nothing enforcing its stop.
		//
		// Real order 33 (PEPE, 2026-09-09): its stop sat at -14.7% of margin, price breached it and
		// reached -19.6% while the trader was down for 15 minutes, and nothing closed it. It only
		// exited because the operator had already requested a manual close — by then back at -8.7%,
		// so the loss happened to be smaller, but that was luck, not the system working.
		//
		// This is the check that makes "verify against the exchange" mean something for a position
		// that already exists: the poll re-runs the same SL/TP touch test against the exchange's
		// own mark price, so a stop breached during any gap is acted on at the next poll instead of
		// waiting for a tick that may never be evaluated. Uses MarkPx, which reconcile already
		// fetches and previously ignored.
		if remote.MarkPx.IsPositive() {
			if reason, hit := closeReason(asPaperOrderView(local[0]), remote.MarkPx); hit {
				logger.Warn("reconcile: position is past its own SL/TP but was never closed; closing now",
					"instId", e.InstID, "id", local[0].ID, "reason", reason, "markPx", remote.MarkPx)
				if err := e.closeReal(ctx, local[0], remote.MarkPx, reason, logger); err != nil {
					logger.Error("reconcile: failed to close a position past its level", "id", local[0].ID, "error", err)
				}
			}
		}
	}

}

// recordEquityReal mirrors trade.go's Trader.recordEquity: the exchange's reported balance is
// ground truth, so this only observes it and records the delta from what was last stored, never
// applying a top-up/reset the way paper mode's balance does. Best-effort.
//
// Takes the RAW exchange balance (NOT run through tradableEquity first) and lets
// RecordExchangeBalance apply the SafeMoneyUSD reserve as a pure derived-EquityUSD view rather
// than a stored delta — CLAUDE.md §32's incident: computing tradableEquity(rawBalance) here and
// feeding that into a plain delta-from-EquityUSD comparison meant the reserve itself was read as
// a realized trade loss the first time SafeMoneyUSD went from 0 to nonzero, corrupting the real
// AccountBalanceUSD by the reserve amount.
func (e *RealTrader) recordEquityReal(ctx context.Context, rawBalance decimal.Decimal, logger *slog.Logger) {
	if e.Repo == nil {
		return
	}
	initial := e.AccountInitialUSD
	if !initial.IsPositive() {
		initial = rawBalance
	}
	if _, err := e.Repo.GetAccountEquity(ctx, e.accountMode(), initial); err != nil {
		logger.Warn("reconcile: equity timeline read failed", "mode", e.accountMode(), "error", err)
		return
	}
	if _, err := e.Repo.RecordExchangeBalance(ctx, e.accountMode(), rawBalance, e.SafeMoneyUSD, e.InstID); err != nil {
		logger.Warn("reconcile: equity timeline write failed", "mode", e.accountMode(), "error", err)
	}
}

// setOpenInFlight marks whether this engine is mid-open, suppressing reconcile's untracked-position
// halt for its own instrument (CLAUDE.md §48).
//
// Guarded by openMu, which evaluateStrategies already holds across the whole open sequence. Taking
// it here too would deadlock; reconcile reads the flag under the same lock via isOpenInFlight.
func (e *RealTrader) setOpenInFlight(v bool) {
	e.openInFlight = v
}

// isOpenInFlight reports whether an open is in progress on this engine.
//
// Takes openMu because reconcile runs on a different goroutine from the open path — reading a bool
// without synchronisation is a data race the Go race detector correctly flags, and the value read
// could be arbitrarily stale. A blocked read here is also exactly the desired behaviour: if an open
// currently holds openMu, reconcile waits for it to finish and then sees the committed row rather
// than the in-flight gap.
func (e *RealTrader) isOpenInFlight() bool {
	e.openMu.Lock()
	defer e.openMu.Unlock()
	return e.openInFlight
}

// shouldDeferUntrackedHalt reports whether an "untracked" remote position should be tolerated
// because this engine is in the middle of opening it.
//
// Extracted as its own predicate so the decision is unit-testable without a repository or a live
// exchange — the surrounding ReconcileWith needs both. That matters here because this exact
// decision, made wrongly, halted all real trading for two hours (CLAUDE.md §48), and a regression
// test should be able to reproduce it directly rather than approximate it.
func (e *RealTrader) shouldDeferUntrackedHalt(logger *slog.Logger, remote *domain.Position) bool {
	if !e.isOpenInFlight() {
		return false
	}
	logger.Info("reconcile: remote position with no local row while an open is in flight; "+
		"deferring to the next pass", "instId", e.InstID, "remoteSize", remote.Pos)
	return true
}
