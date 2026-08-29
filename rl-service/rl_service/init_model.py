"""Create a fresh, untrained SAC model artifact for the serving path (CLAUDE.md §15.11), skipping
the warm-start replay phase (§15.8) per explicit product decision (2026-08-29): live paper-trading
outcomes are the only training signal from the start, rather than initializing on replayed real
history first. This trades away warm-start's head start (avoiding weeks of near-random decisions at
this project's live trade volume, per §15.8) for simplicity — an accepted, explicit tradeoff, not an
oversight.

The observation/action space shapes are derived from the SAME rl_service.obs vectorization
production uses (a probe Observation run through observation_tail/observation_features, exactly how
ReplayEnv derives obs_dim) — never hand-computed — so the saved model's input width always matches
what /predict will actually send it, with no separate constant to keep in sync.

No .learn() call: SAC's constructor already initializes the policy network with random weights,
which is all a "fresh" model is. Continuous learning (config.serve.learning_enabled) takes over
from there against real production experience.

Usage:
    python -m rl_service.init_model [--config configs/config.yaml] [--out models/sac_global.zip]
"""
from __future__ import annotations

import argparse
import os

import gymnasium as gym
import numpy as np
from gymnasium import spaces
from stable_baselines3 import SAC

from rl_service.config import load_config
from rl_service.obs import (
    ACTION_DIM,
    Observation,
    PriceContext,
    TimeframeBlock,
    observation_features,
    observation_tail,
)


def _probe_obs_dim() -> int:
    """Builds one representative Observation and runs it through the real vectorization functions
    to get obs_dim — the same derivation ReplayEnv uses, never a hand-maintained constant that could
    drift from obs.py.

    MUST include a non-empty `timeframes` list: go-engine's buildObservation
    (internal/usecase/rl_sltp_adjust.go) always sends exactly one TimeframeBlock with a real
    PriceContext, and observation_features() reads 6 scalars from price_context alone (4 relative
    OHLC + 2 swing distances) even when the block's own `features` array is empty — which it always
    is in production, since buildObservation never populates TimeframeBlock.Features. Omitting
    timeframes entirely (as an earlier version of this probe did) understates obs_dim by exactly
    that 6, producing a model with zero feature budget: to_vector() would then silently discard
    every real call's price/signal features rather than feeding them to the policy at all."""
    probe = Observation(
        inst_id="BTC-USDT-SWAP",
        last_price=1.0,
        timeframes=[TimeframeBlock(bar="5m", price_context=PriceContext())],
    )
    return len(observation_tail(probe)) + len(observation_features(probe))


class _DummyEnv(gym.Env):
    """A do-nothing env that exists only to hand SAC valid observation/action spaces at
    construction time — go-engine's rlclient calls /predict directly, this process never steps a
    real env. Mirrors ReplayEnv's exact space shapes (CLAUDE.md §15.11) so the saved model is
    interchangeable with one warm-start would have produced."""

    def __init__(self, obs_dim: int):
        super().__init__()
        self.observation_space = spaces.Box(low=-np.inf, high=np.inf, shape=(obs_dim,), dtype=np.float32)
        low = np.array([-1.0, 0.0, -1.0, -1.0] + [0.0] * (ACTION_DIM - 4), dtype=np.float32)
        high = np.ones(ACTION_DIM, dtype=np.float32)
        self.action_space = spaces.Box(low=low, high=high, dtype=np.float32)

    def reset(self, *, seed=None, options=None):
        super().reset(seed=seed)
        return np.zeros(self.observation_space.shape, dtype=np.float32), {}

    def step(self, action):
        obs = np.zeros(self.observation_space.shape, dtype=np.float32)
        return obs, 0.0, True, False, {}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--config", default=None)
    parser.add_argument("--out", default=None, help="Overrides config.serve.model_path if set.")
    args = parser.parse_args()

    cfg = load_config(args.config)
    model_out = args.out or cfg.serve.model_path

    obs_dim = _probe_obs_dim()
    env = _DummyEnv(obs_dim)

    model = SAC("MlpPolicy", env, buffer_size=cfg.serve.buffer_size, verbose=1)

    os.makedirs(os.path.dirname(model_out) or ".", exist_ok=True)
    model.save(model_out)
    print(
        f"Saved untrained SAC model to {model_out} (obs_dim={obs_dim}, action_dim={ACTION_DIM}). "
        "Random initial weights — no warm-start, no .learn() call. Enable serve.learning_enabled "
        "so it starts learning from live paper-trading outcomes (CLAUDE.md §15.11)."
    )


if __name__ == "__main__":
    main()
