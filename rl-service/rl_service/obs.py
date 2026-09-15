"""Shared observation/action schema and vectorization for the global RL agent (CLAUDE.md §15).

Single source of truth for both the live inference path (rl_service/serve/api.py) and the
backtest/warm-start dataset builder — they MUST build the exact same flattened vector from an
Observation, or a model trained by one and served by the other silently misaligns features. Keep in
sync with `domain.Observation` / `domain.Action` on the Go side (go-engine/internal/domain/rl.go).

v8's governing rule, and the reason this file reads the way it does: THE WIDTH IS EXACT. Nothing
here pads, truncates, or adapts to what a loaded model happens to want. A vector that does not
match OBSERVATION_DIM is an error, loudly, because the alternative is what v7 actually did in
production — quietly dropping ten inputs on every single call for weeks, with no log, no metric,
and no failure to notice (docs/RL_V8_PLAN.md).
"""
from __future__ import annotations

from typing import Optional

import numpy as np
from pydantic import BaseModel, Field, field_validator

# v3: single global agent (CLAUDE.md §15.1) — token-identity one-hot and raw price context.
# v4: shared-account fields replaced per-token sub-budgets (§15.6).
# v5: the event-driven signal lifecycle (§15.10) — and strategy signals actually reached the vector.
# v6: signal SL/TP became prices rather than percentages; merged position block (§15.11).
# v7: the risk budget (max_position_pct, max_leverage) became an input.
#
# v8 (current): the observation was audited before a from-scratch retrain and found to be feeding
# the model almost no market data at all (docs/RL_V8_PLAN.md). Three compounding defects:
# to_vector silently truncated the feature block to whatever width the LOADED MODEL wanted (91
# minus an 85-wide tail left a 6-value budget against the 16 Go builds, cutting the live candle's
# OHLC and 6 of 10 returns); TimeframeBlock.features — the derived indicators — was never populated
# by Go at all, so the policy had no RSI, no volatility and no volume since the field was created;
# and the token one-hot was 16 slots against a roster that reached 65, so most tokens one-hotted to
# all zeros while the 8-hourly discovery scan reassigned slot meanings by re-sorting the roster.
#
# So v8 removes every identity one-hot. A token is described by what it IS (volatility, volume,
# price magnitude) rather than which one it is; a strategy by its measured record rather than its
# name; a timeframe by an ordered scalar rather than 16 unordered slots. None of the three has a
# ceiling any more, which is the property that matters as the roster grows — and a newly discovered
# token is comprehensible from its first candle instead of being an unlearned slot.
#
# Added: ten derived indicators per timeframe, computed in Go from the same indicator library the
# strategies use, and a BTC reference block — every prior input was intra-token, so the model could
# never see that an altcoin reverses the moment BTC's candle turns red.
#
# Dropped: is_fork (verified always false — every paper_orders row is 'baseline') and the signal's
# confidence (hardcoded per strategy, so for 24 of 26 kinds it was an identity label in disguise,
# and it contradicted the measured win rate now fed beside it).
#
# Must match domain.ObservationSchemaVersion.
OBSERVATION_SCHEMA_VERSION = 8

# --- action schema ----------------------------------------------------------------------------

# ACTIONS is the decision vocabulary. Which values are valid depends on the request's category:
#   buy / sell   -> open | skip
#   update       -> none | update | close
#   closed_*     -> ignored; that call exists to deliver reward, not to ask anything
#
# Order defines indices and must stay stable.
ACTIONS = ["open", "skip", "none", "update", "close"]

LEGAL_ACTIONS_BY_CATEGORY = {
    "buy": ["open", "skip"],
    "sell": ["open", "skip"],
    "update": ["none", "update", "close"],
}

# ACTION_SCHEMA_VERSION tracks the ACTION vector's layout independently of the observation's.
# v1: Box(2,). v2: sl/tp adjust + strategy-weight slots. v3: dropped weights, added an action head.
# v4: the model SETS sl_px/tp_px as prices; one 5-wide action head.
#
# v5 (current): the single 5-wide head is SPLIT INTO TWO, because masking it at serving time was
# not enough. decode_action masked the argmax to the legal actions for the category (§16.9 added
# that, and it works), but SAC trains on the RAW vector — so on a buy call, reward was attributed
# to all five outputs including `close`, which had been discarded. An output that receives reward
# without having caused anything feels no corrective pressure and is free to drift to the tanh
# bound, which is the direction §54.8 measured when all nine outputs pinned at ±1. Two heads plus
# zeroing the irrelevant one before it reaches the replay buffer (learner.py) means an output only
# takes gradient in the category where it actually decided something.
ACTION_SCHEMA_VERSION = 5

# The open head is ONE scalar whose SIGN decides: >= 0 open, < 0 skip. A two-logit argmax would
# work equally well, but a sign carries the same information in half the width and gives the policy
# a continuous quantity to move rather than a pair of competing logits.
OPEN_HEAD_DIM = 1
# The manage head is a 3-way argmax over none | update | close.
MANAGE_ACTIONS = ["none", "update", "close"]
MANAGE_HEAD_DIM = len(MANAGE_ACTIONS)

# ACTION_DIM: sl_px, tp_px (offsets from live price), size_pct, leverage_frac + the two heads.
ACTION_DIM = 4 + OPEN_HEAD_DIM + MANAGE_HEAD_DIM

# The policy emits SL/TP in [-1, 1], mapped to a price by scaling against MAX_SLTP_OFFSET_PCT of
# the live price. Prices cannot be emitted directly — a network output has no idea whether this
# instrument trades at 0.15 or 65000 — so the model chooses a DISTANCE and the caller turns it into
# a level. Go re-clamps regardless (§15.11); this only keeps the output range aligned with what Go
# will accept.
MAX_SLTP_OFFSET_PCT = 0.10

# --- lifecycle categories ----------------------------------------------------------------------

# SIGNAL_CATEGORIES is the lifecycle stage a /predict call represents (CLAUDE.md §15.10).
#
# v8 note: the three terminal categories are no longer one-hot slots. §15.14 and §20 both had to
# force `timeout` and `manual` closes under `closed_early` because widening a one-hot meant a schema
# bump on both sides — collapsing 358 trades (a fifth of all closes) into one label covering three
# genuinely different things. They are now encoded as a terminal flag plus two scalars (see
# category_block), which distinguishes all five real close reasons in 3 values instead of 8.
SIGNAL_CATEGORIES = [
    "buy",             # strategy fired, no open position -> open or skip
    "sell",            # same, short side
    "update",          # position open: another signal fired, or PnL moved past the threshold
    "closed_tp",       # terminal: take-profit hit
    "closed_sl",       # terminal: stop-loss hit
    "closed_early",    # terminal: the model chose to close
    "closed_timeout",  # terminal: held past the maximum duration
    "closed_manual",   # terminal: an operator closed it
]

TERMINAL_CATEGORIES = frozenset(
    {"closed_tp", "closed_sl", "closed_early", "closed_timeout", "closed_manual"}
)

# A manual close delivers NO reward (CLAUDE.md §15.12): attributing an operator's action to the
# policy would train it on a decision it never made. The call still happens so the learner's
# pending decision resolves rather than leaking, but the reward is zeroed.
ZERO_REWARD_CATEGORIES = frozenset({"closed_manual"})

# --- fixed block widths ------------------------------------------------------------------------
#
# Every width below is EXACT and asserted by a test that builds a full observation and counts the
# result. Adding a field without updating its constant fails that test, which is the whole point:
# v7 had no such check, so ten inputs vanished in production with nothing to notice.

TOKEN_PROFILE_DIM = 7     # what this token IS, replacing the 16-slot identity one-hot
ACCOUNT_DIM = 6           # account state + the risk budget this decision must fit inside
CATEGORY_DIM = 5          # buy/sell/update one-hot + terminal flag + 2 reason scalars
STRATEGY_PROFILE_DIM = 6  # this strategy's measured record, replacing the 24-slot kind one-hot
SIGNAL_DIM = 7            # the signal itself (no confidence — see the v8 note above)
POSITION_DIM = 10         # the open position's state and trajectory (no is_fork — always false)

# Per-timeframe market block: 10 derived indicators + live OHLC + returns + swing distances.
INDICATORS_PER_TIMEFRAME = 10
PRICE_OHLC_DIM = 4
RETURNS_WINDOW = 10
SWING_DIM = 2
MARKET_BLOCK_DIM = INDICATORS_PER_TIMEFRAME + PRICE_OHLC_DIM + RETURNS_WINDOW + SWING_DIM  # 26

# The BTC reference block is the same shape minus the indicators, plus one correlation scalar.
# Always present, even when the token IS BTC — it duplicates harmlessly and keeps the width fixed.
BTC_BLOCK_DIM = PRICE_OHLC_DIM + RETURNS_WINDOW + SWING_DIM + 1  # 17

OBSERVATION_DIM = (
    TOKEN_PROFILE_DIM
    + ACCOUNT_DIM
    + CATEGORY_DIM
    + STRATEGY_PROFILE_DIM
    + SIGNAL_DIM
    + POSITION_DIM
    + MARKET_BLOCK_DIM
    + BTC_BLOCK_DIM
)

# Bars the model may be asked about, as MINUTES. Used only to turn a bar into one ordered scalar —
# a new timeframe needs no vocabulary slot, just a minute count, because log(minutes) is a
# continuous quantity the policy can interpolate over rather than a categorical slot it must learn
# from scratch.
BAR_MINUTES = {
    "1m": 1, "3m": 3, "5m": 5, "15m": 15, "30m": 30,
    "1H": 60, "2H": 120, "4H": 240, "6H": 360, "12H": 720,
    "1D": 1440, "1W": 10080,
}
# Normalizing constant for log(bar minutes): log(10080) for a 1W bar, so the scalar lands in [0, 1]
# across every bar this project could plausibly trade.
_LOG_MAX_BAR = float(np.log(10080.0))


class SchemaError(ValueError):
    """An observation that cannot be vectorized. Never padded over, never truncated away."""


def _f(value: float) -> float:
    """Coerces to a finite float; NaN/inf become 0.0.

    A NaN anywhere in the vector poisons every downstream computation silently — the forward pass
    succeeds and returns NaN actions — so it is stopped here rather than allowed to reach the
    network. Callers should not be producing NaN in the first place; this is the last line.
    """
    v = float(value)
    return v if np.isfinite(v) else 0.0


def _body_position(tf_open: float, high: float, low: float, close: float) -> float:
    """Where the close sits inside its own bar, in [-1, +1]. -1 closed on the low, +1 on the high.

    This slot used to carry `_rel(close, last_price)`, which is STRUCTURALLY ZERO: last_price IS the
    forming candle's close, so the field divided the close by itself on every observation ever
    built — one wasted input here and another in the BTC block, measured dead across all 76,305
    backtest samples.

    Close-within-range is the natural replacement: it is the half of the candle's shape the other
    three OHLC slots cannot express (open/high/low are all relative to the close, so they describe
    the bar's extent but not its conviction), and it needs no extra width.
    """
    rng = high - low
    if rng <= 0:
        return 0.0
    return float(np.clip(2.0 * (close - low) / rng - 1.0, -1.0, 1.0))


def _rel(level: float, price: float) -> float:
    """A price level as a signed fraction of the live price; 0.0 when either is absent."""
    if not level or not price:
        return 0.0
    return _f((level - price) / price)


# --- wire models -------------------------------------------------------------------------------


class StrategySignal(BaseModel):
    strategy_id: int = 0
    side: str = ""

    # PRICES, not percentages (CLAUDE.md §15.11). A strategy derives these from chart structure — a
    # stop below a swing low, a target at a fair-value gap — and converting a level to a percentage
    # throws away exactly the structural information that made it a level. Vectorized as offsets
    # from the live price so one shared policy still generalizes across instruments.
    entry_px: float = 0.0
    sl_px: float = 0.0
    tp_px: float = 0.0

    kind: str = ""
    bar: str = ""

    # This strategy's MEASURED record on this instrument. In v7 these sat beside a hardcoded
    # `confidence` that contradicted them (`grid_like` declared 1.0 while running a 35% win rate and
    # -$4.62 of PnL); the hardcoded number is gone and these are what remains, because they are the
    # ones derived from what actually happened.
    win_rate: float = 0.0
    trade_count: float = 0.0
    avg_rr: float = 0.0            # average reward:risk this strategy proposes
    avg_hold_hours: float = 0.0    # average time its trades stay open
    avg_pnl_per_trade: float = 0.0 # normalized by position size, matching the reward's own scale


class MarketBlock(BaseModel):
    """One timeframe's market state: derived indicators plus raw price action.

    Both halves matter and neither replaces the other. The indicators are what a trader reads off a
    chart (is this volatile? is volume unusual? is it overbought?); the returns and OHLC are the
    raw shape the indicators are computed from, kept so the policy can see structure the fixed
    indicator set does not capture.
    """

    @field_validator("indicators", "close_pct_changes", mode="before")
    @classmethod
    def _null_to_empty(cls, v):
        return [] if v is None else v

    bar: str = ""

    # Ten values, fixed order, computed IN GO (go-engine/internal/usecase/features.go):
    #   ema_ratio_5, ema_ratio_10, ema_ratio_20,
    #   volatility_5, volatility_10, volatility_20,
    #   rsi_14 (normalized to [-1, 1]), volume_ratio_20, atr_ratio_14, range_ratio
    # Sourcing them from Go rather than recomputing in Python removes the two-language duplication
    # §16.2 warns about — there is one implementation, and it is the one the strategies share.
    indicators: list[float] = Field(default_factory=list)

    # The LIVE FORMING candle, not the last closed one (CLAUDE.md §15.11): OKX pushes the in-progress
    # bar on the same WS channel, and on a 1H timeframe the last *closed* candle can be 59 minutes
    # stale — the same freshness problem §15.9's audit found on the SL/TP path.
    open: float = 0.0
    high: float = 0.0
    low: float = 0.0
    close: float = 0.0

    close_pct_changes: list[float] = Field(default_factory=list)
    dist_to_swing_high_pct: float = 0.0
    dist_to_swing_low_pct: float = 0.0


class BTCContext(BaseModel):
    """The wider market, as BTC (docs/RL_V8_PLAN.md).

    Every other input in this observation is intra-token. The operator's own observation is the
    reason this exists: an altcoin can be cleanly trending and reverse the moment BTC's candle turns
    red. Without this the model cannot see that at all — it would have to infer a market-wide regime
    from one instrument's price, which it cannot do.

    The last entry of close_pct_changes is the LIVE forming BTC candle, so "BTC just turned red"
    reaches the model within the same tick rather than at the next bar close.
    """

    @field_validator("close_pct_changes", mode="before")
    @classmethod
    def _null_to_empty(cls, v):
        return [] if v is None else v

    open: float = 0.0
    high: float = 0.0
    low: float = 0.0
    close: float = 0.0
    close_pct_changes: list[float] = Field(default_factory=list)
    dist_to_swing_high_pct: float = 0.0
    dist_to_swing_low_pct: float = 0.0
    # Correlation of this token's recent returns with BTC's over the same window. Fed explicitly
    # rather than left for the policy to infer from the two series: at this project's trade volume
    # it would never learn to compute a correlation, and "is this token currently following BTC"
    # is precisely the question the operator described mattering.
    correlation: float = 0.0


class TokenProfile(BaseModel):
    """What this token IS, replacing v7's 16-slot identity one-hot (docs/RL_V8_PLAN.md).

    The one-hot could not survive a growing roster: 65 tokens against 16 slots meant most of them
    were indistinguishable zeros, and re-sorting the roster on every discovery scan silently
    reassigned the slots that did work. More fundamentally, identity is the wrong input — the model
    should learn "high-volatility, thin-volume instruments need wider stops", which transfers to a
    token discovered tomorrow, not "PEPE behaves like this", which never does.
    """

    typical_volatility: float = 0.0  # ATR / price, averaged — the single biggest BTC-vs-PEPE difference
    log_volume_24h: float = 0.0      # log10 of 24h USD volume: liquidity, and §33.2's binding constraint
    volume_rank: float = 0.0         # rank within the roster, 0..1
    log_price: float = 0.0           # log10 price: tick behaviour differs at 0.000003 vs 90000
    range_24h: float = 0.0
    change_24h: float = 0.0
    log_trade_count: float = 0.0     # log1p of this token's own recorded trades — "how much do I know here"


class PositionState(BaseModel):
    """The open position this call is about, for `update` and terminal categories (§15.11).

    Zero-valued on a buy/sell call, where the decision is whether to open at all. Entry/SL/TP are
    deliberately NOT repeated here — they are already carried on the signal.
    """

    position_open: float = 0.0
    side: float = 0.0  # +1 long, -1 short, 0 flat
    size_usd: float = 0.0
    leverage: float = 0.0

    # age separates "+30% in 10 minutes" from "-5% after 4 hours" — current PnL alone cannot.
    age_seconds: float = 0.0

    unrealized_pnl_pct: float = 0.0
    # How far this position travelled in each direction, not just where it sits now (§15.11). A
    # trade that reached 90% of its target and gave it back teaches something completely different
    # from one that drifted sideways to the same current PnL. pnl_min is negative-ranged.
    pnl_max_pct: float = 0.0
    pnl_min_pct: float = 0.0

    dist_to_sl_pct: float = 0.0
    dist_to_tp_pct: float = 0.0

    # Realized PnL is meaningful only on a terminal category, where it IS the reward signal.
    realized_pnl_usd: float = 0.0
    # The risk the trade actually took (entry-to-stop distance, as a fraction of margin). The reward
    # divides by this rather than by position size: a 5% gain made with a 1% stop and a 5% gain made
    # with a 15% stop are not the same trade, and dividing by size scores them identically.
    risk_pct: float = 0.0
    # How many times this position's levels were moved, for the reward's churn penalty. §15.5 says
    # every adjustment should earn its keep in realized outcome rather than being free to try; the
    # penalty existed without a count to charge against, and §54.9 measured what free movement
    # costs — 81 stop adjustments across 66 orders in one hour.
    sltp_adjustments: float = 0.0


class Observation(BaseModel):
    @field_validator("timeframes", mode="before")
    @classmethod
    def _null_to_empty(cls, v):
        return [] if v is None else v

    schema_version: int = OBSERVATION_SCHEMA_VERSION
    inst_id: str = ""
    last_price: float = 0.0

    token_profile: TokenProfile = Field(default_factory=TokenProfile)
    # Exactly one block, for the decision bar this call is about. A list rather than a single field
    # so a future multi-timeframe observation is an additive change to OBSERVATION_DIM rather than a
    # reshape of the wire format.
    timeframes: list[MarketBlock] = Field(default_factory=list)
    btc: BTCContext = Field(default_factory=BTCContext)

    category: str = "update"
    signal: Optional[StrategySignal] = None
    position_state: PositionState = Field(default_factory=PositionState)
    # Not fed to the model (an id has no ordinal meaning) — carried so a decision can be paired with
    # the outcome that lands hours later.
    order_id: int = 0

    # The shared account pool every token trades against (CLAUDE.md §15.6).
    account_equity_usd: float = 0.0
    account_initial_usd: float = 0.0
    # Peak equity, so drawdown is measured from the high-water mark rather than from the configured
    # starting balance. v7 used equity/initial, which SetAccountCap resets — so every cap change
    # wiped the model's view of drawdown back to ~1.0 (§32.2).
    account_peak_usd: float = 0.0
    open_exposure_usd: float = 0.0
    # Leveraged exposure: v7 summed position SIZE, which is notional before leverage, so a $10
    # position at 10x counted as $10 of committed risk when it is really $100. Both are carried
    # because margin committed and market exposure are different questions.
    open_leveraged_exposure_usd: float = 0.0
    open_position_count: float = 0.0

    # The risk budget this decision must fit inside (schema v7, kept). size_pct is read as a
    # fraction OF max_position_pct, so asking for 1.0 means "the most I am allowed".
    max_position_pct: float = 0.10
    max_leverage: float = 10.0


class Action(BaseModel):
    """The model's decision (CLAUDE.md §15.11).

    Which fields matter depends on the request's category: `open` uses all of them, `update` uses
    sl_px/tp_px, and `skip`/`none`/`close` use none. The controller re-clamps everything regardless.
    """

    action_schema_version: int = ACTION_SCHEMA_VERSION
    action: str = "skip"
    # Direction is never the model's to choose (§16.1) — echoed from the requesting signal so a
    # caller without a strategy layer still gets one.
    side: str = ""
    sl_px: float = 0.0
    tp_px: float = 0.0
    size_pct: float = 0.0
    leverage_frac: float = 0.0
    order_id: int = 0
    confidence: float = 0.0


# --- block builders ----------------------------------------------------------------------------
#
# Each returns EXACTLY its declared width or raises SchemaError. No builder pads, and none returns
# a variable-length result — that is what made v7's truncation possible.


def token_profile_block(obs: Observation) -> np.ndarray:
    p = obs.token_profile
    vec = np.array(
        [
            _f(p.typical_volatility),
            _f(p.log_volume_24h),
            _f(p.volume_rank),
            _f(p.log_price),
            _f(p.range_24h),
            _f(p.change_24h),
            _f(p.log_trade_count),
        ],
        dtype=np.float32,
    )
    _require(vec, TOKEN_PROFILE_DIM, "token_profile")
    return vec


def account_block(obs: Observation) -> np.ndarray:
    """Account state and risk budget, as RATIOS rather than dollars.

    A policy trained on a $2,600 account would otherwise go out of distribution the moment the
    balance is reconfigured — and the decision it makes ("what fraction of my account do I commit")
    is scale-free anyway. §19.1 records what a distribution shift in a model input costs.
    """
    equity = obs.account_equity_usd
    # Drawdown from the HIGH-WATER MARK, not from the configured start (see account_peak_usd).
    peak = obs.account_peak_usd or obs.account_initial_usd or equity
    equity_ratio = equity / peak if peak else 0.0
    exposure_ratio = obs.open_exposure_usd / equity if equity else 0.0
    lev_exposure_ratio = obs.open_leveraged_exposure_usd / equity if equity else 0.0
    # The per-position dollar budget, log-scaled. v7 fed 1/PositionSlots, which moved whenever a
    # token or strategy was enabled even though the economics had not changed: the day slots went
    # 22 -> 273 this input fell 4.55% -> 0.37% while the actual dollar budget went $9.09 -> $9.52.
    # A log dollar figure stays put when the economics stay put.
    budget_usd = equity * obs.max_position_pct
    log_budget = float(np.log10(budget_usd)) if budget_usd > 0 else 0.0
    vec = np.array(
        [
            _f(equity_ratio),
            _f(exposure_ratio),
            _f(lev_exposure_ratio),
            _f(log_budget),
            _f(obs.max_leverage / 100.0),
            # Open position count, scaled: "3 open" and "30 open" are different situations.
            _f(obs.open_position_count / 20.0),
        ],
        dtype=np.float32,
    )
    _require(vec, ACCOUNT_DIM, "account")
    return vec


def category_block(obs: Observation) -> np.ndarray:
    """Which lifecycle decision this is, in 5 values rather than an 8-slot one-hot.

    buy/sell/update stay one-hot because they are genuinely unordered questions. The five terminal
    categories collapse to a flag plus two scalars describing WHY the position closed:
      - level_touch: +1 the price reached a level we set, 0 it did not
      - agency:      +1 the model chose this, 0 the system did, -1 a person did

    That distinguishes all five (tp: touch/system, sl: touch/system, early: no-touch/model,
    timeout: no-touch/system, manual: no-touch/person) in 3 values instead of 5, and — unlike v7,
    where §15.14 and §20 both had to force timeout and manual under closed_early — it can express a
    new close reason without a schema bump.
    """
    cat = obs.category
    vec = np.zeros(CATEGORY_DIM, dtype=np.float32)
    if cat == "buy":
        vec[0] = 1.0
    elif cat == "sell":
        vec[1] = 1.0
    elif cat == "update":
        vec[2] = 1.0
    elif cat in TERMINAL_CATEGORIES:
        vec[3] = 1.0
        if cat in ("closed_tp", "closed_sl"):
            vec[4] = 1.0          # a level we set was touched; the system executed it
        elif cat == "closed_early":
            vec[4] = 0.5          # no level touched; the model chose to exit
        elif cat == "closed_manual":
            vec[4] = -1.0         # a person chose; reward is zeroed for this anyway
        # closed_timeout leaves vec[4] at 0.0: no touch, no decision, just elapsed time.
    return vec


def strategy_profile_block(obs: Observation) -> np.ndarray:
    """This strategy's MEASURED record, replacing v7's 24-slot kind one-hot.

    The one-hot had already overflowed — 26 registered kinds against 24 slots — and, like the token
    one-hot, encoded identity where behaviour is what matters. A strategy added tomorrow is
    comprehensible from its record; it was previously an unlearned slot until it had accumulated
    enough trades to matter, by which point the width might have run out.

    The bar is one ORDERED scalar rather than a 16-slot one-hot: 5m < 15m < 1H is a real ordering,
    and a one-hot destroys it by telling the model the three are unrelated categories.
    """
    sig = obs.signal
    if sig is None:
        return np.zeros(STRATEGY_PROFILE_DIM, dtype=np.float32)
    minutes = BAR_MINUTES.get(sig.bar, 0)
    log_bar = float(np.log(minutes)) / _LOG_MAX_BAR if minutes > 0 else 0.0
    vec = np.array(
        [
            _f(sig.win_rate),
            # Log-compressed: 100% of 2 trades and 60% of 200 are very different evidence, while
            # the difference between 500 and 545 trades is not worth linear scale.
            _f(np.log1p(max(0.0, sig.trade_count))),
            _f(sig.avg_rr),
            _f(sig.avg_hold_hours),
            _f(sig.avg_pnl_per_trade),
            _f(log_bar),
        ],
        dtype=np.float32,
    )
    _require(vec, STRATEGY_PROFILE_DIM, "strategy_profile")
    return vec


def signal_block(obs: Observation) -> np.ndarray:
    """The single signal this call is about.

    `present` disambiguates "no strategy spoke" from "a strategy said zero": without it a
    price-driven update is indistinguishable from a signal with zero conviction.

    v7's `confidence` is gone. It was hardcoded per strategy (0.5 / 0.55 / 0.6 / 0.65 / 1.0) and
    computed by only two of 26 kinds, so for the rest it was the kind one-hot in a single number —
    and it contradicted the win rate now fed beside it.
    """
    sig = obs.signal
    if sig is None:
        return np.zeros(SIGNAL_DIM, dtype=np.float32)

    side = 1.0 if sig.side == "buy" else (-1.0 if sig.side == "sell" else 0.0)
    price = obs.last_price
    entry = _rel(sig.entry_px, price)
    sl = _rel(sig.sl_px, price)
    tp = _rel(sig.tp_px, price)
    # The signal's own reward:risk, from the levels it proposed. Derivable from sl and tp, but a
    # ratio is what the decision actually turns on, and asking the policy to divide two small
    # numbers is asking it to learn something it can simply be told.
    rr = abs(tp - entry) / abs(sl - entry) if abs(sl - entry) > 1e-12 else 0.0
    vec = np.array(
        [1.0, side, entry, sl, tp, _f(entry), _f(min(rr, 10.0))],
        dtype=np.float32,
    )
    _require(vec, SIGNAL_DIM, "signal")
    return vec


def position_block(obs: Observation) -> np.ndarray:
    """The open position's state and trajectory.

    Entry/SL/TP are not repeated here — they are on the signal. What this adds is how long the
    position has been open and how far it travelled in each direction, which current PnL cannot say.

    v7's `is_fork` is gone: shadow forks were retired, every paper_orders row reads 'baseline', so
    the input was a constant false.
    """
    ps = obs.position_state
    equity = obs.account_equity_usd
    vec = np.array(
        [
            _f(ps.position_open),
            _f(ps.side),
            _f(ps.leverage / 100.0),
            _f((ps.size_usd / equity) if equity else 0.0),
            # Hours, not seconds: keeps a multi-hour position on a similar scale to everything else
            # instead of a five-digit number that would dominate.
            _f(ps.age_seconds / 3600.0),
            _f(ps.unrealized_pnl_pct),
            _f(ps.pnl_max_pct),
            _f(ps.pnl_min_pct),
            _f(ps.dist_to_sl_pct),
            _f(ps.dist_to_tp_pct),
        ],
        dtype=np.float32,
    )
    _require(vec, POSITION_DIM, "position")
    return vec


def market_block(obs: Observation) -> np.ndarray:
    """The decision timeframe's market state — indicators plus raw price action.

    This is the block v7 lost. Ten indicators were declared, parsed, and never populated by Go, and
    the raw half was then truncated from the left by to_vector, so the live candle's OHLC and six of
    ten returns never reached the model either. Of 91 inputs, six described the market.
    """
    if len(obs.timeframes) != 1:
        raise SchemaError(
            f"observation must carry exactly 1 timeframe block, got {len(obs.timeframes)}"
        )
    tf = obs.timeframes[0]
    if len(tf.indicators) != INDICATORS_PER_TIMEFRAME:
        raise SchemaError(
            f"timeframe {tf.bar!r}: got {len(tf.indicators)} indicators, "
            f"want exactly {INDICATORS_PER_TIMEFRAME}"
        )
    if len(tf.close_pct_changes) != RETURNS_WINDOW:
        raise SchemaError(
            f"timeframe {tf.bar!r}: got {len(tf.close_pct_changes)} returns, "
            f"want exactly {RETURNS_WINDOW} — a short candle window is a reason to SKIP the "
            "model call, not to pad it"
        )
    price = obs.last_price
    vec = np.array(
        [_f(v) for v in tf.indicators]
        + [
            _rel(tf.open, price),
            _rel(tf.high, price),
            _rel(tf.low, price),
            _body_position(tf.open, tf.high, tf.low, tf.close),
        ]
        + [_f(v) for v in tf.close_pct_changes]
        + [_f(tf.dist_to_swing_high_pct), _f(tf.dist_to_swing_low_pct)],
        dtype=np.float32,
    )
    _require(vec, MARKET_BLOCK_DIM, "market")
    return vec


def btc_block(obs: Observation) -> np.ndarray:
    """The wider market. See BTCContext for why this exists."""
    b = obs.btc
    if len(b.close_pct_changes) != RETURNS_WINDOW:
        raise SchemaError(
            f"btc context: got {len(b.close_pct_changes)} returns, want exactly {RETURNS_WINDOW}"
        )
    # BTC's own levels are relative to BTC's own close, not to this token's price — the two have
    # nothing to do with each other, and dividing one by the other would produce a number with no
    # meaning at all.
    ref = b.close or 0.0
    # The fourth slot carried a literal 0.0, correctly: _rel(close, close) is zero by construction.
    # Writing the constant made the waste explicit rather than accidental, but it was still one of
    # 84 inputs spending itself on a number that never varies. Body position is the same fix applied
    # in market_block — it says whether BTC's own bar closed strong or weak, which is exactly the
    # "it just turned red" signal this block exists for (docs/RL_V8_PLAN.md).
    vec = np.array(
        [
            _rel(b.open, ref),
            _rel(b.high, ref),
            _rel(b.low, ref),
            _body_position(b.open, b.high, b.low, b.close),
        ]
        + [_f(v) for v in b.close_pct_changes]
        + [_f(b.dist_to_swing_high_pct), _f(b.dist_to_swing_low_pct), _f(b.correlation)],
        dtype=np.float32,
    )
    _require(vec, BTC_BLOCK_DIM, "btc")
    return vec


def _require(vec: np.ndarray, want: int, name: str) -> None:
    if vec.shape[0] != want:
        raise SchemaError(f"{name} block is {vec.shape[0]} wide, want exactly {want}")


def to_vector(obs: Observation) -> np.ndarray:
    """Builds the model-input vector. EXACT width — never pads, never truncates.

    v7's version took an `expected_dim` from the loaded model and reshaped the features to fit,
    which let the model dictate what the code sent instead of the other way round. That is how ten
    inputs disappeared in production for weeks with nothing to notice, and it is the same mechanism
    §16.9 recorded collapsing an 89-dim observation into an 83-dim model's fixed tail.

    A caller whose data is incomplete should SKIP the model call, not ask for a padded answer: a
    call that fails downstream leaves a pending decision in the learner that never receives its
    reward, reopening the gap §15.12 closed.
    """
    vec = np.concatenate(
        [
            token_profile_block(obs),
            account_block(obs),
            category_block(obs),
            strategy_profile_block(obs),
            signal_block(obs),
            position_block(obs),
            market_block(obs),
            btc_block(obs),
        ]
    )
    if vec.shape[0] != OBSERVATION_DIM:
        raise SchemaError(
            f"observation vector is {vec.shape[0]} wide, want exactly {OBSERVATION_DIM}"
        )
    return vec.reshape(1, -1)


# --- action decoding ---------------------------------------------------------------------------


def decode_action(raw: np.ndarray, obs: Observation) -> Action:
    """Turns the policy's raw ACTION_DIM vector into the typed Action the Go caller consumes.

    Layout (v5):
      [0] sl offset      -> scaled by MAX_SLTP_OFFSET_PCT, turned into a PRICE against last_price
      [1] tp offset      -> same
      [2] size_pct       in [0, 1] — fraction of the ALLOWED budget (obs.max_position_pct)
      [3] leverage_frac  in [0, 1] — mapped to [1x, max_leverage] by the caller
      [4] open head      -> sign decides open (>= 0) or skip (< 0)
      [5:8] manage head  -> argmax over none | update | close

    Two heads rather than one masked five-way head: the mask worked at serving time but SAC trains
    on the raw vector, so reward reached outputs that had been discarded. See ACTION_SCHEMA_VERSION.
    """
    vec = np.asarray(raw, dtype=np.float32).reshape(-1)
    if vec.shape[0] != ACTION_DIM:
        raise SchemaError(f"action vector is {vec.shape[0]} wide, want exactly {ACTION_DIM}")

    sl_offset = float(np.clip(vec[0], -1.0, 1.0)) * MAX_SLTP_OFFSET_PCT
    tp_offset = float(np.clip(vec[1], -1.0, 1.0)) * MAX_SLTP_OFFSET_PCT
    # Scaled by the budget the observation carried, so the policy's "how much of what I am allowed"
    # becomes the fraction-of-equity the caller expects.
    size_pct = float(np.clip(vec[2], 0.0, 1.0)) * obs.max_position_pct
    leverage_frac = float(np.clip(vec[3], 0.0, 1.0))

    if obs.category in ("buy", "sell"):
        action = "open" if float(vec[4]) >= 0.0 else "skip"
    elif obs.category == "update":
        action = MANAGE_ACTIONS[int(np.argmax(vec[5 : 5 + MANAGE_HEAD_DIM]))]
    else:
        # Terminal: the caller discards this; the call exists to deliver reward.
        action = "none"

    return Action(
        action=action,
        side=obs.signal.side if obs.signal else "",
        sl_px=obs.last_price * (1.0 + sl_offset) if obs.last_price else 0.0,
        tp_px=obs.last_price * (1.0 + tp_offset) if obs.last_price else 0.0,
        size_pct=size_pct,
        leverage_frac=leverage_frac,
        order_id=obs.order_id,
        # How decisively the policy sized this position — the only self-assessment an actor emits
        # without a separate value head being plumbed through.
        confidence=size_pct,
    )


def mask_action_for_learning(raw: np.ndarray, category: str) -> np.ndarray:
    """Zeroes the head that did not decide anything, before the action reaches the replay buffer.

    This is the half of the two-head split that actually fixes the problem. Splitting the heads
    aligns the structure with the question; zeroing here is what stops reward flowing to an output
    that had no effect. On a buy call the manage head is discarded by decode_action, so training on
    its emitted value teaches the network to move an output nothing reads — and an output that
    receives reward without causing anything drifts to the tanh bound unopposed, which is what
    §54.8 measured across all nine outputs.

    Neutral is 0.0 rather than the emitted value: a tanh-squashed policy's neutral point.
    """
    vec = np.asarray(raw, dtype=np.float32).reshape(-1).copy()
    if category in ("buy", "sell"):
        vec[5 : 5 + MANAGE_HEAD_DIM] = 0.0
    elif category == "update":
        vec[4] = 0.0
    else:
        # Terminal calls decide nothing at all; only sl/tp/size/leverage carry over as context.
        vec[4] = 0.0
        vec[5 : 5 + MANAGE_HEAD_DIM] = 0.0
    return vec


def flat_action() -> Action:
    """Fail-safe no-op action — take no trade, touch nothing. CLAUDE.md §5, §15.11."""
    return Action(action="skip", sl_px=0.0, tp_px=0.0, size_pct=0.0, leverage_frac=0.0, confidence=0.0)
