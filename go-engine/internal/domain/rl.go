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
// v4 (current): replaced the per-token sub-budget fields (TokenEquityUSD/TokenBudgetUSD) with the
// shared-account fields AccountEquityUSD/AccountInitialUSD/OpenExposureUSD (CLAUDE.md §15.6's
// 2026-08-28 revision) — capital is one pool the agent sizes against, not a per-token constant.
const ObservationSchemaVersion = 4

// ActionSchemaVersion tracks the Action's layout independently of the observation's — a model
// trained against a narrower action space can't answer the full CLAUDE.md §15.4 action, and that's
// a different failure from an observation-width mismatch. v1 was the original 2-output
// [TargetExposure, LeverageFrac]; v2 is the full action below, including SLAdjustPct/TPAdjustPct
// and the fixed-width StrategyWeights slots. Must match ACTION_SCHEMA_VERSION in
// rl_service/obs.py, which rejects an incompatible loaded model with a 503 rather than returning
// neutral defaults that would look like a working model proposing no adjustments.
const ActionSchemaVersion = 2

// MaxStrategySlots mirrors MAX_STRATEGY_SLOTS in rl_service/obs.py: the policy emits this many
// fixed-width strategy-weight slots, mapped positionally onto the strategy signals in the request's
// Observation.Timeframes (assignment order). Strategy signals past this count get no weight, so an
// assignment set larger than this needs both sides raised and the model retrained.
const MaxStrategySlots = 8

// StrategySignal is one assigned strategy's current read for one timeframe, feeding into the
// per-token agent's strategy_weights action (CLAUDE.md §15.3, §15.4). Mirrors strategy.Signal's
// fields (kept as a separate domain type so domain has no dependency on the strategy package).
type StrategySignal struct {
	StrategyID int64           `json:"strategy_id"`
	Side       string          `json:"side"` // "buy", "sell", or "" for hold
	Confidence decimal.Decimal `json:"confidence"`
	SLPct      decimal.Decimal `json:"sl_pct"`
	TPPct      decimal.Decimal `json:"tp_pct"`
}

// PriceContext is the raw/near-raw price-action window for one timeframe (CLAUDE.md §15.3),
// distinct from the derived FEATURE_COLUMNS-style features in TimeframeBlock.Features — this
// exists specifically so the agent can reason about price shape/levels directly, on equal footing
// with strategy signals, rather than only through what a strategy chose to report. Prices are
// normalized as pct-change returns (ClosePctChanges), not raw dollar values, so the vector
// generalizes across tokens/price regimes; distances are also fractions of price, not absolute.
type PriceContext struct {
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

// RecentTrade is one closed paper order's outcome, part of the recent-performance tail (CLAUDE.md
// §15.3) that lets the agent itself learn to size down after a losing streak.
type RecentTrade struct {
	RealizedPnLUSD decimal.Decimal `json:"realized_pnl_usd"`
	Win            bool            `json:"win"` // close_reason == "tp"
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
	// of the live tick — see usecase/papertrade.go's tick-driven adjustOpenOrdersWithRL for the fix.
	LastPrice  decimal.Decimal  `json:"last_price"`
	Timeframes []TimeframeBlock `json:"timeframes"`

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

	RecentTrades []RecentTrade `json:"recent_trades"` // most-recent-last

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

	// StrategyWeights are normalized to sum to 1 across the strategies present in the request.
	StrategyWeights map[string]decimal.Decimal `json:"strategy_weights"`

	TargetExposure decimal.Decimal `json:"target_exposure"` // in [-1, 1]
	LeverageFrac   decimal.Decimal `json:"leverage_frac"`   // in [0, 1], mapped to [1x, max_leverage]

	// SLAdjustPct/TPAdjustPct are proposed in-trade adjustments to an already-open position's
	// SL/TP, evaluated at candle-close cadence in Phase A (CLAUDE.md §15.4). The caller MUST clamp
	// these through a ratchet (only tighten toward locking in profit / reducing risk, never widen)
	// before applying them — never trust the model's own output as that safety boundary.
	SLAdjustPct decimal.Decimal `json:"sl_adjust_pct"`
	TPAdjustPct decimal.Decimal `json:"tp_adjust_pct"`

	Confidence decimal.Decimal `json:"confidence"`
}
