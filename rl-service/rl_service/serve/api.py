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

**One endpoint, always /predict** (CLAUDE.md §15.11). Terminal (`closed_*`) calls also come here:
they return an action the caller discards, and their realized PnL becomes the reward that trains
every decision that preceded them. There is no separate training route and no queue — one request,
one response.

The algorithm is **SAC**, not PPO (§15.11): PPO is on-policy and can only learn from a fresh batch
of its own current policy's actions, which at tens of trades/day takes weeks to fill. SAC's replay
buffer learns from a trickle, which is what makes continuous learning possible here at all.
"""
from __future__ import annotations

import logging
import os
from typing import Optional

import numpy as np
from fastapi import FastAPI, HTTPException
from stable_baselines3 import SAC
from stable_baselines3.common.utils import update_learning_rate

from rl_service.config import load_config
from rl_service.learner import Learner, reward_from_outcome
from rl_service.obs import (
    ACTION_DIM,
    ACTION_SCHEMA_VERSION,
    OBSERVATION_SCHEMA_VERSION,
    TERMINAL_CATEGORIES,
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
_model: Optional[SAC] = None

# Present only when continuous learning is enabled (CLAUDE.md §15.11). None means the service serves
# frozen weights, which is the required posture for real money.
_learner: Optional[Learner] = None


@app.on_event("startup")
def _load_model() -> None:
    global _model, _learner

    if not os.path.exists(_cfg.serve.model_path):
        logger.warning(
            "No trained model found at %s; /predict will return a flat (no-op) action until a "
            "model is trained and placed there.",
            _cfg.serve.model_path,
        )
        return

    _model = SAC.load(_cfg.serve.model_path, learning_rate=_cfg.serve.learning_rate)

    # SAC.load sets `learning_rate` as a plain attribute, but the checkpoint's serialized
    # `lr_schedule` closure (built from the OLD rate at save time) overrides it unless rebuilt here
    # — verified directly: without this, the actor/critic optimizers kept running at the
    # checkpoint's original rate even though `_model.learning_rate` read the new value.
    _model._setup_lr_schedule()
    new_lr = _model.lr_schedule(1)
    update_learning_rate(_model.actor.optimizer, new_lr)
    update_learning_rate(_model.critic.optimizer, new_lr)
    if getattr(_model, "ent_coef_optimizer", None) is not None:
        update_learning_rate(_model.ent_coef_optimizer, new_lr)

    logger.info(
        "Loaded global RL model from %s (learning_rate overridden to %s, applied to actor/critic"
        "/ent_coef optimizers)",
        _cfg.serve.model_path, _cfg.serve.learning_rate,
    )

    if not _cfg.serve.learning_enabled:
        logger.info("Continuous learning disabled; serving frozen weights.")
        return

    # A model loaded for inference has no replay buffer — SB3 drops it on save unless it is written
    # separately. Restoring it is what makes learning resume rather than restart: without it the
    # service would come back having forgotten every experience it ever collected.
    _model.replay_buffer_class = None
    if os.path.exists(_cfg.serve.buffer_path):
        try:
            _model.load_replay_buffer(_cfg.serve.buffer_path)
            logger.info(
                "Restored replay buffer from %s (%d experiences)",
                _cfg.serve.buffer_path, _model.replay_buffer.size(),
            )
        except Exception:
            logger.exception("Could not restore replay buffer; starting with an empty one")

    _learner = Learner(
        _model,
        learning_starts=_cfg.serve.learning_starts,
        gradient_steps=_cfg.serve.gradient_steps,
        snapshot_every=_cfg.serve.snapshot_every,
        model_path=_cfg.serve.model_path,
        buffer_path=_cfg.serve.buffer_path,
    )
    logger.info("Continuous learning ENABLED (CLAUDE.md §15.11) — freeze this for real money.")


@app.on_event("shutdown")
def _snapshot_on_shutdown() -> None:
    """A clean shutdown must not throw away experience collected since the last snapshot."""
    if _learner is not None:
        try:
            _learner.snapshot()
        except Exception:
            logger.exception("shutdown snapshot failed")


@app.get("/health")
def health():
    # action_compatible distinguishes "up but unloaded" from "up with a model too old to serve the
    # current action schema" — CLAUDE.md §11.2 makes the same point about model_loaded, and the
    # panel needs to tell these apart for the same reason: they mean very different things.
    action_compatible = _model is not None and _model.action_space.shape[0] == ACTION_DIM
    body = {
        "status": "ok",
        "model_loaded": _model is not None,
        "action_schema_version": ACTION_SCHEMA_VERSION,
        "observation_schema_version": OBSERVATION_SCHEMA_VERSION,
        "action_compatible": action_compatible,
        "learning_enabled": _learner is not None,
    }
    if _learner is not None:
        body["learning"] = _learner.stats()
    return body


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

    # A model trained against an older, narrower action space cannot answer the current one —
    # refuse rather than silently serving values that would look like real decisions.
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
    raw = np.asarray(raw).reshape(-1)

    if _learner is not None:
        _learn(obs, obs_vec, raw)

    return decode_action(raw, obs)


def _learn(obs: Observation, obs_vec: np.ndarray, raw: np.ndarray) -> None:
    """Feeds this call into the learning loop (CLAUDE.md §15.11).

    A terminal call CLOSES the loop: it carries the realized PnL that scores the decision recorded
    when the position was opened. Every other call OPENS one, holding the decision until its
    outcome arrives — which may be hours later, and is why decisions are keyed by order id rather
    than assumed to resolve in order.
    """
    assert _learner is not None

    if obs.category in TERMINAL_CATEGORIES:
        reward = reward_from_outcome(
            obs.position_state.realized_pnl_usd,
            obs.account_initial_usd or obs.account_equity_usd,
            obs.position_state.size_usd,
        )
        matched = _learner.complete(obs.order_id, reward, obs_vec.reshape(-1))
        if not matched:
            # Expected after a restart, or for trades opened before learning was enabled — the
            # decision that produced them was never recorded, so there is nothing to score.
            logger.debug("terminal call for unknown order %s; nothing to score", obs.order_id)
        return

    _learner.record(obs.order_id, obs_vec.reshape(-1), raw)
