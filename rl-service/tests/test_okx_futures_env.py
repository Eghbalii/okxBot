"""Sanity checks for OkxFuturesEnv — run with `python -m pytest rl-service/tests`."""
import numpy as np
import pandas as pd

from rl_service.data.features import add_features
from rl_service.env.okx_futures_env import OkxFuturesEnv


def _synthetic_df(n=500):
    rng = np.random.default_rng(0)
    price = 100 + np.cumsum(rng.normal(0, 0.5, size=n))
    df = pd.DataFrame(
        {
            "ts": np.arange(n),
            "open": price,
            "high": price + 0.5,
            "low": price - 0.5,
            "close": price,
            "vol": rng.uniform(10, 100, size=n),
        }
    )
    return add_features(df)


def test_env_reset_and_step_shapes():
    df = _synthetic_df()
    env = OkxFuturesEnv(df, window_size=16, max_position_notional_usd=1000.0)

    obs, info = env.reset()
    assert obs.shape == env.observation_space.shape

    action = np.array([0.5, 0.5], dtype=np.float32)
    obs, reward, terminated, truncated, info = env.step(action)

    assert obs.shape == env.observation_space.shape
    assert isinstance(reward, float)
    assert "equity" in info


def test_env_runs_full_episode_without_error():
    df = _synthetic_df()
    env = OkxFuturesEnv(df, window_size=16, max_position_notional_usd=1000.0)
    env.reset()

    rng = np.random.default_rng(1)
    for _ in range(len(df) - 20):
        action = rng.uniform([-1, 0], [1, 1]).astype(np.float32)
        _, _, terminated, truncated, _ = env.step(action)
        if terminated or truncated:
            break
