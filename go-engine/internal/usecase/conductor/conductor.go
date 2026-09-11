// Package conductor owns the signal lifecycle: the sequence of decisions the RL model is asked to
// make about one trading signal, from a strategy firing through the position closing and its
// realized outcome being delivered back as reward (CLAUDE.md §15.10/§15.12).
//
// "Conductor" rather than "controller" is deliberate. It does not decide direction — strategies do
// (§9/§16.1) — and it does not decide size — the model does. It decides *who plays when*: which
// lifecycle question to ask, at what cadence, and it guarantees the terminal call that carries the
// reward actually happens. Coordination without authorship.
//
// Everything here is pure: no IO, no repository, no model client. usecase.PaperTrader owns those
// and asks the Conductor what to do, which is what makes the cadence and category rules testable
// without a database or a running rl-service.
package conductor

import (
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// Defaults for Config, matching CLAUDE.md §15.12's stated starting points. They are starting
// points, not tuned values — expect to revisit them once real training data exists.
var (
	// DefaultUpdatePnLThresholdPct: an `update` fires once unrealized PnL has moved this far (as a
	// fraction, 0.01 = 1%) since the last update for that order. PnL-delta rather than a fixed
	// interval is what makes the cadence self-adapting: near-silent while a position ranges, dense
	// while it actually moves. It also keeps credit assignment tractable — order 10 meaningful
	// steps per trade instead of thousands of near-identical ones.
	DefaultUpdatePnLThresholdPct = decimal.NewFromFloat(0.01)
	// DefaultUpdateMaxInterval is the time ceiling: a position grinding sideways must still be
	// observed, because funding accrues, setups decay, and "time passed" is itself information the
	// model should get.
	DefaultUpdateMaxInterval = 15 * time.Minute
	// DefaultMaxOpenDuration is how long a position may stay open before PaperTrader force-closes
	// it as a timeout (CLAUDE.md §15.14). 6h per the operator's explicit starting value —
	// positions were observed sitting open a long time with barely-moving PnL, tying up an
	// instrument's one-open-position slot (§16.9) without the position itself going anywhere.
	DefaultMaxOpenDuration = 6 * time.Hour
)

// Config is the conductor's tunable behavior, sourced from paper_trading.rl_* config keys.
type Config struct {
	// UpdatePnLThresholdPct / UpdateMaxInterval drive the update cadence (CLAUDE.md §15.12).
	// Non-positive values fall back to the defaults above.
	UpdatePnLThresholdPct decimal.Decimal
	UpdateMaxInterval     time.Duration

	// AllowEarlyClose gates the model's `close` action (CLAUDE.md §15.12). When false a close
	// proposal is ignored and the position runs to its SL/TP as before — useful for a first live
	// pass where only the SL/TP-adjust behavior is being evaluated.
	AllowEarlyClose bool

	// Clamps bound the SL/TP levels the model sets. Early in training the policy is effectively
	// random, and one absurd stop would otherwise destroy a position — the model is never the
	// safety boundary (§15.11, the RatchetSLTP pattern).
	Clamps Clamps

	// MaxOpenDuration force-closes a position that has been open this long, regardless of what
	// the model would otherwise decide (CLAUDE.md §15.14) — a hard housekeeping limit, not a
	// model decision, same posture as Clamps. Non-positive falls back to DefaultMaxOpenDuration.
	MaxOpenDuration time.Duration
}

func (c Config) pnlThreshold() decimal.Decimal {
	if c.UpdatePnLThresholdPct.IsPositive() {
		return c.UpdatePnLThresholdPct
	}
	return DefaultUpdatePnLThresholdPct
}

func (c Config) maxInterval() time.Duration {
	if c.UpdateMaxInterval > 0 {
		return c.UpdateMaxInterval
	}
	return DefaultUpdateMaxInterval
}

// maxOpenDuration resolves MaxOpenDuration against its default, same pattern as maxInterval.
func (c Config) maxOpenDuration() time.Duration {
	if c.MaxOpenDuration > 0 {
		return c.MaxOpenDuration
	}
	return DefaultMaxOpenDuration
}

// Conductor tracks per-order update state and answers the two questions PaperTrader has at each
// event: which lifecycle category is this, and is an update due yet.
//
// Per-order state is in memory by design. Losing it on restart is harmless — the next tick simply
// triggers one update, which is the correct behavior for a process that has just come back and
// does not know how the position has moved since (CLAUDE.md §15.12).
type Conductor struct {
	Cfg Config

	mu sync.Mutex
	// updates is keyed by order id. Entries are removed when the order closes, so this cannot grow
	// beyond the set of currently-open orders.
	updates map[int64]updateState
	// signals retains the most recent signal per (instID, bar) so a higher-timeframe opinion stays
	// available between its candles (CLAUDE.md §15.12's carry-forward): a 1H signal is meaningful
	// for the whole hour, and dropping it the moment the candle closes would hide it from every
	// update in between.
	signals map[signalKey]domain.StrategySignal
}

type updateState struct {
	lastPnLPct decimal.Decimal
	lastAt     time.Time
}

type signalKey struct {
	instID string
	bar    string
}

// New returns a Conductor ready to use. The zero Config is valid — every field falls back to a
// documented default.
func New(cfg Config) *Conductor {
	return &Conductor{
		Cfg:     cfg,
		updates: make(map[int64]updateState),
		signals: make(map[signalKey]domain.StrategySignal),
	}
}

// OpenCategory returns the lifecycle category for a strategy signal that fired while no position
// is open on this token: the model is being asked whether to take the trade at all (CLAUDE.md
// §15.12). side is strategy.Side's string form ("buy"/"sell"); an unrecognized side returns "",
// which callers treat as "don't ask".
func OpenCategory(side string) string {
	switch side {
	case "buy":
		return domain.CategoryBuy
	case "sell":
		return domain.CategorySell
	default:
		return ""
	}
}

// TerminalCategory maps a close reason to the terminal category that delivers the reward
// (CLAUDE.md §15.10 — the close event IS the reward). Returns "" for a reason with no lifecycle
// meaning at all.
//
// CloseReasonTimeout and CloseReasonManual both share CategoryClosedEarly with CloseReasonRLEarly
// rather than getting their own categories (CLAUDE.md §15.14's reasoning, extended 2026-08-31 to
// the panel's manual close button): a new one-hot category needs a new observation_schema_version
// on both Go and rl_service, which breaks every /predict call until both sides deploy together and
// only starts teaching the model anything once retrained on the wider input. The distinction still
// matters for a HUMAN reading paper_orders — "the model chose to exit" vs. "the position sat too
// long" vs. "an operator closed it by hand" are different stories — so each stays its own DB
// close_reason value ('timeout' since migration 000001, 'manual' since the very first migration);
// the model itself is simply told "this was a decision-driven exit", which is true of all three.
// The alternative — reporting nothing for a manual close, so a human decision is never attributed
// to the policy — was considered and rejected: it would leave that trade training nothing at all,
// and the model has no closed_manual category to report it under honestly either way.
func TerminalCategory(closeReason string) string {
	switch closeReason {
	case "tp":
		return domain.CategoryClosedTP
	case "sl":
		return domain.CategoryClosedSL
	case CloseReasonRLEarly, CloseReasonTimeout, CloseReasonManual:
		return domain.CategoryClosedEarly
	default:
		return ""
	}
}

// CloseReasonRLEarly marks a position the model closed before either level was touched (CLAUDE.md
// §15.12). Recorded as its own reason rather than reusing "manual" so early-closed trades stay
// distinguishable from operator action, and so they can be compared against trades that ran to
// SL/TP — the same evidence-gathering logic as the shadow forks.
// CloseReasonSL/CloseReasonTP are the two reasons produced by an SL/TP touch (SLTPTouchReason).
// Named here alongside the other reasons so call sites can compare against a constant rather than
// a bare string — RealTrader needs to distinguish "the price reached a level" from "this system
// decided to exit", because only the former can already have been executed by the exchange's own
// resting order (CLAUDE.md §35).
const CloseReasonSL = "sl"

// CloseReasonTP is the take-profit twin of CloseReasonSL.
const CloseReasonTP = "tp"

const CloseReasonRLEarly = "rl_early"

// CloseReasonTimeout marks a position PaperTrader itself force-closed because it stayed open
// longer than MaxOpenDuration (CLAUDE.md §15.14, operator request 2026-08-30: some positions sat
// open for a long time with barely-moving PnL). This is a housekeeping/risk decision, not the
// model's — same "don't trust the model to have learned this" posture as RatchetSLTP and the
// clamps — so it fires regardless of RLEarlyClose. Still reported to the model via
// TerminalCategory as closed_early (see its doc comment) so the trade still trains something,
// rather than the silent-no-reward gap a plain "manual" close would leave.
const CloseReasonTimeout = "timeout"

// CloseReasonManual marks a position an operator closed by hand from the panel's Close button
// (2026-08-31 request). Reported to the model via TerminalCategory as closed_early, same
// "decision-driven exit, trains something rather than nothing" treatment as CloseReasonTimeout —
// see TerminalCategory's doc comment for why neither gets its own one-hot category.
const CloseReasonManual = "manual"

// ShouldUpdate reports whether an `update` call is due for the open order at orderID, given its
// current unrealized PnL. It returns true when either trigger fires (CLAUDE.md §15.12):
//
//   - PnL has moved at least UpdatePnLThresholdPct since the last update for this order, or
//   - UpdateMaxInterval has elapsed since then.
//
// A strategy firing is the third trigger, but that path never consults this method — a real
// opinion always reaches the model immediately and is never filtered by cadence.
//
// The first call for an order establishes its baseline but does NOT fire (fixed 2026-09-03): it
// used to return true unconditionally on first sight, on the reasoning that "no baseline exists
// yet, so observing it once establishes one" — harmless as long as an update's answer could only
// adjust SL/TP, but the very first tick after an order opens (~2s later on this engine's cadence)
// has no price movement or elapsed time to judge anything from. Once AllowEarlyClose let the model
// actually CLOSE the position on that answer, every order whose policy had converged to "close"
// was being closed within ~1s of opening at roughly breakeven — this was invisible before because
// the close was silently discarded, not because the question wasn't being asked. Now the first
// call only starts the baseline clock, exactly like every later check.
//
// Calling this CLAIMS the slot when it returns true (it records the new baseline), so concurrent
// callers cannot both fire for the same order — the same check-and-claim pattern as
// PaperTrader.shouldRunRLAdjust.
func (c *Conductor) ShouldUpdate(orderID int64, pnlPct decimal.Decimal, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	prev, seen := c.updates[orderID]
	if !seen {
		c.updates[orderID] = updateState{lastPnLPct: pnlPct, lastAt: now}
		return false
	}

	moved := pnlPct.Sub(prev.lastPnLPct).Abs().GreaterThanOrEqual(c.Cfg.pnlThreshold())
	elapsed := now.Sub(prev.lastAt) >= c.Cfg.maxInterval()
	if !moved && !elapsed {
		return false
	}

	c.updates[orderID] = updateState{lastPnLPct: pnlPct, lastAt: now}
	return true
}

// NoteSignalUpdate records that a strategy-driven update was just sent for this order, so a signal
// firing also resets the cadence baseline. Without this, a signal-triggered call would be followed
// immediately by a redundant PnL-triggered one.
func (c *Conductor) NoteSignalUpdate(orderID int64, pnlPct decimal.Decimal, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates[orderID] = updateState{lastPnLPct: pnlPct, lastAt: now}
}

// Forget drops an order's update state. Called when the order closes; keeping it would leak one
// map entry per closed trade for the lifetime of the process.
func (c *Conductor) Forget(orderID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.updates, orderID)
}

// IsTimedOut reports whether a position opened at openedAt should be force-closed for having run
// longer than MaxOpenDuration (CLAUDE.md §15.14). Pure and stateless — unlike ShouldUpdate this
// needs no per-order tracking, since the order's own OpenedAt is all the input required, and a
// method taking (Config, time, time) rather than an orderID keeps it testable without touching the
// Conductor's internal maps at all.
func (c *Conductor) IsTimedOut(openedAt, now time.Time) bool {
	return now.Sub(openedAt) >= c.Cfg.maxOpenDuration()
}

// RetainSignal stores the most recent signal for (instID, bar) so it can be carried forward onto
// later price-driven update calls (CLAUDE.md §15.12).
func (c *Conductor) RetainSignal(instID, bar string, sig domain.StrategySignal) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signals[signalKey{instID: instID, bar: bar}] = sig
}

// CarriedSignal returns the retained signal for (instID, bar), if any.
//
// Note the model currently receives no explicit staleness for a carried signal: §15.11 dropped the
// signal's own age field, so how long ago it fired reaches the model only indirectly through
// PositionState.AgeSeconds. If carry-forward turns out to need explicit staleness, that is an
// observation-schema change, not a change here.
func (c *Conductor) CarriedSignal(instID, bar string) (domain.StrategySignal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sig, ok := c.signals[signalKey{instID: instID, bar: bar}]
	return sig, ok
}
