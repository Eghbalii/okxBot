package domain

import "github.com/shopspring/decimal"

// ObservationSchemaVersion must be bumped whenever the Observation shape changes (new fields,
// changed StrategySignals/Timeframes widths, etc.) — CLAUDE.md §15.3. rl_service/serve/api.py
// checks this against what the loaded global model was trained for and rejects a mismatch rather
// than silently misaligning features into the wrong vector positions.
//
// v3: switched from one-agent-per-token to a single global agent (CLAUDE.md §15.1) — added
// ActiveTokens (for the token-identity one-hot) and PriceContext (raw price series +
// positional/distance features, CLAUDE.md §15.3) so the agent can reason about price action
// directly instead of only through strategies' interpretation of it.
//
// v4: replaced the per-token sub-budget fields (TokenEquityUSD/TokenBudgetUSD) with the
// shared-account fields AccountEquityUSD/AccountInitialUSD/OpenExposureUSD (CLAUDE.md §15.6's
// 2026-08-28 revision) — capital is one pool the agent sizes against, not a per-token constant.
//
// v5: the event-driven signal lifecycle (CLAUDE.md §15.10). Added Category, a single
// per-call Signal (carrying strategy kind, timeframe, live win rate and staleness), MarketContext
// and PositionState — and, critically, rl_service now actually feeds strategy signals and
// RecentTrades into the model's input vector, which v3/v4 never did despite both sides carrying
// them over the wire the whole time.
//
// v6: CLAUDE.md §15.11. Signal SL/TP became PRICES rather than percentages (a strategy
// derives a level from chart structure; a percentage discards that) and gained EntryPx; the two
// overlapping position blocks merged into one carrying PnLMax/PnLMin and age; price context gained
// the live forming candle's OHLC; MarketContext and RecentTrades dropped.
//
// v7 (current): the risk budget became an INPUT — MaxPositionPct/MaxLeverage, the same caps Go
// already enforces after the fact. Before this the model proposed a fraction of total equity while
// the risk layer independently clamped it, so the policy was optimising in a space its own
// constraints would overrule: offline training drove requested size to ~90% of equity because
// nothing it could see said that was impossible. Action size_pct is now read as a fraction OF the
// cap rather than of the account, which also means a cap change (10x -> 20x leverage, say) adapts
// the same policy instead of needing a retrain.
const ObservationSchemaVersion = 7

// ActionSchemaVersion tracks the Action's layout independently of the observation's — a model
// trained against a narrower action space can't answer the full CLAUDE.md §15.4 action, and that's
// a different failure from an observation-width mismatch. v1 was the original 2-output
// [TargetExposure, LeverageFrac]; v2 is the full action below, including SLAdjustPct/TPAdjustPct
// and the fixed-width StrategyWeights slots. Must match ACTION_SCHEMA_VERSION in
// rl_service/obs.py, which rejects an incompatible loaded model with a 503 rather than returning
// neutral defaults that would look like a working model proposing no adjustments.
//
// v3: dropped StrategyWeights and added an order-action head. Dropping the weights is what removed
// the fixed ceiling on strategy count — with one signal per call, nothing in the action scales with
// the roster, so strategies can be added or removed without an action-space change or a retrain.
//
// v4 (current): the model SETS SLPx/TPPx as prices rather than proposing percentage adjustments,
// and one Action field covers the whole lifecycle (CLAUDE.md §15.11).
const ActionSchemaVersion = 4

// Signal lifecycle categories (CLAUDE.md §15.10) — which decision a /predict call is asking for.
// Must match SIGNAL_CATEGORIES in rl_service/obs.py, including order (it defines the one-hot index).
const (
	CategoryBuy         = "buy"          // strategy fired, no open position -> open or skip
	CategorySell        = "sell"         // same, short side
	CategoryUpdate      = "update"       // position open: new signal, or PnL moved past the threshold
	CategoryClosedTP    = "closed_tp"    // terminal: take-profit hit
	CategoryClosedSL    = "closed_sl"    // terminal: stop-loss hit
	CategoryClosedEarly = "closed_early" // terminal: the model closed it before either level
)

// Action values the model returns (CLAUDE.md §15.11). Deliberately named to match the request
// categories above so the same word means the same thing on both sides of the call. Which are
// valid depends on the request's category — the model always emits one and the controller accepts
// it only where it makes sense (a "close" on a buy call has nothing to close):
//
//	buy / sell -> ActionOpen | ActionSkip
//	update     -> ActionNone | ActionUpdate | ActionClose
//	closed_*   -> ignored; that call exists to deliver reward, not to ask anything
//
// Must match ACTIONS in rl_service/obs.py, including order (it defines the argmax index).
const (
	ActionOpen   = "open"
	ActionSkip   = "skip"
	ActionNone   = "none"
	ActionUpdate = "update"
	ActionClose  = "close"
)

// IsTerminalCategory reports whether c is a close event. Terminal calls carry the realized outcome
// and are what the reward is computed from — the close event IS the reward (CLAUDE.md §15.10), so
// the model is being informed rather than asked to decide anything.
func IsTerminalCategory(c string) bool {
	return c == CategoryClosedTP || c == CategoryClosedSL || c == CategoryClosedEarly
}

// StrategySignal is one strategy's read, and the single signal a /predict call is about
// (CLAUDE.md §15.10). Mirrors strategy.Signal's fields, kept as a separate domain type so domain
// has no dependency on the strategy package.
type StrategySignal struct {
	StrategyID int64           `json:"strategy_id"`
	Side       string          `json:"side"` // "buy", "sell", or "" for hold
	Confidence decimal.Decimal `json:"confidence"`

	// PRICES, not percentages (CLAUDE.md §15.11). A strategy derives these from chart structure —
	// a stop below a swing low, a target at a fair-value gap — and expressing a level as a
	// percentage discards exactly the structure that produced it. rl_service vectorizes them as
	// offsets from the live price, so one shared policy still generalizes across instruments.
	EntryPx decimal.Decimal `json:"entry_px"`
	SLPx    decimal.Decimal `json:"sl_px"`
	TPPx    decimal.Decimal `json:"tp_px"`

	// Kind/Bar are what let one shared policy tell signals apart (CLAUDE.md §15.10): which strategy
	// produced this and on which timeframe. Kind is the strategy.Factories registry name, NOT the
	// strategies-table row id — identity has to be stable across deployments and across cloned
	// sub-strategies, and a row id is neither.
	Kind string `json:"kind"`
	Bar  string `json:"bar"`

	// WinRate/TradeCount are this strategy's realized track record on this instrument, fed as
	// model INPUT so it can learn to discount weak strategies. This is what replaced the
	// strategy_weights output: a measured win rate answers "how much do I trust this" better than
	// a score the model invents, and it costs no action-space width — which is what lets the
	// strategy roster change without retraining.
	WinRate    decimal.Decimal `json:"win_rate"`
	TradeCount int             `json:"trade_count"`
}

// PositionState is the open position a call is about, for `update` and terminal categories
// (CLAUDE.md §15.10). Zero-valued on a buy/sell call, where the decision is whether to open at all.
type PositionState struct {
	PositionOpen bool            `json:"position_open"`
	Side         decimal.Decimal `json:"side"` // +1 long, -1 short, 0 flat
	SizeUSD      decimal.Decimal `json:"size_usd"`
	Leverage     decimal.Decimal `json:"leverage"`

	// AgeSeconds is what separates "+30% in 10 minutes" from "-5% after 4 hours" — current PnL
	// alone cannot express that difference, and they are very different trades.
	AgeSeconds int64 `json:"age_seconds"`

	UnrealizedPnLPct decimal.Decimal `json:"unrealized_pnl_pct"`
	// How far this position travelled in each direction, not just where it sits now (CLAUDE.md
	// §15.11). A trade that reached 90% of its target and gave it all back teaches something
	// completely different from one that drifted sideways to the same current PnL. PnLMinPct is
	// negative-ranged.
	PnLMaxPct decimal.Decimal `json:"pnl_max_pct"`
	PnLMinPct decimal.Decimal `json:"pnl_min_pct"`

	DistToSLPct decimal.Decimal `json:"dist_to_sl_pct"`
	DistToTPPct decimal.Decimal `json:"dist_to_tp_pct"`

	// RealizedPnLUSD is meaningful only on a terminal category, where it IS the reward signal.
	// Entry/SL/TP are not repeated here — they are already on the signal, and duplicating them
	// would spend input width on the same numbers twice (CLAUDE.md §15.11).
	RealizedPnLUSD decimal.Decimal `json:"realized_pnl_usd"`

	// IsFork marks a shadow fork, which tracks its baseline parent rather than committing separate
	// capital (CLAUDE.md §15.4). Fork outcomes are compared against their baseline, so the model
	// should know which it is reasoning about.
	IsFork bool `json:"is_fork"`
}

// PriceContext is the raw/near-raw price-action window for one timeframe (CLAUDE.md §15.3),
// distinct from the derived FEATURE_COLUMNS-style features in TimeframeBlock.Features — this
// exists specifically so the agent can reason about price shape/levels directly, on equal footing
// with strategy signals, rather than only through what a strategy chose to report. Prices are
// normalized as pct-change returns (ClosePctChanges), not raw dollar values, so the vector
// generalizes across tokens/price regimes; distances are also fractions of price, not absolute.
type PriceContext struct {
	// Open/High/Low/Close of the LIVE FORMING candle, not the last closed one (CLAUDE.md §15.11).
	// OKX pushes the in-progress bar on the same WS channel, so this needs no extra REST call; on a
	// 1H timeframe the last *closed* candle can be 59 minutes stale, which is the same freshness
	// problem §15.9's audit found on the SL/TP path.
	Open  decimal.Decimal `json:"open"`
	High  decimal.Decimal `json:"high"`
	Low   decimal.Decimal `json:"low"`
	Close decimal.Decimal `json:"close"`

	// ClosePctChanges is the most-recent-last window of bar-over-bar close returns, e.g.
	// [(c[t-N]-c[t-N-1])/c[t-N-1], ..., (c[t]-c[t-1])/c[t-1]].
	ClosePctChanges []decimal.Decimal `json:"close_pct_changes"`
	// DistToSwingHighPct/DistToSwingLowPct: (swingPrice - midPrice) / midPrice over the recent
	// window — a cheap proxy for "how close is price to a level a discretionary trader would watch
	// on a chart" (e.g. a channel boundary), signed so the agent can tell direction, not just
	// magnitude.
	DistToSwingHighPct decimal.Decimal `json:"dist_to_swing_high_pct"`
	DistToSwingLowPct  decimal.Decimal `json:"dist_to_swing_low_pct"`
}

// TimeframeBlock bundles one timeframe's strategy signals, derived indicator features, and raw
// price context (CLAUDE.md §15.3) into the observation.
type TimeframeBlock struct {
	Bar             string            `json:"bar"`
	StrategySignals []StrategySignal  `json:"strategy_signals"`
	Features        []decimal.Decimal `json:"features"` // reuses rl_service's FEATURE_COLUMNS shape
	PriceContext    PriceContext      `json:"price_context"`
}

// Observation is the feature vector sent to the RL service for one inference step. As of v3
// (CLAUDE.md §15.1) this is served by a single global agent rather than a per-token model — InstID
// plus ActiveTokens is what lets that one shared policy condition its behavior on which token this
// request is for (a one-hot: the caller sets the InstID's position and leaves the rest zero).
// Field names/JSON tags match rl_service's observation construction — keep both sides in sync.
// CLAUDE.md §15.3.
type Observation struct {
	SchemaVersion int    `json:"schema_version"`
	InstID        string `json:"inst_id"`
	// ActiveTokens is the ordered roster the token-identity one-hot is built against — must match
	// what the global model was trained with (order matters: it defines each slot's index).
	ActiveTokens []string `json:"active_tokens"`
	// LastPrice is the current live price for this token — the most recent tick, not a candle
	// close (renamed from MidPrice, which read as "bid/ask midpoint" and didn't reflect what's
	// actually sent: OKX's tickers channel "last" trade price). CLAUDE.md's 2026-08-27 MidPrice
	// freshness audit found the SL/TP-adjust caller was passing a candle-close price here instead
	// of the live tick — see usecase/lifecycle.go's tick-driven runUpdates for the fix.
	LastPrice  decimal.Decimal  `json:"last_price"`
	Timeframes []TimeframeBlock `json:"timeframes"`

	// Category is which lifecycle decision this call represents — see the Category* constants
	// (CLAUDE.md §15.10). This is what tells the model whether it is being asked to open, to manage
	// an open position, or simply being told how one ended.
	Category string `json:"category"`
	// Signal is the single strategy signal this call is about. One signal per call, so the model
	// always knows exactly which strategy and timeframe it is answering. Nil on a price-driven
	// update, where no strategy spoke and only price/PnL moved — rl_service flags that explicitly
	// rather than sending ambiguous zeros.
	Signal        *StrategySignal `json:"signal,omitempty"`
	PositionState PositionState   `json:"position_state"`
	// OrderID ties a decision back to the position it was about, so an outcome landing much later
	// can be paired with the observation that produced it. Not fed to the model (an id has no
	// ordinal meaning) — carried for bookkeeping and training-time pairing.
	OrderID int64 `json:"order_id"`

	Position         decimal.Decimal `json:"position"` // signed current position size
	CurrentLeverage  decimal.Decimal `json:"current_leverage"`
	UnrealizedPnLPct decimal.Decimal `json:"unrealized_pnl_pct"`
	// DistToSLPct/DistToTPPct: (slPx/tpPx - midPrice) / midPrice for the currently open position,
	// zero if flat — direct input for the sl_adjust_pct/tp_adjust_pct decision (CLAUDE.md §15.3).
	DistToSLPct decimal.Decimal `json:"dist_to_sl_pct"`
	DistToTPPct decimal.Decimal `json:"dist_to_tp_pct"`

	// AccountEquityUSD/AccountInitialUSD are the SHARED account balance every token trades against
	// (CLAUDE.md §15.6, revised 2026-08-28 — these replaced the per-token TokenEquityUSD/
	// TokenBudgetUSD sub-budgets). The agent sees the real running balance and its starting point,
	// which is what makes TargetExposure a meaningful "how much of my account do I commit here"
	// decision rather than a fraction of a per-token constant. Reward/training attribution stays
	// per token (§15.5) — that's independent of capital being pooled.
	AccountEquityUSD  decimal.Decimal `json:"account_equity_usd"`
	AccountInitialUSD decimal.Decimal `json:"account_initial_usd"`
	// OpenExposureUSD is the summed notional of all currently-open baseline positions across every
	// token, so the agent can see how much of the shared account is already committed before asking
	// for more — without it, one policy serving N tokens has no way to avoid over-committing.
	OpenExposureUSD decimal.Decimal `json:"open_exposure_usd"`

	// MaxPositionPct/MaxLeverage are the RISK BUDGET this decision has to fit inside (schema v7) —
	// the same caps risk/conductor enforce on the way back out, handed to the model on the way in.
	// The model's size_pct is interpreted as a fraction OF MaxPositionPct rather than of the whole
	// account, so "give me everything I'm allowed" is a bounded, sane request instead of one the
	// risk layer has to overrule. Feeding the caps rather than baking them into the output scaling
	// is what keeps a later change (raising MaxLeverage, adding tokens) working without a retrain.
	MaxPositionPct decimal.Decimal `json:"max_position_pct"`
	MaxLeverage    decimal.Decimal `json:"max_leverage"`

	Features []decimal.Decimal `json:"features"` // legacy flat window, kept for the no-op/pre-Phase-A path
}

// Action is the RL agent's decision returned by the inference service, per token (CLAUDE.md
// §15.4). StrategyWeights is keyed by StrategyID (as a string, for JSON map-key compatibility)
// and should be treated as relative trust scores over the strategies present in the request's
// Observation.Timeframes — the caller normalizes/combines them with each strategy's own Confidence
// to produce the effective directional signal.
type Action struct {
	// ActionSchemaVersion is echoed by rl_service so a mismatch is visible in logs/panel; the
	// service itself refuses to serve an incompatible model, so this is a diagnostic, not a gate.
	ActionSchemaVersion int `json:"action_schema_version"`

	// Action is the model's decision — one of the Action* constants above (CLAUDE.md §15.11).
	Action string `json:"action"`

	// Side is which way to open, for callers that have no strategy layer to take direction from.
	// The paper path IGNORES this: there, direction belongs to the strategy that produced the
	// signal (§9/§16.1) and the model only sizes the trade. cmd/trader has no strategy feeding it
	// (§14's open live-wiring item), so it polls the model directly and needs a side from
	// somewhere. Empty means long.
	Side string `json:"side"`

	// SLPx/TPPx are PRICE LEVELS the model sets, not percentage nudges (CLAUDE.md §15.11): on an
	// open it places the initial stop/target, on an update it moves them. The caller MUST still
	// clamp these — minimum/maximum stop distance, a minimum TP:SL ratio — and run an update
	// through the ratchet so a stop can only tighten. Never trust the model's own output as the
	// safety boundary; early in training it is effectively random.
	SLPx decimal.Decimal `json:"sl_px"`
	TPPx decimal.Decimal `json:"tp_px"`

	// SizePct is the fraction of account equity to commit; LeverageFrac in [0, 1] maps to
	// [1x, MaxLeverage].
	SizePct      decimal.Decimal `json:"size_pct"`
	LeverageFrac decimal.Decimal `json:"leverage_frac"`

	// OrderID is echoed from the request so the controller can pair a response to the position it
	// asked about.
	OrderID int64 `json:"order_id"`

	Confidence decimal.Decimal `json:"confidence"`
}
