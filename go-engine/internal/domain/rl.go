package domain

import "github.com/shopspring/decimal"

// ObservationSchemaVersion must be bumped whenever the Observation shape changes (new fields,
// changed StrategySignals/Timeframes widths, etc.) — CLAUDE.md §15.3. rl_service/serve/api.py
// checks this against what the loaded global model was trained for and rejects a mismatch rather
// than silently misaligning features into the wrong vector positions.
//
// v3 (current): switched from one-agent-per-token to a single global agent (CLAUDE.md §15.1) —
// added ActiveTokens (for the token-identity one-hot) and PriceContext (raw price series +
// positional/distance features, CLAUDE.md §15.3) so the agent can reason about price action
// directly instead of only through strategies' interpretation of it.
const ObservationSchemaVersion = 3

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
	ActiveTokens []string         `json:"active_tokens"`
	MidPrice     decimal.Decimal  `json:"mid_price"`
	Timeframes   []TimeframeBlock `json:"timeframes"`

	Position         decimal.Decimal `json:"position"` // signed current position size
	CurrentLeverage  decimal.Decimal `json:"current_leverage"`
	UnrealizedPnLPct decimal.Decimal `json:"unrealized_pnl_pct"`
	// DistToSLPct/DistToTPPct: (slPx/tpPx - midPrice) / midPrice for the currently open position,
	// zero if flat — direct input for the sl_adjust_pct/tp_adjust_pct decision (CLAUDE.md §15.3).
	DistToSLPct decimal.Decimal `json:"dist_to_sl_pct"`
	DistToTPPct decimal.Decimal `json:"dist_to_tp_pct"`

	// TokenEquityUSD/TokenBudgetUSD are this token's own sub-budget (CLAUDE.md §15.6/§15.7), not
	// total account equity — reward/training stays attributed per token (§15.5) even though one
	// shared policy serves every token's requests.
	TokenEquityUSD decimal.Decimal `json:"token_equity_usd"`
	TokenBudgetUSD decimal.Decimal `json:"token_budget_usd"`

	RecentTrades []RecentTrade `json:"recent_trades"` // most-recent-last

	Features []decimal.Decimal `json:"features"` // legacy flat window, kept for the no-op/pre-Phase-A path
}

// Action is the RL agent's decision returned by the inference service, per token (CLAUDE.md
// §15.4). StrategyWeights is keyed by StrategyID (as a string, for JSON map-key compatibility)
// and should be treated as relative trust scores over the strategies present in the request's
// Observation.Timeframes — the caller normalizes/combines them with each strategy's own Confidence
// to produce the effective directional signal.
type Action struct {
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
