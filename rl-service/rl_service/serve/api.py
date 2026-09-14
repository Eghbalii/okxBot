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
from rl_service.learner import Learner
from rl_service.obs import (
    ACTION_DIM,
    ACTION_SCHEMA_VERSION,
    OBSERVATION_DIM,
    OBSERVATION_SCHEMA_VERSION,
    TERMINAL_CATEGORIES,
    ZERO_REWARD_CATEGORIES,
    Action,
    Observation,
    SchemaError,
    decode_action,
    flat_action,
    mask_action_for_learning,
    to_vector,
)
from rl_service.reward import trade_reward

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

    _repair_entropy(_model)

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


def _repair_entropy(model: SAC) -> None:
    """Restores the entropy target and coefficient a loaded checkpoint would otherwise carry.

    THE ROOT CAUSE OF §54.8's COLLAPSE, and why this runs on every load rather than once.

    SAC defaults target_entropy to -dim(action_space) and trains alpha to satisfy it. Over eight
    tanh-squashed dimensions that target demands a near-deterministic policy, so alpha was driven
    from 1.0 to 0.000919 — a thousandfold drop. With alpha at zero the entropy term vanishes from
    the actor loss, and nothing then penalises the policy for drifting to the tanh bounds. It did:
    every output pinned at ±1, returning an identical answer to every input, while /predict
    succeeded, /health stayed green, and the learner reported real gradient steps throughout.

    BOTH halves are required and neither works alone. A corrected target with the collapsed alpha
    restored from the checkpoint re-diverged within 31 gradient steps (measured during the
    rollback); resetting alpha under the old target is simply undone by the same signal that
    collapsed it the first time. A rollback to a healthy snapshot fixes neither, because the
    snapshot carries the same alpha.
    """
    try:
        import torch

        model.target_entropy = float(_cfg.serve.target_entropy)

        if _cfg.serve.reset_entropy_coef and getattr(model, "log_ent_coef", None) is not None:
            with torch.no_grad():
                model.log_ent_coef.fill_(0.0)  # log(1.0) — alpha back to SAC's own starting point
            logger.info("Entropy coefficient reset to 1.0 (docs/RL_V8_PLAN.md)")

        logger.info("Entropy target set to %s", model.target_entropy)
    except Exception:
        # A failure here must not stop the service from serving. It does mean the next collapse
        # would be unguarded, which is why /health reports entropy_coef: the number is visible even
        # when this repair could not run.
        logger.exception("entropy repair failed; serving anyway, watch /health's entropy_coef")


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
    # observation_compatible is the v8 addition, and the reason it matters is that its absence is
    # what let ten inputs vanish for weeks: to_vector used to reshape the observation to whatever
    # the loaded model wanted, so a mis-shaped model looked exactly like a working one
    # (docs/RL_V8_PLAN.md). model_loaded:true is NOT evidence the model is usable — §16.9 records a
    # wrongly-shaped model serving happily while the policy saw no market data at all.
    observation_compatible = (
        _model is not None and _model.observation_space.shape[0] == OBSERVATION_DIM
    )
    body = {
        "status": "ok",
        "model_loaded": _model is not None,
        "action_schema_version": ACTION_SCHEMA_VERSION,
        "observation_schema_version": OBSERVATION_SCHEMA_VERSION,
        "action_compatible": action_compatible,
        "observation_compatible": observation_compatible,
        "observation_dim": OBSERVATION_DIM,
        "model_observation_dim": (
            int(_model.observation_space.shape[0]) if _model is not None else None
        ),
        "learning_enabled": _learner is not None,
    }
    # The entropy coefficient, exposed so the next collapse is VISIBLE. SAC trains alpha to hit its
    # target entropy, and on this project's 9-dim action space the default target (-dim = -9)
    # demanded a near-deterministic policy: alpha fell 1.0 -> 0.000919, the entropy term vanished
    # from the actor loss, and nothing then penalised the policy for drifting to the tanh bounds —
    # all nine outputs pinned at ±1, returning an identical answer to every input (§54.8). Every
    # health signal stayed green throughout. A falling alpha here is the early warning that was
    # missing.
    if _model is not None:
        try:
            body["entropy_coef"] = float(_model.ent_coef_tensor.detach().cpu().item())
        except Exception:
            ent = getattr(_model, "ent_coef", None)
            body["entropy_coef"] = float(ent) if isinstance(ent, (int, float)) else None
        body["target_entropy"] = float(getattr(_model, "target_entropy", 0.0))
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

    # A model whose INPUT width differs cannot be served, and this check is new in v8.
    #
    # Before it, to_vector took the loaded model's own width and padded or truncated the observation
    # to fit — so the model dictated what the code sent, and a mis-shaped model was indistinguishable
    # from a working one. §16.9 records an 83-dim model against an 89-dim caller collapsing every
    # observation into the fixed tail (the policy saw no market data at all) while /health reported
    # model_loaded: true throughout. Refusing is the only honest answer.
    obs_dim = _model.observation_space.shape[0]
    if obs_dim != OBSERVATION_DIM:
        raise HTTPException(
            status_code=503,
            detail=(
                f"loaded model expects a {obs_dim}-dim observation; this service builds "
                f"{OBSERVATION_DIM} (observation_schema_version {OBSERVATION_SCHEMA_VERSION}). "
                "Train a model against the current schema — the width is never padded to fit."
            ),
        )

    try:
        obs_vec = to_vector(obs)
    except SchemaError as e:
        # 422 rather than 503: the observation is at fault, not the model. The caller should skip
        # the call rather than retry — Go validates before sending for exactly this reason, so a
        # 422 here means the two sides disagree about the schema.
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
        ps = obs.position_state
        breakdown = trade_reward(
            realized_pnl_usd=ps.realized_pnl_usd,
            risk_pct=ps.risk_pct,
            position_size_usd=ps.size_usd,
            leverage=ps.leverage,
            equity_usd=obs.account_equity_usd,
            peak_equity_usd=obs.account_peak_usd or obs.account_initial_usd,
            # An operator's manual close trains nothing: attributing a person's decision to the
            # policy would score it on something it never did (§15.12). The call still happens so
            # the pending decision resolves rather than leaking.
            zero_reward=obs.category in ZERO_REWARD_CATEGORIES,
        )
        matched = _learner.complete(obs.order_id, breakdown.total, obs_vec.reshape(-1))
        if not matched:
            # Expected after a restart, or for trades opened before learning was enabled — the
            # decision that produced them was never recorded, so there is nothing to score.
            logger.debug("terminal call for unknown order %s; nothing to score", obs.order_id)
        return

    # Zero the head that decided nothing before it reaches the replay buffer (docs/RL_V8_PLAN.md).
    # decode_action reads only the head matching this category, so training on the other one teaches
    # the network to move an output nothing reads — and an output rewarded without having caused
    # anything drifts to the tanh bound unopposed, which is the state §54.8 measured.
    _learner.record(obs.order_id, obs_vec.reshape(-1), mask_action_for_learning(raw, obs.category))
