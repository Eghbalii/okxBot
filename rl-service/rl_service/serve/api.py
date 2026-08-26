"""FastAPI inference server exposing the single global RL policy over HTTP (CLAUDE.md §15).

The Go trading engine (`go-engine/internal/rlclient`) calls POST /predict with the current
market/account observation for one token and receives the agent's suggested action. One shared
model serves every token's requests — token identity is an observation input (a one-hot over
Observation.active_tokens), not a model-selection key (CLAUDE.md §15.1). The Observation/Action
schema and vectorization logic live in rl_service/obs.py, shared with the warm-start replay env
(rl_service/env/replay_env.py, §15.8) so both build the exact same model-input vector. Keep both
in sync with `domain.Observation` / `domain.Action` on the Go side
(go-engine/internal/domain/rl.go) — including OBSERVATION_SCHEMA_VERSION vs
domain.ObservationSchemaVersion.
"""
from __future__ import annotations

import logging
import os

from fastapi import FastAPI, HTTPException
from stable_baselines3 import PPO

from rl_service.config import load_config
from rl_service.obs import (
    OBSERVATION_SCHEMA_VERSION,
    Action,
    Observation,
    flat_action,
    to_vector,
)

logger = logging.getLogger("rl_service.serve")

app = FastAPI(title="okxBot RL inference service")

_cfg = load_config(os.environ.get("CONFIG_PATH"))

# Single global policy (CLAUDE.md §15.1) — every token's /predict call is answered by this one
# model; token identity reaches the model only through the observation vector's one-hot, not
# through model selection. None until a trained model exists at _cfg.serve.model_path.
_model: PPO | None = None


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
        return flat_action()

    expected_dim = _model.observation_space.shape[0]
    try:
        obs_vec = to_vector(obs, expected_dim)
    except ValueError as e:
        raise HTTPException(status_code=422, detail=str(e)) from e

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
