"""FastAPI inference server exposing the single global RL policy over HTTP (CLAUDE.md §15).

The Go trading engine (`go-engine/internal/rlclient`) calls POST /predict with the current
market/account observation for one token and receives the agent's suggested action. One shared
model serves every token's requests — token identity is an observation input (a one-hot over
Observation.active_tokens), not a model-selection key (CLAUDE.md §15.1). Keep the request/response
schemas here in sync with `domain.Observation` / `domain.Action` on the Go side
(go-engine/internal/domain/rl.go) — including OBSERVATION_SCHEMA_VERSION vs
domain.ObservationSchemaVersion.
"""
from __future__ import annotations

import logging
import os

import numpy as np
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field
from stable_baselines3 import PPO

from rl_service.config import load_config

logger = logging.getLogger("rl_service.serve")

app = FastAPI(title="okxBot RL inference service")

_cfg = load_config(os.environ.get("CONFIG_PATH"))

# Observation schema version this service understands — must match domain.ObservationSchemaVersion
# on the Go side (go-engine/internal/domain/rl.go). CLAUDE.md §15.3: bump both together whenever
# the observation shape changes, so a stale/mismatched caller gets a clear 422 instead of a
# silently misaligned feature vector.
#
# v3: switched to a single global agent (CLAUDE.md §15.1) — added active_tokens (token-identity
# one-hot) and price_context (raw price series + positional/distance features) per timeframe block,
# plus dist_to_sl_pct/dist_to_tp_pct on the top-level observation.
OBSERVATION_SCHEMA_VERSION = 3

# Single global policy (CLAUDE.md §15.1) — every token's /predict call is answered by this one
# model; token identity reaches the model only through the observation vector's one-hot, not
# through model selection. None until a trained model exists at _cfg.serve.model_path.
_model: PPO | None = None


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
    mid_price: float
    timeframes: list[TimeframeBlock] = Field(default_factory=list)

    position: float
    current_leverage: float
    unrealized_pnl_pct: float
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


def _flat_action() -> Action:
    # Fail safe: no model loaded means no new risk is taken and no in-trade SL/TP adjustment is
    # proposed. The Go-side risk manager and SL/TP ratchet clamp still apply their own hard limits
    # regardless of what this endpoint returns (CLAUDE.md §5, §15.4).
    return Action(target_exposure=0.0, leverage_frac=0.0, sl_adjust_pct=0.0, tp_adjust_pct=0.0, confidence=0.0)


def _one_hot(inst_id: str, active_tokens: list[str]) -> np.ndarray:
    vec = np.zeros(len(active_tokens), dtype=np.float32)
    if inst_id in active_tokens:
        vec[active_tokens.index(inst_id)] = 1.0
    return vec


@app.on_event("startup")
def _load_model() -> None:
    global _model
    if os.path.exists(_cfg.serve.model_path):
        _model = PPO.load(_cfg.serve.model_path)
        logger.info("Loaded global RL model from %s", _cfg.serve.model_path)
    else:
        logger.warning(
            "No trained model found at %s; /predict will return a flat (no-op) action until a "
            "model is trained and placed there.",
            _cfg.serve.model_path,
        )


@app.get("/health")
def health():
    return {"status": "ok", "model_loaded": _model is not None}


@app.post("/predict", response_model=Action)
def predict(obs: Observation) -> Action:
    if obs.schema_version != OBSERVATION_SCHEMA_VERSION:
        raise HTTPException(
            status_code=422,
            detail=(
                f"observation schema_version mismatch: got {obs.schema_version}, "
                f"this service understands {OBSERVATION_SCHEMA_VERSION}"
            ),
        )

    if _model is None:
        return _flat_action()

    expected_dim = _model.observation_space.shape[0]
    token_id_vec = _one_hot(obs.inst_id, obs.active_tokens)
    tail = np.concatenate(
        [
            token_id_vec,
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

    # Flatten timeframe blocks: derived features, then raw price context (close returns + swing
    # distances) — kept alongside, not instead of, the derived features, so the agent can reason
    # about price action directly rather than only through strategies' interpretation of it
    # (CLAUDE.md §15.3). Legacy flat `features` window appended last for the pre-Phase-A no-op path.
    tf_features: list[float] = []
    for block in obs.timeframes:
        tf_features.extend(block.features)
        tf_features.extend(block.price_context.close_pct_changes)
        tf_features.append(block.price_context.dist_to_swing_high_pct)
        tf_features.append(block.price_context.dist_to_swing_low_pct)
    features = np.array(tf_features + list(obs.features), dtype=np.float32)

    padded_len = expected_dim - len(tail)
    if len(features) < padded_len:
        features = np.pad(features, (padded_len - len(features), 0))
    elif len(features) > padded_len:
        features = features[-padded_len:]

    obs_vec = np.concatenate([features, tail]).reshape(1, -1)
    if obs_vec.shape[1] != expected_dim:
        raise HTTPException(status_code=422, detail=f"observation shape mismatch: got {obs_vec.shape[1]}, want {expected_dim}")

    action, _ = _model.predict(obs_vec, deterministic=True)
    target_exposure, leverage_frac = float(action[0][0]), float(action[0][1])

    # strategy_weights/sl_adjust_pct/tp_adjust_pct are not yet produced by a trained model (today's
    # action_space is still the original Box(2,) in okx_futures_env.py) — returned as neutral
    # defaults until the env/training side is extended to the full CLAUDE.md §15.4 action space.
    strategy_weights = {
        str(sig.strategy_id): 1.0 / max(1, sum(len(b.strategy_signals) for b in obs.timeframes))
        for block in obs.timeframes
        for sig in block.strategy_signals
    }
    return Action(
        strategy_weights=strategy_weights,
        target_exposure=target_exposure,
        leverage_frac=leverage_frac,
        sl_adjust_pct=0.0,
        tp_adjust_pct=0.0,
        confidence=1.0,
    )
