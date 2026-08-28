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
# v5: the event-driven signal lifecycle (CLAUDE.md §15.10). Added category and a single per-call
# `signal` — and, critically, actually fed strategy signals into the model's input vector, which
# v3/v4 never did despite carrying them over the wire the whole time.
#
# v6 (current): CLAUDE.md §15.11. Signal SL/TP became prices rather than percentages (a strategy
# derives a level from chart structure; a percentage discards that) and gained entry_px; the two
# overlapping position blocks merged into one carrying pnl_max/pnl_min and age; price context now
# includes the LIVE FORMING candle's OHLC; market_context and recent_trades dropped. One-hot
# vocabularies are over-provisioned so the roster can grow without a retrain. Must match
# domain.ObservationSchemaVersion on the Go side.
OBSERVATION_SCHEMA_VERSION = 6

# ACTIONS is the model's single decision field (CLAUDE.md §15.11). Deliberately named to match the
# input categories so the same word means the same thing on both sides of the call: the model sees
# category "update" and answers "update".
#
# Which values are valid depends on the request's category — the model always emits all of them and
# the controller accepts only those that make sense (a "close" on a buy call has nothing to close):
#   buy / sell   -> open | skip
#   update       -> none | update | close
#   closed_*     -> ignored; that call exists to deliver reward, not to ask anything
#
# Order defines the argmax index and must stay stable.
ACTIONS = ["open", "skip", "none", "update", "close"]

# ACTION_SCHEMA_VERSION tracks the ACTION vector's layout, independently of the observation's
# schema_version — a model trained against a different action space cannot serve a caller expecting
# this one. v1: Box(2,) [target_exposure, leverage_frac]. v2: added sl/tp_adjust plus fixed
# strategy_weight slots. v3: dropped strategy_weights, added an order-action head.
#
# v4 (current): the model now SETS sl_px/tp_px as prices rather than proposing percentage
# adjustments, and the action head covers the full lifecycle (CLAUDE.md §15.11). Nothing here scales
# with the strategy roster, which is what lets strategies be added or removed without retraining.
# Keep in sync with domain.ActionSchemaVersion.
ACTION_SCHEMA_VERSION = 4

# ACTION_DIM: sl_px, tp_px (both as offsets from live price), size_pct, leverage_frac + action head.
ACTION_DIM = 4 + len(ACTIONS)

# The policy emits SL/TP in [-1, 1] and they are mapped to a price by scaling against
# MAX_SLTP_OFFSET_PCT of the live price. Prices themselves can't be emitted directly — a network
# output has no idea whether this instrument trades at 0.15 or 65000 — so the model chooses a
# DISTANCE and the caller turns it into a level. Go re-clamps the result regardless (§15.11), this
# just keeps the policy's output range aligned with the range Go will actually accept.
MAX_SLTP_OFFSET_PCT = 0.10


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

# TIMEFRAMES lists every bar that can carry a signal. Only 5m/15m/1H are decision timeframes today
# (CLAUDE.md §9); the rest are listed so adding one later doesn't change the vector width.
TIMEFRAMES = ["5m", "15m", "1H", "4H", "1D", "1m", "3m", "30m", "2H", "6H", "12H", "1W"]

# One-hot widths are over-provisioned on purpose (CLAUDE.md §15.11). A one-hot is fixed-width, so
# the vector — and any model trained against it — is sized by these numbers, not by how many
# entries are in use. Reserving spare slots costs a few always-zero inputs; crossing a ceiling costs
# a full retrain. Today: 14 of 24 kinds, 3 of 16 timeframes, 2 of 16 tokens.
#
# APPENDING to a vocabulary is safe. REORDERING or inserting in the middle is not: it silently
# reassigns every later slot's meaning for an already-trained model, which corrupts the policy
# without any error to notice.
MAX_STRATEGY_KIND_SLOTS = 24
MAX_TIMEFRAME_SLOTS = 16
MAX_TOKEN_SLOTS = 16


def _one_hot(value: str, vocabulary: list[str], width: int) -> np.ndarray:
    """One-hot over a fixed vocabulary, padded out to `width` reserved slots.

    An unknown value yields all zeros rather than raising, so a strategy kind or timeframe the model
    was never trained on degrades to "unrecognized" instead of crashing inference or — worse —
    colliding with a slot that means something else.
    """
    vec = np.zeros(width, dtype=np.float32)
    if value in vocabulary:
        idx = vocabulary.index(value)
        if idx < width:
            vec[idx] = 1.0
    return vec


class StrategySignal(BaseModel):
    strategy_id: int
    side: str = ""
    confidence: float = 0.0

    # PRICES, not percentages (CLAUDE.md §15.11). A strategy derives these from chart structure — a
    # stop below a swing low, a target at a fair-value gap — and converting a level to a percentage
    # throws away exactly the structural information that made it a level. Vectorized as offsets
    # from the live price so one shared policy still generalizes across instruments.
    entry_px: float = 0.0
    sl_px: float = 0.0
    tp_px: float = 0.0

    # kind/bar are what let one shared policy tell signals apart (CLAUDE.md §15.10): which strategy
    # produced this, and on which timeframe. Without them every signal looks alike to the model.
    kind: str = ""
    bar: str = ""

    # This strategy's realized track record on this instrument, fed as input so the model can learn
    # to discount weak strategies. This is what replaced the strategy_weights output: win rate is a
    # better answer to "how much do I trust this" than a score the model has to invent, and it costs
    # no action-space width, so the strategy roster can change without retraining.
    win_rate: float = 0.0
    # Log-compressed at vectorization time: 100% of 2 trades and 60% of 200 are very different
    # evidence, but the difference between 500 and 545 trades is not worth linear scale.
    trade_count: float = 0.0


class PriceContext(BaseModel):
    """Recent price action for one timeframe.

    ohlc is the LIVE FORMING candle, not the last closed one (CLAUDE.md §15.11): OKX pushes the
    in-progress bar on the same WS channel, and on a 1H timeframe the last *closed* candle can be
    59 minutes stale — the same freshness problem §15.9's audit found on the SL/TP path.
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


class TimeframeBlock(BaseModel):
    # Go marshals nil slices as JSON `null` (see Observation's validator) and these are nested one
    # level deeper, so they need the same coercion — a timeframe with no strategy signals is the
    # normal case on a quiet bar, not an error.
    @field_validator("strategy_signals", "features", mode="before")
    @classmethod
    def _null_to_empty(cls, v):
        return [] if v is None else v

    bar: str
    strategy_signals: list[StrategySignal] = Field(default_factory=list)
    features: list[float] = Field(default_factory=list)
    price_context: PriceContext = Field(default_factory=PriceContext)


class PositionState(BaseModel):
    """The open position this call is about, for `update` and terminal categories (§15.11).

    Zero-valued on a buy/sell call, where the decision is whether to open at all. Entry/SL/TP are
    deliberately NOT repeated here — they are already carried on the signal, and duplicating them
    would spend input width on the same numbers twice.
    """

    position_open: float = 0.0
    side: float = 0.0  # +1 long, -1 short, 0 flat
    size_usd: float = 0.0
    leverage: float = 0.0

    # age_seconds is what separates "+30% in 10 minutes" from "-5% after 4 hours" — current PnL
    # alone cannot express that difference, and they are very different trades.
    age_seconds: float = 0.0

    unrealized_pnl_pct: float = 0.0
    # How far this position travelled in each direction, not just where it sits now (CLAUDE.md
    # §15.11). A trade that reached 90% of its target and gave it all back teaches something
    # completely different from one that drifted sideways to the same current PnL, and without
    # these two the model cannot tell them apart. pnl_min is negative-ranged.
    pnl_max_pct: float = 0.0
    pnl_min_pct: float = 0.0

    dist_to_sl_pct: float = 0.0
    dist_to_tp_pct: float = 0.0

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
    @field_validator("timeframes", "features", mode="before")
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
    position_state: PositionState = Field(default_factory=PositionState)
    # Stable id of the order this call refers to, so a decision can be tied back to the position it
    # was about when the outcome finally lands. Not fed to the model (an id has no ordinal meaning);
    # carried for the caller's own bookkeeping and for training-time pairing.
    order_id: int = 0

    # The shared account pool every token trades against (CLAUDE.md §15.6). account_equity_usd is
    # the live running balance, account_initial_usd its configured starting point (so drawdown is
    # visible as a ratio), and open_exposure_usd how much of it is already committed to open
    # positions across ALL tokens — without that last one, one policy serving N tokens has no way
    # to avoid over-committing the shared pool.
    account_equity_usd: float = 0.0
    account_initial_usd: float = 0.0
    open_exposure_usd: float = 0.0

    # Legacy flat window, still accepted for the pre-Phase-A / cmd/trader no-op path (CLAUDE.md
    # §15.3's TODO on usecase/trade.go) until that loop is repointed at the global-agent design.
    features: list[float] = Field(default_factory=list)


class Action(BaseModel):
    """The model's decision (CLAUDE.md §15.11).

    Which fields matter depends on the request's category: `open` uses all of them, `update` uses
    sl_px/tp_px, and `skip`/`none`/`close` use none. The model always emits every value; the
    controller applies only what is meaningful, and re-clamps everything regardless.
    """

    action_schema_version: int = ACTION_SCHEMA_VERSION
    # One of ACTIONS: open | skip on a buy/sell call, none | update | close on an update call.
    action: str = "skip"
    # Which way to open, for callers with no strategy layer to take direction from (cmd/trader's
    # poll loop). The paper path ignores this — there, direction belongs to the strategy that
    # produced the signal and the model only sizes the trade. Echoed from the request's signal.
    side: str = ""
    # PRICES, not percentages — the model SETS these, it does not merely nudge them. Derived from
    # the policy's chosen distance times the live price, since a network output cannot know an
    # instrument's price scale.
    sl_px: float = 0.0
    tp_px: float = 0.0
    # Fraction of account equity to commit, and leverage in [0, 1] mapped to [1x, max_leverage].
    size_pct: float = 0.0
    leverage_frac: float = 0.0
    # Echoed from the request so the controller can pair a response to the position it asked about.
    order_id: int = 0
    confidence: float = 0.0



def observation_tail(obs: Observation) -> np.ndarray:
    """Token identity + account state + the signal/position block — CLAUDE.md §15.11.

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
            _one_hot(obs.inst_id, obs.active_tokens, MAX_TOKEN_SLOTS),
            np.array([equity_ratio, exposure_ratio], dtype=np.float32),
            signal_block(obs),
            position_block(obs),
        ]
    )


def signal_block(obs: Observation) -> np.ndarray:
    """Category one-hot + the single strategy signal this call is about (CLAUDE.md §15.11).

    This is the part that was MISSING before §15.10: strategy signals were built in Go, sent, and
    parsed, but never reached the model's input vector — so the policy was asked to weigh strategies
    whose opinions it could not see. Fixed-width regardless of how many strategies are registered,
    because exactly one signal is carried per call.

    `present` disambiguates "no strategy spoke" from "a strategy said zero": without it a
    price-driven update is indistinguishable from a signal with zero confidence, and the model would
    learn from the ambiguity.
    """
    sig = obs.signal
    side = 0.0
    if sig is not None:
        side = 1.0 if sig.side == "buy" else (-1.0 if sig.side == "sell" else 0.0)

    scalars = np.array(
        [
            1.0 if sig is not None else 0.0,
            side,
            sig.confidence if sig else 0.0,
            # Prices as offsets from the live price: an entry at 65000 and one at 0.15 are the same
            # decision on different instruments, and raw levels would not generalize across the
            # tokens one shared policy has to serve.
            _rel(sig.entry_px, obs.last_price) if sig else 0.0,
            _rel(sig.sl_px, obs.last_price) if sig else 0.0,
            _rel(sig.tp_px, obs.last_price) if sig else 0.0,
            sig.win_rate if sig else 0.0,
            # Log-compressed: 5 vs 50 trades of evidence matters far more than 500 vs 545, and a
            # linear count would dominate the vector's scale.
            float(np.log1p(sig.trade_count)) if sig else 0.0,
        ],
        dtype=np.float32,
    )

    return np.concatenate([
        _one_hot(obs.category, SIGNAL_CATEGORIES, len(SIGNAL_CATEGORIES)),
        _one_hot(sig.kind if sig else "", STRATEGY_KINDS, MAX_STRATEGY_KIND_SLOTS),
        _one_hot(sig.bar if sig else "", TIMEFRAMES, MAX_TIMEFRAME_SLOTS),
        scalars,
    ])


def position_block(obs: Observation) -> np.ndarray:
    """The open position this call is about (CLAUDE.md §15.11).

    Entry/SL/TP are not repeated here — they are already on the signal. What this adds is the
    position's *trajectory*: how long it has been open, and how far it travelled in each direction,
    which current PnL alone cannot express.
    """
    ps = obs.position_state
    return np.array(
        [
            ps.position_open,
            ps.side,
            ps.leverage,
            # Size as a fraction of the account, same scale-free reason as the account ratios.
            (ps.size_usd / obs.account_equity_usd) if obs.account_equity_usd else 0.0,
            ps.is_fork,
            # Hours, not seconds: keeps a multi-hour position on a similar scale to the other inputs
            # instead of a five-digit number that would dominate them.
            ps.age_seconds / 3600.0,
            ps.unrealized_pnl_pct,
            ps.pnl_max_pct,
            ps.pnl_min_pct,
            ps.dist_to_sl_pct,
            ps.dist_to_tp_pct,
        ],
        dtype=np.float32,
    )


def _rel(level: float, price: float) -> float:
    """A price level as a signed fraction of the live price; 0.0 when either is absent."""
    if not level or not price:
        return 0.0
    return (level - price) / price


def observation_features(obs: Observation) -> np.ndarray:
    """Per-timeframe derived indicators + raw price action, then the legacy flat window.

    Kept alongside (not instead of) the strategy signal so the agent can reason about price action
    directly, not only through what a strategy chose to report (CLAUDE.md §15.3).

    The candle's OHLC is the LIVE FORMING bar and is fed relative to the live price, so it stays
    scale-free across instruments like every other price in the vector.
    """
    tf_features: list[float] = []
    for block in obs.timeframes:
        tf_features.extend(block.features)
        pc = block.price_context
        tf_features.extend([
            _rel(pc.open, obs.last_price),
            _rel(pc.high, obs.last_price),
            _rel(pc.low, obs.last_price),
            _rel(pc.close, obs.last_price),
        ])
        tf_features.extend(pc.close_pct_changes)
        tf_features.append(pc.dist_to_swing_high_pct)
        tf_features.append(pc.dist_to_swing_low_pct)
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

    Layout (CLAUDE.md §15.11), all emitted by the policy in [-1, 1] and mapped here:
      [0] sl offset       -> scaled by MAX_SLTP_OFFSET_PCT, turned into a PRICE against last_price
      [1] tp offset       -> same
      [2] size_pct        in [0, 1] — fraction of account equity to commit
      [3] leverage_frac   in [0, 1] — mapped to [1x, max_leverage] by the caller
      [4:] action head    -> argmax over ACTIONS

    The model chooses SL/TP as a DISTANCE and this turns it into a level, because a raw network
    output has no way to know whether the instrument trades at 0.15 or 65000. Which action values
    are valid depends on obs.category; the model always emits all of them and the caller accepts
    only the meaningful ones.

    Nothing here scales with the number of registered strategies — that is what lets the roster
    change without an action-space change or a retrain. The Go side re-clamps everything (§15.11);
    none of this is a safety boundary.
    """
    vec = np.asarray(raw, dtype=np.float32).reshape(-1)
    if vec.shape[0] < ACTION_DIM:
        raise ValueError(f"action vector too short: got {vec.shape[0]}, want {ACTION_DIM}")

    sl_offset = float(np.clip(vec[0], -1.0, 1.0)) * MAX_SLTP_OFFSET_PCT
    tp_offset = float(np.clip(vec[1], -1.0, 1.0)) * MAX_SLTP_OFFSET_PCT
    size_pct = float(np.clip(vec[2], 0.0, 1.0))
    leverage_frac = float(np.clip(vec[3], 0.0, 1.0))

    # An argmax rather than a threshold, so exactly one action is always selected.
    action = ACTIONS[int(np.argmax(vec[4 : 4 + len(ACTIONS)]))]

    return Action(
        action=action,
        # Direction is never the model's to choose (CLAUDE.md §15.11) — echo the requesting
        # signal's side so a caller without a strategy layer still gets one.
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


def flat_action() -> Action:
    """Fail-safe no-op action — take no trade, touch nothing. CLAUDE.md §5, §15.11."""
    return Action(action="skip", sl_px=0.0, tp_px=0.0, size_pct=0.0, leverage_frac=0.0, confidence=0.0)
