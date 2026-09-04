// Package port defines the interfaces use-cases/services depend on, so adapters (Postgres, OKX,
// Redis, ...) can be swapped without touching business logic. See CLAUDE.md §10.
package port

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// Candle is one persisted OHLCV bar for an instrument (domain.Candle plus storage identity
// fields). The bar's timestamp lives on the embedded domain.Candle.Timestamp.
type Candle struct {
	InstID string
	Bar    string
	domain.Candle
}

// StrategyConfig is a persisted, pluggable signal-generator configuration (see CLAUDE.md §9).
// Every strategy descends from a locked "origin" row (IsOrigin true, ClonedFrom nil) representing
// the built-in implementation as shipped; sub-strategies (ClonedFrom set) store their own
// parameter overrides in Config without mutating the origin (CLAUDE.md §11.3).
type StrategyConfig struct {
	ID         int64
	Name       string
	InstIDs    []string
	Kind       string // e.g. "rsi_sma", "macd", "ict_smc", "custom"
	Config     json.RawMessage
	Enabled    bool
	IsOrigin   bool
	ClonedFrom *int64
}

// StrategyAssignment is a durable token+timeframe binding for a strategy (CLAUDE.md §11.3): which
// strategy variant evaluates on which instrument's candle close. Loaded from Postgres on every
// process start so a crash/restart never loses which variant was running where — this is
// distinct from usecase.StrategyAssignment, which pairs a *live* strategy.Strategy value (already
// resolved from this row's StrategyID+Config) with its Bar for the running PaperTrader.
type StrategyAssignment struct {
	ID         int64
	StrategyID int64
	InstID     string
	Bar        string
	Enabled    bool
	// Mode scopes this assignment to "paper" or "real" trading (CLAUDE.md, real-trading readiness
	// plan, 2026-09-04) — paper and real trading each maintain independent strategy assignments,
	// so a strategy tuned/enabled for paper trading has no effect on real trading and vice versa.
	// Defaults to "paper" for every row created before this field existed.
	Mode string
}

// StrategyStats summarizes one strategy's paper-trading track record (CLAUDE.md §11.3),
// computed from paper_orders — no separately maintained counters.
type StrategyStats struct {
	StrategyID  int64
	SignalCount int64
	// Wins/Losses are decided by realized PnL, not by close_reason. Counting close_reason='tp'
	// as the win is wrong once the RL ratchet (CLAUDE.md §15.4) trails a stop into profit: a
	// stop-loss touch then banks a GAIN. Measured on real data, 101 of 152 'sl' closes were
	// profitable, so the old definition reported 0% win rate for a strategy making money.
	Wins         int64 // closed with realized_pnl > 0
	Losses       int64 // closed with realized_pnl <= 0
	OpenCount    int64
	RealizedPnL  decimal.Decimal
	FirstOpened  *time.Time
	LastActivity *time.Time
}

// TokenStats summarizes one token's last-24h paper-trading activity for the panel's "Manage
// tokens" modal (2026-09-04 request) — mirrors StrategyStats' shape/scoping (variant='baseline'
// only, same fork-dilution reasoning, CLAUDE.md §16.9) but keyed by inst_id instead of
// strategy_id, and windowed to trades CLOSED in the last 24h rather than all-time (PnL only exists
// once a trade closes, matching how the account-wide 24h stat already works, CLAUDE.md §32).
type TokenStats struct {
	InstID        string
	PositionCount int64 // closed in the last 24h
	PnLUSD        decimal.Decimal
	PnLPct        decimal.Decimal // relative to the token's own summed entry notional in the window
}

// PositionFilter selects/sorts/pages positions across trading modes for the panel (CLAUDE.md
// §11.4). Limit/Offset were added 2026-09-02: the panel used to fetch every matching row on every
// 5s poll and paginate/sort client-side, which became a genuinely slow query and a multi-MB
// payload once closed positions numbered in the hundreds (each row carries FeaturesJSON, the full
// decision-time observation, averaging ~3.8KB). Now capped server-side to one page at a time.
type PositionFilter struct {
	Mode   string // "paper", "demo", "real", or "" for all
	InstID string // "" for all
	Open   *bool  // nil = both open and closed
	// SortBy: "opened_at", "closed_at" (default), "pnl", "inst_id". SortDesc reverses order.
	SortBy   string
	SortDesc bool
	// Limit caps the number of rows returned; <= 0 means no cap (used by callers that need the
	// full set, e.g. handlePaperTradingStats' open-position count). Offset is rows to skip,
	// applied only when Limit > 0.
	Limit  int
	Offset int
}

// PaperOrder is a virtual (forward-test) trade opened by the Paper Trading Engine (CLAUDE.md §8).
type PaperOrder struct {
	ID         int64
	InstID     string
	StrategyID *int64
	// Bar is the decision timeframe the signal that opened this order fired on (e.g. "5m", "1H").
	// Captured at open time rather than reconstructed from strategy_assignments afterward, since
	// one strategy can be assigned to several bars for the same instrument (CLAUDE.md §9) and the
	// assignment table alone can't say which bar THIS particular order came from. Empty for orders
	// opened before this field existed.
	Bar          string
	Side         string // "buy" or "sell"
	EntryPx      decimal.Decimal
	SLPx         *decimal.Decimal
	TPPx         *decimal.Decimal
	Size         decimal.Decimal
	Leverage     decimal.Decimal
	OpenedAt     time.Time
	ClosedAt     *time.Time
	CloseReason  *string // "sl", "tp", "manual", "timeout"
	ClosePx      *decimal.Decimal
	RealizedPnL  *decimal.Decimal
	FeaturesJSON json.RawMessage
	Mode         string // "paper", "demo", or "real" (CLAUDE.md §11.4); defaults to "paper"

	// PnLMaxPct/PnLMinPct are the peak and trough unrealized PnL this position has reached while
	// open (CLAUDE.md §15.11) — model input, not reporting. A trade that ran to 90% of its target
	// and gave it back is a different lesson from one that drifted sideways to the same current PnL.
	// Persisted rather than kept in memory because a restart would otherwise reset the high-water
	// mark to the current PnL, telling the model a round-tripped trade had never been in profit.
	// PnLMinPct is negative-ranged.
	PnLMaxPct decimal.Decimal
	PnLMinPct decimal.Decimal

	// ParentOrderID/Variant are what remains of the SL/TP shadow-fork mechanic (CLAUDE.md §15.4),
	// removed 2026-09-02 in favor of the RL agent editing an order's SL/TP in place
	// (RecordPaperOrderAdjustment is the audit trail that replaces the fork). Every order is now
	// Variant="baseline"/ParentOrderID=nil going forward; the columns and this Go-side pair stay
	// only so historical rows from before the change still scan correctly.
	ParentOrderID *int64
	Variant       string // "baseline" (default); "rl_adjusted" only appears in historical rows

	// StrategyName is joined in by ListPositions for display (CLAUDE.md §11.4's positions panel) —
	// not a stored column, and not populated by OpenPaperOrder/ListOpenPaperOrders. Empty when
	// StrategyID is nil (an order opened with no strategy attribution).
	StrategyName string

	// ManualCloseRequested is set by RequestManualClose (the panel's Close button, 2026-08-31
	// request) and checked by PaperTrader.monitorOpenOrders on its next tick — cmd/api runs in a
	// separate process and cannot run the real close path itself, so this is intent, not a close.
	ManualCloseRequested bool

	// ExchangeOrderID/ExchangeAlgoOrderID are OKX's own order IDs for a real trade (CLAUDE.md §27,
	// nil for paper/demo rows). ExchangeOrderID identifies the entry market order; ExchangeAlgoOrderID
	// identifies the resting SL/TP algo/conditional order placed immediately after — needed later
	// to amend or cancel it, since real trading edits that order in place rather than forking
	// (§27.3). Populated only by RealTrader's open path.
	ExchangeOrderID     *string
	ExchangeAlgoOrderID *string

	// Status is nil for paper/demo rows (which have no fill lifecycle — a paper order is always
	// instantly and fully filled) and set for rows sourced from real_orders (CLAUDE.md, real-
	// trading readiness plan, 2026-09-04): "pending", "partial", "filled", or "canceled". Lets
	// handleListPositions present RealOrder rows through the same DTO shape the panel already
	// consumes for paper/demo positions, without inventing a second response type.
	Status *string
}

// RealOrder is a real-money trade placed against the exchange (CLAUDE.md, real-trading readiness
// plan, 2026-09-04) — stored in its own table, separate from PaperOrder/paper_orders. This is a
// deliberate reversal of the earlier decision (§27.3/§27.7) to share paper_orders with mode='real':
// a real order has a fill lifecycle (Status) with no paper-trading equivalent (a paper order is
// always instantly and fully filled), so it needs its own home rather than a column that would mean
// nothing on every paper row. Every RealOrder IS mode="real" by construction — the table itself is
// the mode discriminator, there is no Mode field here.
//
// Mirrors PaperOrder field-for-field except: no ParentOrderID/Variant (real trading has no
// shadow-fork mechanic, §27.3) and no Mode (redundant by construction), plus the new Status field.
type RealOrder struct {
	ID         int64
	InstID     string
	StrategyID *int64
	Bar        string
	Side       string // "buy" or "sell"
	EntryPx    decimal.Decimal
	SLPx       *decimal.Decimal
	TPPx       *decimal.Decimal
	Size       decimal.Decimal
	Leverage   decimal.Decimal
	OpenedAt   time.Time
	ClosedAt   *time.Time
	CloseReason  *string // "sl", "tp", "manual", "timeout", "rl_early"
	ClosePx      *decimal.Decimal
	RealizedPnL  *decimal.Decimal
	FeaturesJSON json.RawMessage

	// Status tracks the fill lifecycle, independent of ClosedAt/CloseReason (which keep meaning
	// "the position was later closed by SL/TP/manual/timeout" — unchanged from PaperOrder).
	// "pending": order accepted by the exchange, fill not yet confirmed — written immediately so
	// an in-flight order is visible on the panel, not only after it resolves.
	// "partial": partially filled, remainder canceled by the fill timeout — a real, smaller
	// position exists.
	// "filled": fully filled — the normal case.
	// "canceled": never filled before the fill timeout — no position exists; the row stays so a
	// timed-out attempt is still visible rather than silently dropped.
	Status string

	PnLMaxPct decimal.Decimal
	PnLMinPct decimal.Decimal

	// StrategyName is joined in by ListRealPositions for display — not a stored column.
	StrategyName string

	ManualCloseRequested bool

	ExchangeOrderID     *string
	ExchangeAlgoOrderID *string
}

// AccountEquity is one trading mode's shared running balance (CLAUDE.md §15.6, revised
// 2026-08-28). This replaced the per-token sub-budgets: every token trades against ONE pool, and
// how much of it goes into any single position is the RL agent's decision (bounded by Go-side
// caps), not a config constant split evenly across tokens up front.
//
// Tracked as its own row rather than summed from paper_orders on every check so "the account is at
// zero" is a fact the system can act on directly, and so reset events (ResetCount/LastResetAt) stay
// visible for training-run analysis rather than looking like unlimited free money.
type AccountEquity struct {
	Mode       string          // "paper", "demo", or "real"
	InitialUSD decimal.Decimal // configured starting balance a reset returns to
	EquityUSD  decimal.Decimal // "Total Equity": running balance SINCE the last reset/cap choice
	// AccountBalanceUSD is "Account Balance": the real, continuous running total (CLAUDE.md
	// §31.2/§31.3). Moves by the exact same delta as EquityUSD on every trade (ApplyRealizedPnL),
	// AND moves to the exact same new value as EquityUSD on an operator-chosen cap (SetAccountCap,
	// which is economically a deposit/withdrawal — it changes the real balance too, not just the
	// baseline positions size against). It is NEVER independently reset the way EquityUSD is by an
	// automatic drain-to-zero (ApplyRealizedPnL's own reset path) — that is the one case where the
	// two fields actually diverge: EquityUSD snaps back to InitialUSD, AccountBalanceUSD keeps
	// recording the real (negative-going-forward) total. Outside of that one case, EquityUSD ==
	// AccountBalanceUSD is a structural invariant, since neither field in this schema ever carries
	// unrealized PnL.
	AccountBalanceUSD decimal.Decimal
	ResetCount        int
	LastResetAt       *time.Time
	UpdatedAt         time.Time
}

// EquityPoint is one entry in a mode's balance timeline (CLAUDE.md §15.7). Every balance change
// writes one, so a drain-and-reset that happens overnight is visible in the panel's chart
// afterward instead of only in logs nobody was watching at the time.
type EquityPoint struct {
	ID        int64
	Mode      string
	EquityUSD decimal.Decimal // balance AFTER this change
	DeltaUSD  decimal.Decimal // signed: realized PnL for a trade, top-up amount for a reset
	Reason    string          // "trade", "reset", or "seed"
	OrderID   *int64
	InstID    string
	CreatedAt time.Time
}

// PaperTradingConfig is the panel-editable control-box config for cmd/paper-trader (CLAUDE.md):
// pause/stop trading, disable one signal direction, restrict which strategy kinds/timeframes/
// tokens are active. Restart-required to apply, same posture as cmd/strategy-tester's own
// runtime config — cmd/paper-trader reads this fresh from Postgres at every start.
type PaperTradingConfig struct {
	TradingState    string // "running", "paused", or "stopped"
	DisableLong     bool
	DisableShort    bool
	ActiveKinds     []string // empty = no per-kind restriction
	DisabledInstIDs []string // empty = no token disabled
	ActiveBars      []string // empty = use paper_trading.bars from config.yaml as-is
	UpdatedAt       time.Time
}

// PaperTradingConfigPatch is SavePaperTradingConfig's input — nil fields leave the corresponding
// column unchanged, matching tester.RuntimeConfig's patch shape. The slice fields are pointers to
// a slice (not a bare slice) so "explicitly set to empty" (clear the restriction) is distinguishable
// from "field omitted" (leave whatever restriction is already saved untouched).
type PaperTradingConfigPatch struct {
	TradingState    *string
	DisableLong     *bool
	DisableShort    *bool
	ActiveKinds     *[]string
	DisabledInstIDs *[]string
	ActiveBars      *[]string
}

// ParamChange is one recorded strategy parameter-change event (CLAUDE.md §16): either
// cmd/strategy-optimizer persisting a winning tuned candidate (Source="optimizer") or an operator
// editing a sub-strategy's params by hand via the panel (Source="manual"). Backs the panel's
// price-chart marker-line view of "when did this token's strategy params last change."
type ParamChange struct {
	ID         int64
	StrategyID int64
	InstID     string
	OldConfig  json.RawMessage // nil if the strategy had no prior config (first-ever change)
	NewConfig  json.RawMessage
	Source     string // "optimizer" or "manual"
	CreatedAt  time.Time
}

// PaperOrderAdjustment is one row of a paper order's in-trade SL/TP adjustment history (CLAUDE.md
// §15.4/§15.12 revision, 2026-09-02): the RL SL/TP-adjust mechanic edits the order's SL/TP in
// place rather than forking, and each field it moves gets its own append-only row here so a
// click on the order in the panel can show exactly what changed and when.
type PaperOrderAdjustment struct {
	ID        int64
	OrderID   int64
	Field     string // "sl" or "tp"
	OldValue  *decimal.Decimal
	NewValue  *decimal.Decimal
	Source    string // "model", "optimizer", or "manual"
	CreatedAt time.Time
}

// Repository is the persistence port. internal/postgres implements this.
type Repository interface {
	SaveCandle(ctx context.Context, c Candle) error
	// ListCandles returns the most recent `limit` finalized candles for instID/bar, oldest first —
	// backs the panel's price-chart marker overlay (GET /api/candles, CLAUDE.md §16 point 6). Reads
	// the same durable `candles` hypertable PaperTrader writes to (CLAUDE.md §7); no separate
	// candle store.
	ListCandles(ctx context.Context, instID, bar string, limit int) ([]Candle, error)

	CreateStrategy(ctx context.Context, s StrategyConfig) (int64, error)
	GetStrategy(ctx context.Context, id int64) (StrategyConfig, error)
	ListStrategies(ctx context.Context, instID string, enabledOnly bool) ([]StrategyConfig, error)
	UpdateStrategyConfig(ctx context.Context, id int64, config json.RawMessage, enabled bool) error
	DeleteStrategy(ctx context.Context, id int64) error
	// ResetStrategyToOrigin overwrites a sub-strategy's Config with its origin's current Config.
	ResetStrategyToOrigin(ctx context.Context, id int64) error

	// CreateAssignment binds a strategy to an instrument+timeframe. Durable so a restart reloads
	// exactly which variant was running where (CLAUDE.md §11.3) instead of relying on in-code
	// wiring like cmd/paper-trader/main.go's current hardcoded []usecase.StrategyAssignment.
	CreateAssignment(ctx context.Context, a StrategyAssignment) (int64, error)
	// ListAssignments returns assignments for mode ("paper" or "real") — CLAUDE.md real-trading
	// readiness plan, 2026-09-04: paper and real trading each maintain independent assignments, so
	// a strategy tuned/enabled for one has no effect on the other.
	ListAssignments(ctx context.Context, instID string, enabledOnly bool, mode string) ([]StrategyAssignment, error)
	SetAssignmentEnabled(ctx context.Context, id int64, enabled bool) error
	DeleteAssignment(ctx context.Context, id int64) error

	// StrategyStatsFor computes per-strategy track record from paper_orders (CLAUDE.md §11.3).
	StrategyStatsFor(ctx context.Context, strategyID int64) (StrategyStats, error)

	// TokenStats24h computes each active token's last-24h paper-trading activity (position count,
	// PnL$, PnL%) for the panel's "Manage tokens" modal — one row per inst_id that has at least
	// one baseline trade closed in the window.
	TokenStats24h(ctx context.Context) ([]TokenStats, error)

	// OpenPaperOrder inserts o and returns its id. o.ExchangeOrderID is persisted when set (real
	// trading, CLAUDE.md §27) — the algo order's ID is not known until after this call returns
	// (PlaceAlgoOrder happens second), so it is written separately via SetExchangeAlgoOrderID.
	OpenPaperOrder(ctx context.Context, o PaperOrder) (int64, error)
	// GetPaperOrder fetches a single order by id, open or closed. Used by the manual SL/TP-edit
	// endpoint (CLAUDE.md §27.3's plan §3b) to resolve EntryPx/Leverage/Side before converting the
	// operator's percentage input to a price — the existing list-shaped reads (ListOpenPaperOrders/
	// ListPositions) are all the wrong shape for "fetch one order I already have the id of."
	GetPaperOrder(ctx context.Context, id int64) (PaperOrder, error)
	// SetExchangeAlgoOrderID records the resting SL/TP algo order's OKX-assigned ID on an already-
	// open real order (CLAUDE.md §27.3), so it can be amended/cancelled later. Real trading only.
	SetExchangeAlgoOrderID(ctx context.Context, id int64, algoOrderID string) error
	ClosePaperOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal) error
	// UpdatePaperOrderSLTP applies an in-trade SL/TP adjustment to an open order (CLAUDE.md §15.4).
	// Callers MUST have already run the proposed new prices through the ratchet clamp
	// (usecase.RatchetSLTP) before calling this — the repository does not re-validate the ratchet
	// constraint itself. This is now the ONLY path the RL SL/TP-adjust mechanic uses (2026-09-02
	// revision): it used to fork the order instead of editing it, but fork volume grew large
	// enough to distort per-strategy stats, so the mechanic now edits the one real order directly
	// and RecordPaperOrderAdjustment (below) is the audit trail that replaces the fork.
	UpdatePaperOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal) error
	ListOpenPaperOrders(ctx context.Context, instID string) ([]PaperOrder, error)
	// RequestManualClose flags an open order for the panel's manual close button (2026-08-31
	// request). cmd/api runs in a separate process from the PaperTrader that owns this order's
	// instrument, so it cannot run the real close path itself — it only sets this flag; PaperTrader
	// closes the order at the live price on its next tick (usecase.PaperTrader.monitorOpenOrders),
	// close_reason='manual', reported to the model as closed_early (conductor.TerminalCategory),
	// the same "decision-driven exit, trains something rather than nothing" treatment as a timeout
	// close (CLAUDE.md §15.14) — chosen over reporting nothing at all, since the model has no
	// closed_manual category to report a plain manual close under. Returns an error if id is not a
	// currently-open order.
	RequestManualClose(ctx context.Context, id int64) error
	// RequestManualCloseAll is RequestManualClose's bulk form, used when the operator sets
	// trading_state="stopped" from the panel's control box (CLAUDE.md): flags every open paper
	// order for close on its next tick. Returns how many rows were flagged.
	RequestManualCloseAll(ctx context.Context) (int, error)
	// UpdatePaperOrderPnLExtremes records new peak/trough unrealized PnL for an open order
	// (CLAUDE.md §15.11). Both are written together since they move as one high-water pair.
	UpdatePaperOrderPnLExtremes(ctx context.Context, id int64, maxPct, minPct decimal.Decimal) error
	// ListPositions returns positions (open and/or closed) across trading modes for the panel
	// (CLAUDE.md §11.4), filtered/sorted/paged per f.
	ListPositions(ctx context.Context, f PositionFilter) ([]PaperOrder, error)
	// CountPositions returns how many rows f's Mode/InstID/Open filters match, ignoring
	// SortBy/Limit/Offset — what the panel's pagination control needs to know the total page
	// count, without pulling every row back just to len() it.
	CountPositions(ctx context.Context, f PositionFilter) (int, error)

	// OpenRealOrder inserts a real order and returns its id (CLAUDE.md, real-trading readiness
	// plan, 2026-09-04). Callers MUST set o.Status explicitly (normally "pending" — see RealOrder's
	// doc comment) rather than relying on the column default, matching this codebase's existing
	// style of Go-side explicitness for values the caller already knows.
	OpenRealOrder(ctx context.Context, o RealOrder) (int64, error)
	// GetRealOrder fetches a single real order by id, mirroring GetPaperOrder.
	GetRealOrder(ctx context.Context, id int64) (RealOrder, error)
	// UpdateRealOrderStatus transitions a real order's fill status once PlaceOrder's outcome is
	// known: "filled" or "partial" (entryPx/size non-nil, corrected to the exchange-confirmed
	// avgPx/filled size) or "canceled" (both nil — the entry never filled, no position exists).
	UpdateRealOrderStatus(ctx context.Context, id int64, status string, entryPx *decimal.Decimal, size *decimal.Decimal) error
	// SetRealOrderFeatures records the decision-time observation snapshot, mirroring how
	// FeaturesJSON is set on PaperOrder — called once the fill/partial/canceled outcome is known.
	SetRealOrderFeatures(ctx context.Context, id int64, featuresJSON json.RawMessage) error
	// SetRealOrderExchangeAlgoOrderID mirrors SetExchangeAlgoOrderID for real_orders. Kept for
	// parity even though no resting exchange-side algo order is placed today (§27.3's correction:
	// real trading watches SL/TP in-process, the same mechanism paper trading uses).
	SetRealOrderExchangeAlgoOrderID(ctx context.Context, id int64, algoOrderID string) error
	// CloseRealOrder mirrors ClosePaperOrder.
	CloseRealOrder(ctx context.Context, id int64, closePx decimal.Decimal, reason string, realizedPnL decimal.Decimal) error
	// UpdateRealOrderSLTP mirrors UpdatePaperOrderSLTP — callers must have already clamped the
	// proposed levels (RatchetSLTP for a model-driven edit; no clamp at all for a manual/operator
	// edit, CLAUDE.md §27.7 commit 6) before calling this.
	UpdateRealOrderSLTP(ctx context.Context, id int64, slPx, tpPx *decimal.Decimal) error
	// ListOpenRealOrders mirrors ListOpenPaperOrders, restricted to Status IN ('filled','partial')
	// — a still-pending order is not yet a real position and must never be double-counted as one.
	ListOpenRealOrders(ctx context.Context, instID string) ([]RealOrder, error)
	// RequestRealManualClose mirrors RequestManualClose — flags an open real order for RealTrader's
	// own tick loop to close on its next tick (RealTrader.monitorOpenPositions), the same
	// intent-not-action pattern RequestManualClose uses since cmd/api runs in a separate process.
	RequestRealManualClose(ctx context.Context, id int64) error
	// UpdateRealOrderPnLExtremes mirrors UpdatePaperOrderPnLExtremes.
	UpdateRealOrderPnLExtremes(ctx context.Context, id int64, maxPct, minPct decimal.Decimal) error
	// ListRealPositions mirrors ListPositions for real_orders. f.Mode is ignored (every row is real
	// by construction); f.Open filters on Status IN ('filled','partial') AND ClosedAt IS NULL/NOT
	// NULL as appropriate — a "canceled" row is never "open" (nothing to be open) and is only ever
	// returned by a f.Open == nil (both) or explicit closed query, never an open-only one.
	ListRealPositions(ctx context.Context, f PositionFilter) ([]RealOrder, error)
	// CountRealPositions mirrors CountPositions.
	CountRealPositions(ctx context.Context, f PositionFilter) (int, error)
	// RecordRealOrderAdjustment mirrors RecordPaperOrderAdjustment, into real_order_adjustments.
	RecordRealOrderAdjustment(ctx context.Context, orderID int64, field string, oldValue, newValue *decimal.Decimal, source string) error
	// ListRealOrderAdjustments mirrors ListPaperOrderAdjustments. Reuses the PaperOrderAdjustment
	// shape (the fields are identical) rather than a parallel RealOrderAdjustment struct.
	ListRealOrderAdjustments(ctx context.Context, orderID int64) ([]PaperOrderAdjustment, error)

	// GetAccountEquity returns mode's current balance row, creating it (seeded at initialUSD, with
	// a reason="seed" history point) if it doesn't exist yet — CLAUDE.md §15.6.
	GetAccountEquity(ctx context.Context, mode string, initialUSD decimal.Decimal) (AccountEquity, error)
	// ApplyRealizedPnL adds pnl (signed) to mode's running balance and records an EquityPoint for
	// it. If the resulting equity is <= 0 AND mode is not "real", it is reset back to InitialUSD,
	// ResetCount/LastResetAt are recorded, and a second reason="reset" point is written
	// (CLAUDE.md §15.7's "give it another chance"). Real mode NEVER auto-resets — a drained real
	// account is a stop condition requiring a human decision, so it is left at/below zero and
	// reported as drained instead. Returns the updated row and whether a reset occurred.
	ApplyRealizedPnL(ctx context.Context, mode string, pnl decimal.Decimal, orderID *int64, instID string) (AccountEquity, bool, error)
	// ListEquityHistory returns mode's balance timeline for the panel's chart (CLAUDE.md §15.7),
	// oldest-first for direct plotting. A zero since means no lower bound; limit<=0 means no cap.
	ListEquityHistory(ctx context.Context, mode string, since time.Time, limit int) ([]EquityPoint, error)
	// SetAccountCap is an OPERATOR-TRIGGERED equivalent of the automatic drain-to-zero reset
	// ApplyRealizedPnL already performs (CLAUDE.md §31.2): sets both InitialUSD and EquityUSD to
	// newCapUSD, bumps ResetCount, stamps LastResetAt to now, and writes a reason="reset" history
	// point — the exact same schema/semantics an automatic reset uses, just invoked explicitly
	// (e.g. "I've decided to trade with $40 from this point on") rather than triggered by a drain.
	// This is deliberately NOT a new parallel "trading cap" concept: LastResetAt is what the panel's
	// equity chart and the dynamic per-position sizing (equity / active token count) both anchor to,
	// so this single value is the one and only definition of "the balance since I last chose one."
	SetAccountCap(ctx context.Context, mode string, newCapUSD decimal.Decimal) (AccountEquity, error)

	// RecordPaperOrderAdjustment appends one entry to an order's in-trade SL/TP adjustment history
	// (CLAUDE.md §15.4/§15.12 revision, 2026-09-02) — replaces the old shadow-fork mechanic's
	// implicit "look at the fork's levels" comparison with an explicit, append-only log so a click
	// on the order in the panel can show exactly what changed, when, and by whom (model, the
	// strategy-optimizer, or a manual edit). oldValue/newValue are nil-able since a field can be
	// set from nothing (first adjustment) or unset (not expected today, but the column allows it).
	RecordPaperOrderAdjustment(ctx context.Context, orderID int64, field string, oldValue, newValue *decimal.Decimal, source string) error
	// ListPaperOrderAdjustments returns orderID's adjustment history, oldest first — the shape the
	// panel's order-detail modal consumes to render a chronological change table.
	ListPaperOrderAdjustments(ctx context.Context, orderID int64) ([]PaperOrderAdjustment, error)

	// RecordParamChange appends one entry to a strategy's durable parameter-change timeline
	// (CLAUDE.md §16, §16.3 step 5) — called both by cmd/strategy-optimizer after persisting a
	// winning candidate and by cmd/api's manual strategy-update handler, so the panel's timeline
	// reflects every change regardless of source.
	RecordParamChange(ctx context.Context, c ParamChange) (int64, error)
	// ListParamChanges returns instID's parameter-change history at or after since (zero time =
	// no lower bound), oldest first — the shape the panel's marker-overlay chart consumes.
	ListParamChanges(ctx context.Context, instID string, since time.Time) ([]ParamChange, error)

	// GetPaperTradingConfig returns mode's ("paper" or "real") panel-editable control-box config
	// (CLAUDE.md real-trading readiness plan, 2026-09-04 — paper_trading_config is now one row per
	// mode), seeding it at column defaults if it hasn't been written yet.
	GetPaperTradingConfig(ctx context.Context, mode string) (PaperTradingConfig, error)
	// SavePaperTradingConfig applies patch's non-nil fields onto mode's row.
	SavePaperTradingConfig(ctx context.Context, mode string, patch PaperTradingConfigPatch) (PaperTradingConfig, error)
	// SetAssignmentsEnabledForKinds bulk-enables/disables mode's strategy_assignments so only
	// assignments whose strategy's Kind is in activeKinds are enabled — the global per-kind "active
	// strategies" toggle, scoped to one mode. A no-op when activeKinds is empty (no restriction
	// configured).
	SetAssignmentsEnabledForKinds(ctx context.Context, mode string, activeKinds []string) error
}
