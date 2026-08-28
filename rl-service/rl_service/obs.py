"""Shared observation/action schema and vectorization for the global RL agent (CLAUDE.md §15).

Single source of truth for both the live inference path (rl_service/serve/api.py) and the
warm-start replay env (rl_service/env/replay_env.py) — they MUST build the exact same flattened
vector from an Observation, or a model trained by one and served by the other silently misaligns
features. Keep in sync with `domain.Observation` / `domain.Action` on the Go side
(go-engine/internal/domain/rl.go).
"""
from __future__ import annotations

import numpy as np
from pydantic import BaseModel, Field

# v3: switched to a single global agent (CLAUDE.md §15.1) — added active_tokens (token-identity
# one-hot) and price_context (raw price series + positional/distance features) per timeframe block,
# plus dist_to_sl_pct/dist_to_tp_pct on the top-level observation.
#
# v4 (current): replaced the per-token sub-budget fields (token_equity_usd/token_budget_usd) with
# the shared-account fields account_equity_usd/account_initial_usd/open_exposure_usd (CLAUDE.md
# §15.6's 2026-08-28 revision) — capital is one pool the agent sizes trades against, not a per-token
# constant. Must match domain.ObservationSchemaVersion on the Go side.
OBSERVATION_SCHEMA_VERSION = 4

# MAX_STRATEGY_SLOTS bounds the policy's fixed-width strategy_weights output (CLAUDE.md §15.4).
# PPO's action space must be a fixed shape, but the number of strategies assigned to a given
# token+timeframe varies per request — so the policy emits this many weight slots and the
# observation's strategy signals are read positionally against them: slot i corresponds to the i-th
# strategy signal in the request (ordered as go-engine's buildObservation appends them, which is
# assignment order). Signals beyond this count are ignored by the weighting; slots beyond the number
# of present signals are dropped from the response. Raising this changes the action width and
# invalidates existing trained models — bump ACTION_SCHEMA_VERSION with it.
MAX_STRATEGY_SLOTS = 8

# ACTION_SCHEMA_VERSION tracks the ACTION vector's layout, independently of the observation's
# schema_version — a model trained against a narrower action space cannot serve a caller expecting
# the wider one. v1 was the original Box(2,) [target_exposure, leverage_frac]; v2 is the full
# CLAUDE.md §15.4 action: [target_exposure, leverage_frac, sl_adjust_pct, tp_adjust_pct,
# w_0..w_{MAX_STRATEGY_SLOTS-1}]. Keep in sync with domain.ActionSchemaVersion on the Go side.
ACTION_SCHEMA_VERSION = 2

# ACTION_DIM is the policy's output width: the 4 scalar decisions plus the strategy-weight slots.
ACTION_DIM = 4 + MAX_STRATEGY_SLOTS

# SL/TP adjustment outputs are emitted in [-1, 1] by the policy and scaled to a fraction of price by
# MAX_SLTP_ADJUST_PCT. This mirrors usecase.MaxSLTPAdjustPct on the Go side (CLAUDE.md §15.4's "±2%
# per decision step") so the model's raw output range maps onto the same bounded adjustment the Go
# ratchet will clamp it to anyway — keeping the two in sync means the policy explores the full range
# the ratchet actually accepts, instead of spending most of its output range on values Go clips.
MAX_SLTP_ADJUST_PCT = 0.02


class StrategySignal(BaseModel):
    strategy_id: int
    side: str = ""
    confidence: float = 0.0
    sl_pct: float = 0.0
    tp_pct: float = 0.0


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


class Observation(BaseModel):
    schema_version: int = OBSERVATION_SCHEMA_VERSION
    inst_id: str
    # Ordered roster the token-identity one-hot is built against — must match what the loaded
    # global model was trained with (order defines each slot's index).
    active_tokens: list[str] = Field(default_factory=list)
    # Renamed from mid_price: this is the live tick ("last") price, not a bid/ask midpoint. Keep
    # in sync with domain.Observation.LastPrice on the Go side (json tag last_price).
    last_price: float
    timeframes: list[TimeframeBlock] = Field(default_factory=list)

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
    strategy_weights: dict[str, float] = Field(default_factory=dict)
    target_exposure: float
    leverage_frac: float
    sl_adjust_pct: float = 0.0
    tp_adjust_pct: float = 0.0
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
        ]
    )


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


def ordered_strategy_ids(obs: Observation) -> list[int]:
    """The strategy ids the policy's weight slots map onto, in slot order (CLAUDE.md §15.4).

    Flattened across timeframe blocks in request order — the same order go-engine's
    buildObservation appends them — and truncated to MAX_STRATEGY_SLOTS. Duplicate ids (the same
    strategy assigned to more than one timeframe) each get their own slot, since they carry
    genuinely different signals; the response dict then keys by id, so the last one wins there.
    """
    ids: list[int] = []
    for block in obs.timeframes:
        for sig in block.strategy_signals:
            ids.append(sig.strategy_id)
            if len(ids) >= MAX_STRATEGY_SLOTS:
                return ids
    return ids


def decode_action(raw: np.ndarray, obs: Observation) -> Action:
    """Turns the policy's raw ACTION_DIM vector into the typed Action the Go caller consumes.

    Layout (CLAUDE.md §15.4), all emitted by the policy in tanh-ish ranges and mapped here:
      [0] target_exposure  in [-1, 1]  -> used as-is (sign = side)
      [1] leverage_frac    in [0, 1]   -> mapped to [1x, max_leverage] by the caller
      [2] sl_adjust_pct    in [-1, 1]  -> scaled by MAX_SLTP_ADJUST_PCT to a fraction of price
      [3] tp_adjust_pct    in [-1, 1]  -> scaled by MAX_SLTP_ADJUST_PCT to a fraction of price
      [4:] strategy weight slots       -> clipped to [0, 1] and normalized to sum to 1 across the
                                          strategies actually present in this request

    Only the slots backed by a real strategy signal are returned, so a request carrying two
    strategies gets a two-entry dict regardless of MAX_STRATEGY_SLOTS. The Go side still applies its
    own ratchet/risk clamps to everything here — none of this is a safety boundary (CLAUDE.md §15.4).
    """
    vec = np.asarray(raw, dtype=np.float32).reshape(-1)
    if vec.shape[0] < ACTION_DIM:
        raise ValueError(f"action vector too short: got {vec.shape[0]}, want {ACTION_DIM}")

    target_exposure = float(np.clip(vec[0], -1.0, 1.0))
    leverage_frac = float(np.clip(vec[1], 0.0, 1.0))
    sl_adjust_pct = float(np.clip(vec[2], -1.0, 1.0)) * MAX_SLTP_ADJUST_PCT
    tp_adjust_pct = float(np.clip(vec[3], -1.0, 1.0)) * MAX_SLTP_ADJUST_PCT

    ids = ordered_strategy_ids(obs)
    weights = np.clip(vec[4 : 4 + len(ids)], 0.0, 1.0)
    total = float(weights.sum())
    # An all-zero output means the policy trusts none of them; fall back to uniform rather than
    # emitting a dict of zeros, which the caller would read as "no directional signal at all" and
    # which is indistinguishable from a bug on its side.
    if total <= 0.0:
        weights = np.full(len(ids), 1.0 / len(ids), dtype=np.float32) if ids else weights
    else:
        weights = weights / total

    return Action(
        strategy_weights={str(sid): float(w) for sid, w in zip(ids, weights)},
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
