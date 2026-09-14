package domain

import "github.com/shopspring/decimal"

// ObservationSchemaVersion must be bumped whenever the Observation shape changes — CLAUDE.md
// §15.3. rl_service checks it against what the loaded model was trained for and rejects a mismatch
// rather than silently misaligning features into the wrong vector positions.
//
// v3: single global agent (CLAUDE.md §15.1) — token-identity one-hot and raw price context.
// v4: shared-account fields replaced the per-token sub-budgets (§15.6).
// v5: the event-driven signal lifecycle (§15.10) — and strategy signals actually reached the vector.
// v6: signal SL/TP became prices rather than percentages; merged position block (§15.11).
// v7: the risk budget (MaxPositionPct/MaxLeverage) became an input.
//
// v8 (current): the observation was audited before a from-scratch retrain and found to be feeding
// the model almost no market data at all (docs/RL_V8_PLAN.md). Of 91 inputs, SIX described the
// market. Three compounding defects: rl_service's to_vector padded/truncated the feature block to
// whatever the loaded model wanted, so the live candle's OHLC and 6 of 10 returns were cut on every
// call; TimeframeBlock.Features — the derived indicators — was never populated here at all, so the
// policy had no RSI, no volatility and no volume since the field was created; and the token one-hot
// was 16 slots against a roster that reached 65, most of them indistinguishable zeros.
//
// So v8 removes every identity one-hot. A token is described by what it IS (TokenProfile), a
// strategy by its measured record, a timeframe by an ordered scalar — none has a ceiling, and a
// token discovered tomorrow is comprehensible from its first candle rather than being an unlearned
// slot. Adds ten indicators per timeframe and a BTC reference block: every prior input was
// intra-token, so the model could never see that an altcoin reverses when BTC's candle turns red.
//
// The width is now EXACT on both sides. A caller whose data is incomplete must SKIP the model call
// rather than send a short observation — see Observation.Validate.
const ObservationSchemaVersion = 8

// ActionSchemaVersion tracks the Action's layout independently of the observation's — a model
// trained against a different action space is a different failure from a width mismatch.
//
// v1: [TargetExposure, LeverageFrac]. v2: SL/TP adjust + fixed strategy-weight slots. v3: dropped
// the weights (which is what removed the ceiling on strategy count) and added an action head.
// v4: the model SETS SLPx/TPPx as prices; one 5-wide head.
//
// v5 (current): the head is SPLIT IN TWO. Masking it at serving time was not enough — decode_action
// masked the argmax to the category's legal actions (§16.9) and that works, but SAC trains on the
// RAW vector, so on a buy call reward reached all five outputs including the discarded ones. An
// output rewarded without having caused anything feels no corrective pressure and drifts to the
// tanh bound, which is the state §54.8 measured across all nine outputs.
const ActionSchemaVersion = 5

// Signal lifecycle categories (CLAUDE.md §15.10) — which decision a /predict call is asking for.
// Must match SIGNAL_CATEGORIES in rl_service/obs.py.
//
// v8 note: CategoryClosedTimeout and CategoryClosedManual are new. §15.14 and §20 both had to force
// those closes under CategoryClosedEarly because widening a one-hot meant a schema bump on both
// sides — collapsing 358 trades (a fifth of all closes) into one label covering three genuinely
// different things: the model choosing to exit, the system timing out, and a person intervening.
const (
	CategoryBuy           = "buy"            // strategy fired, no open position -> open or skip
	CategorySell          = "sell"           // same, short side
	CategoryUpdate        = "update"         // position open: new signal, or PnL moved past the threshold
	CategoryClosedTP      = "closed_tp"      // terminal: take-profit hit
	CategoryClosedSL      = "closed_sl"      // terminal: stop-loss hit
	CategoryClosedEarly   = "closed_early"   // terminal: the model chose to close
	CategoryClosedTimeout = "closed_timeout" // terminal: held past the maximum duration
	CategoryClosedManual  = "closed_manual"  // terminal: an operator closed it
)

// Action values the model returns (CLAUDE.md §15.11). Which are valid depends on the request's
// category:
//
//	buy / sell -> ActionOpen | ActionSkip
//	update     -> ActionNone | ActionUpdate | ActionClose
//	closed_*   -> ignored; that call exists to deliver reward, not to ask anything
const (
	ActionOpen   = "open"
	ActionSkip   = "skip"
	ActionNone   = "none"
	ActionUpdate = "update"
	ActionClose  = "close"
)

// IsTerminalCategory reports whether c is a close event. Terminal calls carry the realized outcome
// and are what the reward is computed from — the close event IS the reward (CLAUDE.md §15.10).
func IsTerminalCategory(c string) bool {
	switch c {
	case CategoryClosedTP, CategoryClosedSL, CategoryClosedEarly,
		CategoryClosedTimeout, CategoryClosedManual:
		return true
	}
	return false
}

// IsZeroRewardCategory reports whether a terminal call should deliver no reward. A manual close is
// an operator's action, and attributing it to the policy would train it on a decision it never made
// (CLAUDE.md §15.12). The call still happens so the learner's pending decision resolves rather than
// leaking, but no gradient follows.
func IsZeroRewardCategory(c string) bool { return c == CategoryClosedManual }

// IndicatorsPerTimeframe is the exact number of derived indicators every MarketBlock must carry.
// Must match INDICATORS_PER_TIMEFRAME in rl_service/obs.py. Order is fixed and defined by
// usecase.BuildIndicators.
const IndicatorsPerTimeframe = 10

// ReturnsWindow is the exact number of bar-over-bar close returns every market block must carry.
// Must match RETURNS_WINDOW in rl_service/obs.py.
//
// EXACT, not "at most": a window one short changes the vector width, which in v7 was silently
// absorbed by padding. A caller without enough candles skips the model call.
const ReturnsWindow = 10

// StrategySignal is one strategy's read, and the single signal a /predict call is about
// (CLAUDE.md §15.10). Mirrors strategy.Signal's fields, kept separate so domain has no dependency
// on the strategy package.
type StrategySignal struct {
	StrategyID int64  `json:"strategy_id"`
	Side       string `json:"side"` // "buy", "sell", or "" for hold

	// PRICES, not percentages (CLAUDE.md §15.11). A strategy derives these from chart structure —
	// a stop below a swing low, a target at a fair-value gap — and expressing a level as a
	// percentage discards exactly the structure that produced it.
	EntryPx decimal.Decimal `json:"entry_px"`
	SLPx    decimal.Decimal `json:"sl_px"`
	TPPx    decimal.Decimal `json:"tp_px"`

	// Kind/Bar identify the signal. Kind is the strategy.Factories registry name, NOT the
	// strategies-table row id — identity has to be stable across deployments and cloned
	// sub-strategies, and a row id is neither. As of v8 neither is one-hot encoded: Kind is not fed
	// to the model at all (its BEHAVIOUR is, below) and Bar becomes one ordered scalar.
	Kind string `json:"kind"`
	Bar  string `json:"bar"`

	// This strategy's MEASURED record on this instrument. In v7 these sat beside a hardcoded
	// Confidence that contradicted them — grid_like declared 1.0 while running a 35% win rate and
	// -$4.62 of PnL. Confidence is gone (it was computed by only 2 of 26 kinds, so for the rest it
	// was the kind one-hot in a single number); these are what remains, because they are derived
	// from what actually happened.
	WinRate        decimal.Decimal `json:"win_rate"`
	TradeCount     int             `json:"trade_count"`
	AvgRR          decimal.Decimal `json:"avg_rr"`            // average reward:risk it proposes
	AvgHoldHours   decimal.Decimal `json:"avg_hold_hours"`    // average time its trades stay open
	AvgPnLPerTrade decimal.Decimal `json:"avg_pnl_per_trade"` // normalized by position size
}

// TokenProfile describes what an instrument IS, replacing v7's 16-slot identity one-hot
// (docs/RL_V8_PLAN.md).
//
// The one-hot could not survive a growing roster: 65 tokens against 16 slots left most of them as
// indistinguishable zeros, and re-sorting the roster on every discovery scan reassigned the slots
// that did work. More fundamentally, identity is the wrong input — the model should learn
// "high-volatility, thin-volume instruments need wider stops", which transfers to a token
// discovered tomorrow, not "PEPE behaves like this", which never does.
type TokenProfile struct {
	// TypicalVolatility is ATR/price averaged over the window — the single biggest difference
	// between a BTC and a PEPE, and what should drive stop distance.
	TypicalVolatility decimal.Decimal `json:"typical_volatility"`
	// LogVolume24h is log10 of 24h USD volume: liquidity, which §33.2 found to be the binding
	// constraint on what this bot can actually trade.
	LogVolume24h decimal.Decimal `json:"log_volume_24h"`
	VolumeRank   decimal.Decimal `json:"volume_rank"` // within the roster, 0..1
	// LogPrice is log10 price: tick behaviour differs at 0.000003 and at 90000.
	LogPrice  decimal.Decimal `json:"log_price"`
	Range24h  decimal.Decimal `json:"range_24h"`
	Change24h decimal.Decimal `json:"change_24h"`
	// LogTradeCount is log1p of this token's own recorded trades — "how much do I know here".
	LogTradeCount decimal.Decimal `json:"log_trade_count"`
}

// MarketBlock is one timeframe's market state: derived indicators plus raw price action.
//
// Both halves matter and neither replaces the other. The indicators are what a trader reads off a
// chart (is this volatile? is volume unusual? is it overbought?); the returns and OHLC are the raw
// shape those are computed from, kept so the policy can see structure the fixed indicator set does
// not capture.
type MarketBlock struct {
	Bar string `json:"bar"`

	// Indicators is EXACTLY IndicatorsPerTimeframe values in the order BuildIndicators defines.
	// This is the field v7 declared, parsed, and never populated — the model had no RSI, no
	// volatility and no volume for the entire life of the schema.
	Indicators []decimal.Decimal `json:"indicators"`

	// OHLC of the LIVE FORMING candle, not the last closed one (CLAUDE.md §15.11). OKX pushes the
	// in-progress bar on the same WS channel, so this needs no extra REST call; on a 1H timeframe
	// the last *closed* candle can be 59 minutes stale.
	Open  decimal.Decimal `json:"open"`
	High  decimal.Decimal `json:"high"`
	Low   decimal.Decimal `json:"low"`
	Close decimal.Decimal `json:"close"`

	// ClosePctChanges is EXACTLY ReturnsWindow bar-over-bar close returns, oldest first.
	ClosePctChanges []decimal.Decimal `json:"close_pct_changes"`

	// DistToSwingHighPct/DistToSwingLowPct: (swing - price) / price over the recent window — a
	// cheap proxy for "how close is price to a level a discretionary trader would watch", signed so
	// the model can tell direction, not just magnitude.
	DistToSwingHighPct decimal.Decimal `json:"dist_to_swing_high_pct"`
	DistToSwingLowPct  decimal.Decimal `json:"dist_to_swing_low_pct"`
}

// BTCContext is the wider market (docs/RL_V8_PLAN.md).
//
// Every other input in this observation is intra-token. The operator's own observation is why this
// exists: an altcoin can be cleanly trending and reverse the moment BTC's candle turns red. Without
// it the model would have to infer a market-wide regime from one instrument's price, which it
// cannot do. The last entry of ClosePctChanges is the LIVE forming BTC candle, so "BTC just turned
// red" reaches the model within the same tick rather than at the next bar close.
type BTCContext struct {
	Open            decimal.Decimal   `json:"open"`
	High            decimal.Decimal   `json:"high"`
	Low             decimal.Decimal   `json:"low"`
	Close           decimal.Decimal   `json:"close"`
	ClosePctChanges []decimal.Decimal `json:"close_pct_changes"`

	DistToSwingHighPct decimal.Decimal `json:"dist_to_swing_high_pct"`
	DistToSwingLowPct  decimal.Decimal `json:"dist_to_swing_low_pct"`

	// Correlation of this token's recent returns with BTC's over the same window. Fed explicitly
	// rather than left for the policy to infer: at this project's trade volume it would never learn
	// to compute a correlation, and "is this token currently following BTC" is precisely the
	// question that matters.
	Correlation decimal.Decimal `json:"correlation"`
}

// PositionState is the open position a call is about, for `update` and terminal categories.
// Zero-valued on a buy/sell call, where the decision is whether to open at all.
type PositionState struct {
	PositionOpen bool            `json:"position_open"`
	Side         decimal.Decimal `json:"side"` // +1 long, -1 short, 0 flat
	SizeUSD      decimal.Decimal `json:"size_usd"`
	Leverage     decimal.Decimal `json:"leverage"`

	// AgeSeconds separates "+30% in 10 minutes" from "-5% after 4 hours" — current PnL alone
	// cannot express that difference, and they are very different trades.
	AgeSeconds int64 `json:"age_seconds"`

	UnrealizedPnLPct decimal.Decimal `json:"unrealized_pnl_pct"`
	// How far this position travelled in each direction, not just where it sits now (§15.11). A
	// trade that reached 90% of its target and gave it back teaches something completely different
	// from one that drifted sideways to the same current PnL. PnLMinPct is negative-ranged.
	PnLMaxPct decimal.Decimal `json:"pnl_max_pct"`
	PnLMinPct decimal.Decimal `json:"pnl_min_pct"`

	DistToSLPct decimal.Decimal `json:"dist_to_sl_pct"`
	DistToTPPct decimal.Decimal `json:"dist_to_tp_pct"`

	// RealizedPnLUSD is meaningful only on a terminal category, where it IS the reward signal.
	RealizedPnLUSD decimal.Decimal `json:"realized_pnl_usd"`
	// RiskPct is the entry-to-stop distance as a fraction of margin — what the trade actually put
	// at risk. The reward divides by this rather than by position size, because a 5% gain made with
	// a 1% stop and one made with a 15% stop are not the same trade (docs/RL_V8_PLAN.md).
	RiskPct decimal.Decimal `json:"risk_pct"`

	// IsFork is gone as of v8: shadow forks were retired and every paper_orders row reads
	// 'baseline', so the input was a constant false.
}

// Observation is the feature vector sent to the RL service for one decision.
//
// Every field here reaches the model. That was not true before v8 — Features/Indicators were
// carried over the wire and never vectorized — so Validate exists to make a short or malformed
// observation a caught error rather than something rl_service pads over.
type Observation struct {
	SchemaVersion int    `json:"schema_version"`
	InstID        string `json:"inst_id"`
	// LastPrice is the current live price — the most recent tick, not a candle close.
	LastPrice decimal.Decimal `json:"last_price"`

	TokenProfile TokenProfile `json:"token_profile"`
	// Timeframes carries EXACTLY ONE block, for the decision bar this call is about. A slice rather
	// than a single field so a future multi-timeframe observation is an additive width change
	// rather than a reshape of the wire format.
	Timeframes []MarketBlock `json:"timeframes"`
	BTC        BTCContext    `json:"btc"`

	// Category is which lifecycle decision this call represents (CLAUDE.md §15.10).
	Category string `json:"category"`
	// Signal is the single strategy signal this call is about. Nil on a price-driven update, where
	// no strategy spoke and only price/PnL moved — rl_service flags that explicitly rather than
	// sending ambiguous zeros.
	Signal        *StrategySignal `json:"signal,omitempty"`
	PositionState PositionState   `json:"position_state"`
	// OrderID ties a decision back to the position it was about, so an outcome landing hours later
	// can be paired with the observation that produced it. Not fed to the model.
	OrderID int64 `json:"order_id"`

	// The shared account pool every token trades against (CLAUDE.md §15.6).
	AccountEquityUSD  decimal.Decimal `json:"account_equity_usd"`
	AccountInitialUSD decimal.Decimal `json:"account_initial_usd"`
	// AccountPeakUSD is the high-water mark, so drawdown is measured from the peak rather than from
	// the configured starting balance. v7 fed equity/initial, and SetAccountCap rewrites initial
	// (§32.2) — so every cap change wiped the model's view of drawdown back to ~1.0.
	AccountPeakUSD  decimal.Decimal `json:"account_peak_usd"`
	OpenExposureUSD decimal.Decimal `json:"open_exposure_usd"`
	// OpenLeveragedExposureUSD is exposure AFTER leverage. v7 summed position size, which is
	// notional before leverage, so a $10 position at 10x counted as $10 of committed risk when it
	// is really $100. Both are carried because margin committed and market exposure are different
	// questions.
	OpenLeveragedExposureUSD decimal.Decimal `json:"open_leveraged_exposure_usd"`
	OpenPositionCount        int             `json:"open_position_count"`

	// MaxPositionPct/MaxLeverage are the risk budget this decision must fit inside (v7, kept). The
	// model's size_pct is read as a fraction OF MaxPositionPct, so "give me everything I'm allowed"
	// is a bounded request rather than one the risk layer has to overrule.
	MaxPositionPct decimal.Decimal `json:"max_position_pct"`
	MaxLeverage    decimal.Decimal `json:"max_leverage"`
}

// Action is the RL agent's decision (CLAUDE.md §15.11).
type Action struct {
	// ActionSchemaVersion is echoed by rl_service so a mismatch is visible in logs; the service
	// itself refuses to serve an incompatible model, so this is a diagnostic, not a gate.
	ActionSchemaVersion int `json:"action_schema_version"`

	// Action is one of the Action* constants.
	Action string `json:"action"`

	// Side is which way to open, for callers with no strategy layer to take direction from. The
	// paper path IGNORES this: there, direction belongs to the strategy that produced the signal
	// (§9/§16.1) and the model only sizes the trade.
	Side string `json:"side"`

	// SLPx/TPPx are PRICE LEVELS the model sets, not percentage nudges. The caller MUST still clamp
	// them — minimum/maximum stop distance, a minimum TP:SL ratio — and run an update through the
	// ratchet so a stop can only tighten. Never trust the model's output as the safety boundary;
	// early in training it is effectively random.
	SLPx decimal.Decimal `json:"sl_px"`
	TPPx decimal.Decimal `json:"tp_px"`

	// SizePct is the fraction of account equity to commit; LeverageFrac in [0,1] maps to
	// [1x, MaxLeverage].
	SizePct      decimal.Decimal `json:"size_pct"`
	LeverageFrac decimal.Decimal `json:"leverage_frac"`

	// OrderID is echoed from the request so the controller can pair a response to the position it
	// asked about.
	OrderID int64 `json:"order_id"`

	Confidence decimal.Decimal `json:"confidence"`
}
