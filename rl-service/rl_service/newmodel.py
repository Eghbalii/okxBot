"""Create a fresh, untrained SAC model matching the CURRENT observation/action schema.

Needed whenever the observation shape changes: a saved SAC locks its observation_space width, so a
v6 artifact (89 inputs) cannot serve a v7 caller (91). `to_vector` pads or truncates to whatever the
loaded model expects rather than failing, which is exactly how CLAUDE.md §16.9's silent-failure
incident happened — an 83-dim model served an 89-dim pipeline with the entire feature budget
truncated away, so every observation collapsed to the fixed tail and the policy returned an
identical answer to every input while /health still reported model_loaded: true.

Deliberately creates the model against the REAL vector width, derived from obs.py itself rather
than a hardcoded number, so this cannot drift from the schema it is meant to match.

Usage:
    python -m rl_service.newmodel --out models/sac_v7.zip [--feature-budget N]
"""
from __future__ import annotations

import argparse
import os

import gymnasium as gym
import numpy as np
from gymnasium import spaces
from stable_baselines3 import SAC

from rl_service.obs import (
    ACTION_DIM,
    ACTION_SCHEMA_VERSION,
    OBSERVATION_SCHEMA_VERSION,
    Observation,
    observation_tail,
)


def model_input_dim(feature_budget: int) -> int:
    """Total input width: the fixed tail plus however many timeframe features are budgeted.

    The tail is measured from obs.py directly — asking the code rather than restating its layout is
    what keeps this correct across schema changes.
    """
    probe = Observation(inst_id="PROBE", last_price=1.0)
    return int(observation_tail(probe).shape[0]) + feature_budget


class _ShapeEnv(gym.Env):
    """A do-nothing env that exists only to give SAC the right spaces to build against.

    Never stepped: SB3 requires an env to infer network shapes at construction, and this model is
    trained by feeding a replay buffer directly (see pretrain.py), never by env interaction.
    """

    def __init__(self, obs_dim: int, act_dim: int):
        self.observation_space = spaces.Box(-np.inf, np.inf, (obs_dim,), np.float32)
        self.action_space = spaces.Box(-1.0, 1.0, (act_dim,), np.float32)

    def reset(self, **kwargs):
        return np.zeros(self.observation_space.shape, np.float32), {}

    def step(self, action):
        return np.zeros(self.observation_space.shape, np.float32), 0.0, True, False, {}


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--out", required=True)
    p.add_argument(
        "--feature-budget",
        type=int,
        default=6,
        help="Timeframe-feature slots. The deployed v6 model ran 89 total against an 83-wide tail, "
        "i.e. a budget of 6; keeping it preserves how much price context reaches the model.",
    )
    p.add_argument("--buffer-size", type=int, default=100000)
    p.add_argument("--learning-starts", type=int, default=100)
    args = p.parse_args()

    obs_dim = model_input_dim(args.feature_budget)
    print(
        f"observation schema v{OBSERVATION_SCHEMA_VERSION}, action schema v{ACTION_SCHEMA_VERSION}"
    )
    print(f"building SAC with obs_dim={obs_dim} act_dim={ACTION_DIM}")

    model = SAC(
        "MlpPolicy",
        _ShapeEnv(obs_dim, ACTION_DIM),
        buffer_size=args.buffer_size,
        learning_starts=args.learning_starts,
        verbose=0,
    )

    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    model.save(args.out)
    print(f"saved fresh model to {args.out}")
    print("no replay buffer written: a new model starts with no experience by definition")


if __name__ == "__main__":
    main()
