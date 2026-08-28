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

import numpy as np
from fastapi import FastAPI, HTTPException
from stable_baselines3 import PPO

from rl_service.config import load_config
from rl_service.obs import (
    ACTION_DIM,
    ACTION_SCHEMA_VERSION,
    OBSERVATION_SCHEMA_VERSION,
    Action,
    Observation,
    decode_action,
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
    # action_compatible distinguishes "up but unloaded" from "up with a model too old to serve the
    # current action schema" — CLAUDE.md §11.2 makes the same point about model_loaded, and the
    # panel needs to tell these apart for the same reason: they mean very different things.
    action_compatible = _model is not None and _model.action_space.shape[0] == ACTION_DIM
    return {
        "status": "ok",
        "model_loaded": _model is not None,
        "action_schema_version": ACTION_SCHEMA_VERSION,
        "action_compatible": action_compatible,
    }


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

    # A model trained against the old Box(2,) action space cannot answer the full CLAUDE.md §15.4
    # action — refuse rather than silently returning zeros for sl/tp_adjust and uniform strategy
    # weights, which would look like a working model proposing no adjustments (exactly the failure
    # this endpoint used to have with its hardcoded defaults).
    action_dim = _model.action_space.shape[0]
    if action_dim != ACTION_DIM:
        raise HTTPException(
            status_code=503,
            detail=(
                f"loaded model has a {action_dim}-dim action space; this service serves "
                f"action_schema_version {ACTION_SCHEMA_VERSION} ({ACTION_DIM} dims). Retrain "
                "against the current env (python -m rl_service.train --warm-start)."
            ),
        )

    raw, _ = _model.predict(obs_vec, deterministic=True)
    return decode_action(np.asarray(raw).reshape(-1), obs)
