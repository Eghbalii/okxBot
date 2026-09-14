"""Create a fresh, untrained SAC model artifact for the serving path (CLAUDE.md §15.11).

The observation width comes from `obs.OBSERVATION_DIM` — a CONSTANT, not a probe.

That is the v8 change and it matters. The previous version built a representative Observation and
measured the vector it produced, on the reasoning that a derived width can never drift from the
code. It could and did: the probe had to guess what production actually sends, its own docstring
recorded discovering that an earlier probe understated the width by exactly six (producing a model
with zero feature budget), and every model this script wrote inherited whatever the probe happened
to model that day. Meanwhile `to_vector` reshaped real observations to fit the result, so a wrong
width was never visible — §16.9 records an 83-dim model serving an 89-dim caller for weeks while
/health reported model_loaded: true.

v8 inverts the relationship: the code declares the width, a test pins it against a real build, and
both this script and /predict refuse anything else.

Entropy is configured here rather than left at SAC's default, because the default is what collapsed
the last model (§54.8): target_entropy defaults to -dim(action_space), which over eight
tanh-squashed dimensions demands a near-deterministic policy, so alpha was trained from 1.0 down to
0.000919 and the policy drifted to the tanh bounds unopposed.

No .learn() call: SAC's constructor initializes the policy with random weights, which is all a
"fresh" model is. Training happens in the backtest warm start and then continuously from live
outcomes.

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
from rl_service.obs import ACTION_DIM, OBSERVATION_DIM


class _DummyEnv(gym.Env):
    """A do-nothing env that exists only to hand SAC valid spaces at construction time — the
    trading engine calls /predict directly, and this process never steps a real env."""

    def __init__(self, obs_dim: int):
        super().__init__()
        self.observation_space = spaces.Box(
            low=-np.inf, high=np.inf, shape=(obs_dim,), dtype=np.float32
        )
        # sl and tp offsets are signed (a stop can sit either side of price); size, leverage and the
        # two decision heads are not. The open head is the exception among the heads: its SIGN is
        # the decision, so it spans [-1, 1] like the offsets.
        low = np.array([-1.0, -1.0, 0.0, 0.0, -1.0, 0.0, 0.0, 0.0], dtype=np.float32)
        high = np.ones(ACTION_DIM, dtype=np.float32)
        if low.shape[0] != ACTION_DIM:
            raise ValueError(
                f"action bounds describe {low.shape[0]} dimensions, ACTION_DIM is {ACTION_DIM} — "
                "the two must be edited together or the model's outputs land in the wrong ranges"
            )
        self.action_space = spaces.Box(low=low, high=high, dtype=np.float32)

    def reset(self, *, seed=None, options=None):
        super().reset(seed=seed)
        return np.zeros(self.observation_space.shape, dtype=np.float32), {}

    def step(self, action):
        return np.zeros(self.observation_space.shape, dtype=np.float32), 0.0, True, False, {}


def main() -> None:
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument("--config", default=None)
    parser.add_argument("--out", default=None, help="Overrides config.serve.model_path if set.")
    args = parser.parse_args()

    cfg = load_config(args.config)
    model_out = args.out or cfg.serve.model_path

    model = SAC(
        "MlpPolicy",
        _DummyEnv(OBSERVATION_DIM),
        buffer_size=cfg.serve.buffer_size,
        gradient_steps=cfg.serve.gradient_steps,
        # See the module docstring: SAC's own default is what collapsed the previous model.
        target_entropy=cfg.serve.target_entropy,
        verbose=1,
    )

    os.makedirs(os.path.dirname(model_out) or ".", exist_ok=True)
    model.save(model_out)
    print(
        f"Saved untrained SAC model to {model_out} "
        f"(obs_dim={OBSERVATION_DIM}, action_dim={ACTION_DIM}, "
        f"target_entropy={cfg.serve.target_entropy}). Random initial weights."
    )


if __name__ == "__main__":
    main()
