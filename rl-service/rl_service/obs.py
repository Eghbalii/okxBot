"""Shared observation/action schema and vectorization for the global RL agent (CLAUDE.md §15).

Single source of truth for both the live inference path (rl_service/serve/api.py) and the
warm-start replay env (rl_service/env/replay_env.py) — they MUST build the exact same flattened
vector from an Observation, or a model trained by one and served by the other silently misaligns
features. Keep in sync with `domain.Observation` / `domain.Action` on the Go side
(go-engine/internal/domain/rl.go).
"""
from __future__ import annotations

from typing import Optional

import numpy as np
from pydantic import BaseModel, Field, field_validator

# v3: switched to a single global agent (CLAUDE.md §15.1) — added active_tokens (token-identity
# one-hot) and price_context (raw price series + positional/distance features) per timeframe block,
# plus dist_to_sl_pct/dist_to_tp_pct on the top-level observation.
#
# v4: replaced the per-token sub-budget fields (token_equity_usd/token_budget_usd) with the
# shared-account fields account_equity_usd/account_initial_usd/open_exposure_usd (CLAUDE.md §15.6's
# 2026-08-28 revision) — capital is one pool the agent sizes trades against, not a per-token
# constant.
#
# v5 (current): the event-driven signal lifecycle (CLAUDE.md §15.10). Added category, a single
# per-call `signal` (with strategy kind, timeframe, win rate and staleness), market_context and
# position_state — and, critically, actually fed strategy signals and recent_trades INTO the model's
# input vector, which v3/v4 never did despite carrying both over the wire. Must match
# domain.ObservationSchemaVersion on the Go side.
OBSERVATION_SCHEMA_VERSION = 5

# ORDER_ACTIONS is the decision head for an OPEN position (CLAUDE.md §15.10's `update` category):
# leave it alone, move its SL/TP, or close it now. This replaced the idea of an "optimize" signal
# side — the decision belongs on the output, where the model has live price and position state,
# not on the strategy, which only sees candles.
#
# Order defines the argmax index and must stay stable.
ORDER_ACTIONS = ["none", "adjust", "close"]

# ACTION_SCHEMA_VERSION tracks the ACTION vector's layout, independently of the observation's
# schema_version — a model trained against a different action space cannot serve a caller expecting
# this one. v1: Box(2,) [target_exposure, leverage_frac]. v2: added sl/tp_adjust plus fixed
# strategy_weight slots. v3 (current): dropped strategy_weights entirely and added the order-action
# head (CLAUDE.md §15.10).
#
# Dropping the weights is what removes the fixed ceiling on strategy count: one signal per call
# means nothing in the action scales with the roster, so strategies can be added or removed without
# changing the action width or retraining. Trust in a strategy is now an INPUT (its live win rate)
# rather than a score the model has to invent. Keep in sync with domain.ActionSchemaVersion.
ACTION_SCHEMA_VERSION = 3

# ACTION_DIM: target_exposure, leverage_frac, sl_adjust_pct, tp_adjust_pct + the order-action head.
ACTION_DIM = 4 + len(ORDER_ACTIONS)

# SL/TP adjustment outputs are emitted in [-1, 1] by the policy and scaled to a fraction of price by
# MAX_SLTP_ADJUST_PCT. This mirrors usecase.MaxSLTPAdjustPct on the Go side (CLAUDE.md §15.4's "±2%
# per decision step") so the model's raw output range maps onto the same bounded adjustment the Go
# ratchet will clamp it to anyway — keeping the two in sync means the policy explores the full range
# the ratchet actually accepts, instead of spending most of its output range on values Go clips.
MAX_SLTP_ADJUST_PCT = 0.02


# SIGNAL_CATEGORIES is the lifecycle stage a /predict call represents (CLAUDE.md §15.10). The
# category tells the model which decision it is being asked to make, which is why it replaced a
# flat buy/sell/hold plus a separate "optimize" action: opening a position, managing an open one,
# and being told how one ended are genuinely different questions over the same fields.
#
# Order defines the one-hot index and must stay stable — inserting a category in the middle would
# silently reassign every later slot's meaning to an already-trained model.
SIGNAL_CATEGORIES = [
    "buy",           # strategy fired, no open position on this token -> open or skip
    "sell",          # same, short side
    "update",        # position open: another signal fired, or PnL moved past the threshold
    "closed_tp",     # terminal: take-profit hit
    "closed_sl",     # terminal: stop-loss hit
    "closed_early",  # terminal: the model closed it before either level
]

# Terminal categories carry the realized outcome and are what the reward is computed from
# (CLAUDE.md §15.10) — the close event IS the reward, not a separate pipeline.
TERMINAL_CATEGORIES = frozenset({"closed_tp", "closed_sl", "closed_early"})

# STRATEGY_KINDS mirrors go-engine's strategy.Factories registry. The model needs to know WHICH
# strategy produced a signal, and identity has to be stable across restarts and roster changes —
# so it is keyed by kind name, not by the strategies table's row id (which differs per deployment
# and per cloned sub-strategy). A kind the model was not trained on one-hots to all zeros, which
# reads as "some strategy I don't recognize" rather than colliding with a known one.
#
# Keep in sync with strategy.Factories; appending is safe, reordering is not.
STRATEGY_KINDS = [
    "rsi_sma",
    "rsi_sma_fuzzy",
    "double_top_bottom",
    "dual_ma_atr",
    "ema_cross_trailing",
    "grid_like",
    "pivot_reversal",
    "pmax",
    "seasonal_atr_short",
    "sma_cross_fixed_exit",
    "stepped_trailing",
    "stoch_cross",
    "trend_confluence",
    "weekly_dip_buy",
]

# TIMEFRAMES is the decision-cadence roster (CLAUDE.md §9): the bars strategies decide on. Context
# timeframes (4H/1D) are read by strategies but never carry a signal, so they need no slot here.
TIMEFRAMES = ["5m", "15m", "1H"]


def _one_hot(value: str, vocabulary: list[str]) -> np.ndarray:
    """One-hot over a fixed vocabulary; an unknown value yields all zeros rather than raising, so a
    newly-registered strategy kind degrades to "unrecognized" instead of crashing inference."""
    vec = np.zeros(len(vocabulary), dtype=np.float32)
    if value in vocabulary:
        vec[vocabulary.index(value)] = 1.0
    return vec


class StrategySignal(BaseModel):
    strategy_id: int
    side: str = ""
    confidence: float = 0.0
    sl_pct: float = 0.0
    tp_pct: float = 0.0
    # kind/bar are what let one shared policy tell signals apart (CLAUDE.md §15.10): which strategy
    # produced this, and on which timeframe. Without them every signal looks alike to the model.
    kind: str = ""
    bar: str = ""
    # This strategy's realized track record on this instrument, fed as input so the model can learn
    # to discount weak strategies. This is what replaced the strategy_weights output: win rate is a
    # better answer to "how much do I trust this" than a score the model has to invent, and it costs
    # no action-space width, so the strategy roster can change without retraining.
    win_rate: float = 0.0
    trade_count: float = 0.0
    # age_seconds is how stale this signal is. A higher-timeframe signal stays meaningful between
    # its candles, so signals are carried forward and aged rather than vanishing — this is what
    # makes an `update` observation complete instead of full of ambiguous zeros.
    age_seconds: float = 0.0


class PriceContext(BaseModel):
    close_pct_changes: list[float] = Field(default_factory=list)
    dist_to_swing_high_pct: float = 0.0
    dist_to_swing_low_pct: float = 0.0


class TimeframeBlock(BaseModel):
    bar: str
    strategy_signals: list[StrategySignal] = Field(default_factory=list)
    features: list[float] = Field(default_factory=list)
    price_context: PriceContext = Field(default_factory=PriceContext)


class RecentTrade(BaseModel):
    realized_pnl_usd: float
    win: bool


class MarketContext(BaseModel):
    """Fixed-width summary of the OTHER strategies currently holding an opinion on this instrument.

    One signal per /predict call (CLAUDE.md §15.10) means the model can't see two strategies
    agreeing within a single decision — and confluence is usually the strongest read there is. This
    block restores that without naming strategies individually, so its width is independent of the
    roster size and the ceiling stays gone.
    """

    others_long: float = 0.0        # count of other live signals currently on the long side
    others_short: float = 0.0
    mean_confidence: float = 0.0    # across those other live signals
    seconds_since_other: float = 0.0  # age of the most recent other signal


class PositionState(BaseModel):
    """The open position this call is about, for `update` and terminal categories (§15.10).

    Absent (position_open=0) on a buy/sell call, where the decision is whether to open at all.
    """

    position_open: float = 0.0
    entry_px: float = 0.0
    sl_px: float = 0.0
    tp_px: float = 0.0
    size_usd: float = 0.0
    leverage: float = 0.0
    age_seconds: float = 0.0
    # Realized PnL is meaningful only on a terminal category, where it IS the reward signal
    # (CLAUDE.md §15.10). Zero elsewhere.
    realized_pnl_usd: float = 0.0
    # A fork tracks its baseline parent rather than committing separate capital (§15.4); the model
    # should know it is reasoning about one, since fork outcomes are compared against the baseline.
    is_fork: float = 0.0


class Observation(BaseModel):
    # Go marshals a nil slice as JSON `null`, not `[]`, and Pydantic rejects null for a list field.
    # An observation with no recent trades (every one on a fresh install) would otherwise be a 422
    # from the caller's perspective and look like a schema mismatch. Coercing null -> [] here fixes
    # it for every list field at once, rather than requiring each Go call site to remember to
    # allocate empty slices. Same null-vs-[] class of bug CLAUDE.md §14 records hitting on the panel.
    @field_validator("timeframes", "recent_trades", "features", mode="before")
    @classmethod
    def _null_to_empty(cls, v):
        return [] if v is None else v

    schema_version: int = OBSERVATION_SCHEMA_VERSION
    inst_id: str
    # Ordered roster the token-identity one-hot is built against — must match what the loaded
    # global model was trained with (order defines each slot's index).
    active_tokens: list[str] = Field(default_factory=list)
    # Renamed from mid_price: this is the live tick ("last") price, not a bid/ask midpoint. Keep
    # in sync with domain.Observation.LastPrice on the Go side (json tag last_price).
    last_price: float
    timeframes: list[TimeframeBlock] = Field(default_factory=list)

    # --- signal lifecycle (CLAUDE.md §15.10) ---
    # category is which decision this call represents; see SIGNAL_CATEGORIES. Empty is treated as
    # "update" so a caller that predates this field still produces a well-formed vector.
    category: str = "update"
    # The single signal this call is about — one signal per call, so the model always knows exactly
    # which strategy and timeframe it is answering (§15.10). None on a pure price-driven update,
    # where no strategy spoke and only price/PnL moved.
    # typing.Optional, not `StrategySignal | None`: Pydantic evaluates annotations at runtime, and
    # the deployment target includes Python 3.9 where PEP 604 unions are not valid there.
    signal: Optional[StrategySignal] = None
    market_context: MarketContext = Field(default_factory=MarketContext)
    position_state: PositionState = Field(default_factory=PositionState)
    # Stable id of the order this call refers to, so a decision can be tied back to the position it
    # was about when the outcome finally lands. Not fed to the model (an id has no ordinal meaning);
    # carried for the caller's own bookkeeping and for training-time pairing.
    order_id: int = 0

    position: float = 0.0
    current_leverage: float = 0.0
    unrealized_pnl_pct: float = 0.0
    dist_to_sl_pct: float = 0.0
    dist_to_tp_pct: float = 0.0

    # The shared account pool every token trades against (CLAUDE.md §15.6). account_equity_usd is
    # the live running balance, account_initial_usd its configured starting point (so drawdown is
    # visible as a ratio), and open_exposure_usd how much of it is already committed to open
    # positions across ALL tokens — without that last one, one policy serving N tokens has no way
    # to avoid over-committing the shared pool.
    account_equity_usd: float = 0.0
    account_initial_usd: float = 0.0
    open_exposure_usd: float = 0.0

    recent_trades: list[RecentTrade] = Field(default_factory=list)

    # Legacy flat window, still accepted for the pre-Phase-A / cmd/trader no-op path (CLAUDE.md
    # §15.3's TODO on usecase/trade.go) until that loop is repointed at the global-agent design.
    features: list[float] = Field(default_factory=list)


class Action(BaseModel):
    action_schema_version: int = ACTION_SCHEMA_VERSION
    target_exposure: float
    leverage_frac: float
    sl_adjust_pct: float = 0.0
    tp_adjust_pct: float = 0.0
    # What to do with the open position this call was about: "none", "adjust", or "close"
    # (CLAUDE.md §15.10). Meaningful only for the `update` category — on buy/sell the decision is
    # target_exposure, and on a terminal category nothing is being decided at all.
    order_action: str = "none"
    confidence: float


def one_hot_token(inst_id: str, active_tokens: list[str]) -> np.ndarray:
    vec = np.zeros(len(active_tokens), dtype=np.float32)
    if inst_id in active_tokens:
        vec[active_tokens.index(inst_id)] = 1.0
    return vec


def observation_tail(obs: Observation) -> np.ndarray:
    """Token-identity one-hot + account/position scalars — CLAUDE.md §15.3.

    The account fields are fed as RATIOS, not raw dollars: a policy trained on a $100 account would
    otherwise see out-of-distribution inputs the moment the balance is reconfigured, and the
    decision it has to make ("what fraction of my account do I commit here") is scale-free anyway.
    equity_ratio is drawdown from the starting balance; exposure_ratio is how much of the account is
    already committed across all tokens (CLAUDE.md §15.6).
    """
    initial = obs.account_initial_usd or obs.account_equity_usd
    equity_ratio = obs.account_equity_usd / initial if initial else 0.0
    exposure_ratio = obs.open_exposure_usd / obs.account_equity_usd if obs.account_equity_usd else 0.0
    return np.concatenate(
        [
            one_hot_token(obs.inst_id, obs.active_tokens),
            np.array(
                [
                    obs.position,
                    obs.current_leverage,
                    obs.unrealized_pnl_pct,
                    obs.dist_to_sl_pct,
                    obs.dist_to_tp_pct,
                    equity_ratio,
                    exposure_ratio,
                ],
                dtype=np.float32,
            ),
            # The two blocks that §15.3 specified but that never actually reached the model until
            # the §15.10 redesign — the whole point of that revision.
            signal_block(obs),
            recent_trades_block(obs),
        ]
    )


def signal_block(obs: Observation) -> np.ndarray:
    """The signal lifecycle block: category, the signal itself, market context, position state.

    This is the part that was MISSING (CLAUDE.md §15.10): strategy signals were built in Go, sent,
    and parsed, but never reached the model's input vector — so the policy was asked to weigh
    strategies whose opinions it could not see. Everything here is fixed-width regardless of how
    many strategies are registered, because exactly one signal is carried per call.

    `present` disambiguates "no strategy spoke" from "a strategy said zero": without it a
    price-driven update is indistinguishable from a signal with zero confidence, and the model would
    learn from the ambiguity.
    """
    sig = obs.signal
    present = 1.0 if sig is not None else 0.0
    side = 0.0
    if sig is not None:
        side = 1.0 if sig.side == "buy" else (-1.0 if sig.side == "sell" else 0.0)

    scalars = [
        present,
        side,
        sig.confidence if sig else 0.0,
        sig.sl_pct if sig else 0.0,
        sig.tp_pct if sig else 0.0,
        sig.win_rate if sig else 0.0,
        # Trade count is compressed: the difference between 5 and 50 trades of evidence matters far
        # more than between 500 and 545, and an uncompressed count would dominate the vector's scale.
        np.log1p(sig.trade_count) if sig else 0.0,
        # Minutes, not seconds — keeps staleness on a similar scale to the other inputs.
        (sig.age_seconds / 60.0) if sig else 0.0,
    ]

    mc = obs.market_context
    ps = obs.position_state
    scalars.extend([
        mc.others_long,
        mc.others_short,
        mc.mean_confidence,
        mc.seconds_since_other / 60.0,
        ps.position_open,
        ps.leverage,
        ps.age_seconds / 60.0,
        ps.is_fork,
        # Position prices are fed RELATIVE to the live price, never as raw dollars: an entry of
        # 65000 and one of 0.15 are the same decision in different tokens, and raw levels would not
        # generalize across instruments the way one shared policy requires.
        _rel(ps.entry_px, obs.last_price),
        _rel(ps.sl_px, obs.last_price),
        _rel(ps.tp_px, obs.last_price),
        # Size as a fraction of the account, for the same scale-free reason (CLAUDE.md §15.6).
        (ps.size_usd / obs.account_equity_usd) if obs.account_equity_usd else 0.0,
    ])

    return np.concatenate([
        _one_hot(obs.category, SIGNAL_CATEGORIES),
        _one_hot(sig.kind if sig else "", STRATEGY_KINDS),
        _one_hot(sig.bar if sig else "", TIMEFRAMES),
        np.array(scalars, dtype=np.float32),
    ])


def _rel(level: float, price: float) -> float:
    """A price level as a signed fraction of the live price; 0.0 when either is absent."""
    if not level or not price:
        return 0.0
    return (level - price) / price


def recent_trades_block(obs: Observation, window: int = 10) -> np.ndarray:
    """Fixed-width tail of this token's recent realized outcomes (CLAUDE.md §15.3).

    Specified in §15.3 so the agent can learn to size down after a losing streak, but — like the
    strategy signals — it was being sent and then dropped before reaching the model. Padded at the
    FRONT so the most recent trade always lands in the last slot regardless of how many exist.
    """
    trades = obs.recent_trades[-window:]
    pnl = [t.realized_pnl_usd for t in trades]
    wins = [1.0 if t.win else -1.0 for t in trades]
    pad = window - len(trades)
    return np.array([0.0] * pad + pnl + [0.0] * pad + wins, dtype=np.float32)


def observation_features(obs: Observation) -> np.ndarray:
    """Flattened per-timeframe derived features + raw price context, then the legacy flat window —
    CLAUDE.md §15.3. Kept alongside (not instead of) strategy signals/derived features so the agent
    can reason about price action directly, not only through what a strategy chose to report."""
    tf_features: list[float] = []
    for block in obs.timeframes:
        tf_features.extend(block.features)
        tf_features.extend(block.price_context.close_pct_changes)
        tf_features.append(block.price_context.dist_to_swing_high_pct)
        tf_features.append(block.price_context.dist_to_swing_low_pct)
    return np.array(tf_features + list(obs.features), dtype=np.float32)


def to_vector(obs: Observation, expected_dim: int) -> np.ndarray:
    """Builds the exact model-input vector /predict and the replay env both use: features
    (padded/truncated to fit) concatenated with the fixed-size tail. Raises ValueError if the
    result still doesn't match expected_dim after padding/truncation (shouldn't happen given the
    padding logic below, but guards against a caller passing an inconsistent expected_dim)."""
    tail = observation_tail(obs)
    features = observation_features(obs)

    padded_len = expected_dim - len(tail)
    if len(features) < padded_len:
        features = np.pad(features, (padded_len - len(features), 0))
    elif len(features) > padded_len:
        features = features[-padded_len:]

    vec = np.concatenate([features, tail]).reshape(1, -1)
    if vec.shape[1] != expected_dim:
        raise ValueError(f"observation vector shape mismatch: got {vec.shape[1]}, want {expected_dim}")
    return vec


def decode_action(raw: np.ndarray, obs: Observation) -> Action:
    """Turns the policy's raw ACTION_DIM vector into the typed Action the Go caller consumes.

    Layout (CLAUDE.md §15.4/§15.10), all emitted by the policy in tanh-ish ranges and mapped here:
      [0] target_exposure  in [-1, 1]  -> used as-is (sign = side)
      [1] leverage_frac    in [0, 1]   -> mapped to [1x, max_leverage] by the caller
      [2] sl_adjust_pct    in [-1, 1]  -> scaled by MAX_SLTP_ADJUST_PCT to a fraction of price
      [3] tp_adjust_pct    in [-1, 1]  -> scaled by MAX_SLTP_ADJUST_PCT to a fraction of price
      [4:] order-action head           -> argmax over ORDER_ACTIONS

    Nothing here scales with the number of registered strategies — that is what lets the roster
    change without an action-space change or a retrain (§15.10). The Go side still applies its own
    ratchet/risk clamps to everything here — none of this is a safety boundary (CLAUDE.md §15.4).
    """
    vec = np.asarray(raw, dtype=np.float32).reshape(-1)
    if vec.shape[0] < ACTION_DIM:
        raise ValueError(f"action vector too short: got {vec.shape[0]}, want {ACTION_DIM}")

    target_exposure = float(np.clip(vec[0], -1.0, 1.0))
    leverage_frac = float(np.clip(vec[1], 0.0, 1.0))
    sl_adjust_pct = float(np.clip(vec[2], -1.0, 1.0)) * MAX_SLTP_ADJUST_PCT
    tp_adjust_pct = float(np.clip(vec[3], -1.0, 1.0)) * MAX_SLTP_ADJUST_PCT

    # The order-action head is an argmax over ORDER_ACTIONS rather than a threshold, so exactly one
    # action is always selected and the choice is scale-free.
    order_action = ORDER_ACTIONS[int(np.argmax(vec[4 : 4 + len(ORDER_ACTIONS)]))]

    return Action(
        order_action=order_action,
        target_exposure=target_exposure,
        leverage_frac=leverage_frac,
        sl_adjust_pct=sl_adjust_pct,
        tp_adjust_pct=tp_adjust_pct,
        # Confidence reports how decisively the policy sized this position, which is the only
        # self-assessment a PPO actor emits without a separate value head being plumbed through.
        confidence=abs(target_exposure),
    )


def flat_action() -> Action:
    """Fail-safe no-op action — no new risk, no SL/TP adjustment proposed. CLAUDE.md §5, §15.4."""
    return Action(target_exposure=0.0, leverage_frac=0.0, sl_adjust_pct=0.0, tp_adjust_pct=0.0, confidence=0.0)
