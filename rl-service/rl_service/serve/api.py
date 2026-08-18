"""FastAPI inference server exposing the trained RL policy over HTTP.

The Go trading engine (`go-engine/internal/rlclient`) calls POST /predict with the current
market/account observation and receives the agent's suggested action. Keep the request/response
schemas here in sync with `rlclient.Observation` / `rlclient.Action` on the Go side.
"""
from __future__ import annotations

import logging
import os

import numpy as np
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
from stable_baselines3 import PPO

from rl_service.config import load_config

logger = logging.getLogger("rl_service.serve")

app = FastAPI(title="okxBot RL inference service")

_cfg = load_config(os.environ.get("CONFIG_PATH"))
_model: PPO | None = None


class Observation(BaseModel):
    inst_id: str
    mid_price: float
    position: float
    current_leverage: float
    unrealized_pnl_pct: float
    equity_usd: float
    features: list[float] = []


class Action(BaseModel):
    target_exposure: float
    leverage_frac: float
    confidence: float


@app.on_event("startup")
def _load_model() -> None:
    global _model
    if os.path.exists(_cfg.serve.model_path):
        _model = PPO.load(_cfg.serve.model_path)
        logger.info("Loaded RL model from %s", _cfg.serve.model_path)
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
    if _model is None:
        # Fail safe: no model means no new risk is taken. The Go-side risk manager still applies
        # its own hard limits regardless of what this endpoint returns.
        return Action(target_exposure=0.0, leverage_frac=0.0, confidence=0.0)

    expected_dim = _model.observation_space.shape[0]
    tail = np.array(
        [obs.position, obs.current_leverage, obs.unrealized_pnl_pct, obs.equity_usd],
        dtype=np.float32,
    )
    features = np.array(obs.features, dtype=np.float32)

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
    return Action(target_exposure=target_exposure, leverage_frac=leverage_frac, confidence=1.0)
