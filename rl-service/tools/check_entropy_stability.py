"""Diagnostic: trains a fresh SAC model on a warm-start dataset in stages, logging alpha/entropy
at each stage, to check whether the collapse from CLAUDE.md §54.8 recurs
(target_entropy=-8 default -> alpha 1.0 -> 0.0009 in 66 steps).

Run whenever the warm-start dataset or entropy config changes, since both feed the same failure
mode: a bad observation/reward distribution or a wrong target_entropy can each independently
drive alpha to collapse. Seeded explicitly (unlike warmstart.py/init_model.py, which have no seed
control at all) so one run's trajectory is not conflated with seed variance — docs/RL_V8_PLAN.md's
"Session 2026-09-15" section records a staged curve that wasn't reproducible for exactly this
reason.

Verified 2026-09-16 across seeds 42/1/100 on the v8 dataset with target_entropy=-4.5: all three
converged to alpha in [0.056, 0.065] at 19,100 cumulative steps, a smooth decline with no collapse
— nowhere near §54.8's 0.0009. Re-run this after any change to the observation schema, reward
function, or dataset before trusting that result still holds.

Usage:
    python -m tools.check_entropy_stability --data data/warmstart_v8.jsonl --seed 42
    python -m tools.check_entropy_stability --data data/warmstart_v8.jsonl --seed 1 --stages 1000,5000,10000,19100
"""
from __future__ import annotations

import argparse
import sys

import gymnasium as gym
import numpy as np
import torch
from gymnasium import spaces
from stable_baselines3 import SAC
from stable_baselines3.common.logger import configure

sys.path.insert(0, ".")
from rl_service.obs import ACTION_DIM, OBSERVATION_DIM, to_vector  # noqa: E402
from rl_service.warmstart import action_for, read_samples  # noqa: E402


class _DummyEnv(gym.Env):
    """Hands SAC valid spaces at construction time; never stepped for real."""

    def __init__(self, obs_dim: int):
        super().__init__()
        self.observation_space = spaces.Box(low=-np.inf, high=np.inf, shape=(obs_dim,), dtype=np.float32)
        low = np.array([-1.0, -1.0, 0.0, 0.0, -1.0, 0.0, 0.0, 0.0], dtype=np.float32)
        high = np.ones(ACTION_DIM, dtype=np.float32)
        self.action_space = spaces.Box(low=low, high=high, dtype=np.float32)

    def reset(self, *, seed=None, options=None):
        super().reset(seed=seed)
        return np.zeros(self.observation_space.shape, dtype=np.float32), {}

    def step(self, action):
        return np.zeros(self.observation_space.shape, dtype=np.float32), 0.0, True, False, {}


def report(model: SAC, cum_steps: int) -> None:
    alpha = torch.exp(model.log_ent_coef).item()
    log_ent = model.log_ent_coef.item()
    logs = model.logger.name_to_value
    actor_loss = logs.get("train/actor_loss", float("nan"))
    critic_loss = logs.get("train/critic_loss", float("nan"))
    print(f"{cum_steps:>18}{alpha:>10.5f}{log_ent:>15.5f}{actor_loss:>14.4f}{critic_loss:>14.4f}")


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--data", required=True, help="warmstart.jsonl from cmd/backtest")
    p.add_argument("--seed", type=int, default=42)
    p.add_argument("--target-entropy", type=float, default=-4.5)
    p.add_argument(
        "--stages",
        default="1000,5000,10000,19100",
        help="comma-separated CUMULATIVE step targets to report alpha at",
    )
    p.add_argument("--out", default="", help="where to save the trained model (default: don't save)")
    args = p.parse_args()

    cumulative_targets = [int(x) for x in args.stages.split(",")]
    stage_sizes = []
    prev = 0
    for target in cumulative_targets:
        stage_sizes.append(target - prev)
        prev = target

    np.random.seed(args.seed)
    torch.manual_seed(args.seed)

    print(f"seed={args.seed}")
    model = SAC(
        "MlpPolicy",
        _DummyEnv(OBSERVATION_DIM),
        buffer_size=100_000,
        gradient_steps=1,
        target_entropy=args.target_entropy,
        seed=args.seed,
        verbose=0,
    )
    model.set_logger(configure(folder=None, format_strings=[]))

    print(f"target_entropy={model.target_entropy}")
    print("loading dataset...")

    n = 0
    rewards: list[float] = []
    for s in read_samples(args.data):
        obs_vec = to_vector(s.observation)
        next_vec = to_vector(s.terminal)
        model.replay_buffer.add(
            obs=obs_vec,
            next_obs=next_vec,
            action=action_for(s).reshape(1, -1),
            reward=np.array([s.reward], dtype=np.float32),
            done=np.array([True]),
            infos=[{}],
        )
        rewards.append(s.reward)
        n += 1

    if n == 0:
        print("ERROR: dataset produced no samples", file=sys.stderr)
        raise SystemExit(1)

    arr = np.asarray(rewards, dtype=np.float64)
    print(f"loaded {n} samples: reward mean={arr.mean():+.4f} positive={100 * (arr > 0).mean():.1f}%")

    print()
    print(f"{'cumulative_steps':>18}{'alpha':>10}{'log_ent_coef':>15}{'actor_loss':>14}{'critic_loss':>14}")
    report(model, 0)

    cum = 0
    for stage in stage_sizes:
        model.train(gradient_steps=stage, batch_size=256)
        cum += stage
        report(model, cum)

    if args.out:
        model.save(args.out)
        print(f"\nsaved {args.out}")


if __name__ == "__main__":
    main()
