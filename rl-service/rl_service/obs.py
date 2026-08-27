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
# plus dist_to_sl_pct/dist_to_tp_pct on the top-level observation. Must match
# domain.ObservationSchemaVersion on the Go side.
OBSERVATION_SCHEMA_VERSION = 3


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

    token_equity_usd: float = 0.0
    token_budget_usd: float = 0.0

    recent_trades: list[RecentTrade] = Field(default_factory=list)

    # Legacy flat window, still accepted for the pre-Phase-A / cmd/trader no-op path (CLAUDE.md
    # §15.3's TODO on usecase/trade.go) until that loop is repointed at the global-agent design.
    features: list[float] = Field(default_factory=list)


class Action(BaseModel):
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
    """Token-identity one-hot + account/position scalars — CLAUDE.md §15.3."""
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
                    obs.token_equity_usd,
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


def flat_action() -> Action:
    """Fail-safe no-op action — no new risk, no SL/TP adjustment proposed. CLAUDE.md §5, §15.4."""
    return Action(target_exposure=0.0, leverage_frac=0.0, sl_adjust_pct=0.0, tp_adjust_pct=0.0, confidence=0.0)
