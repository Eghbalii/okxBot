"""Sanity checks for ReplayEnv (CLAUDE.md §15.8) — run with `python -m pytest rl-service/tests`.

Uses synthetic CandleRow data (no real Postgres) so these run in any environment; the Postgres
read path itself (rl_service/data/postgres.py) is thin enough to not need its own mock — it's
exercised for real whenever build_replay_env is actually run against a live database.
"""
from __future__ import annotations

from datetime import datetime, timedelta

import numpy as np
from stable_baselines3 import PPO
from stable_baselines3.common.monitor import Monitor

from rl_service.data.postgres import CandleRow
from rl_service.env.replay_env import ReplayEnv, _TokenSeries, _rows_to_feature_dicts


def _make_candles(n, start_price=100.0, inst_id="BTC-USDT-SWAP", bar="1m", seed=0):
    rng = np.random.default_rng(seed)
    rows = []
    price = start_price
    ts0 = datetime(2026, 1, 1)
    for i in range(n):
        price *= 1 + rng.uniform(-0.005, 0.005)
        rows.append(
            CandleRow(
                inst_id=inst_id, bar=bar, ts=ts0 + timedelta(minutes=i),
                open=price, high=price * 1.001, low=price * 0.999, close=price, volume=10.0,
            )
        )
    return rows


def _two_token_series():
    rows1 = _make_candles(200, inst_id="BTC-USDT-SWAP", seed=0)
    rows2 = _make_candles(200, inst_id="XAU-USD-SWAP", start_price=2000.0, seed=1)
    return [
        _TokenSeries(inst_id="BTC-USDT-SWAP", bars={"1m": _rows_to_feature_dicts(rows1)}),
        _TokenSeries(inst_id="XAU-USD-SWAP", bars={"1m": _rows_to_feature_dicts(rows2)}),
    ]


def test_reset_and_step_shapes():
    series = _two_token_series()
    env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=10_000.0)

    obs, info = env.reset()
    assert obs.shape == env.observation_space.shape

    action = env.action_space.sample()
    obs2, reward, terminated, truncated, info = env.step(action)
    assert obs2.shape == env.observation_space.shape
    assert isinstance(reward, float)
    assert "inst_id" in info


def test_token_identity_one_hot_present_in_tail():
    # The token-identity one-hot (CLAUDE.md §15.1) must differ for a step on token[0] vs. token[1]
    # even holding position/leverage/equity constant — otherwise the shared policy has no way to
    # condition its behavior per token.
    series = _two_token_series()
    env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=10_000.0)

    obs_token0, _ = env.reset()
    env._token_idx = 1
    env._step_idx = 0
    obs_token1 = env._current_obs_vec()

    assert not np.array_equal(obs_token0, obs_token1)


def test_multi_token_rollout_advances_through_all_tokens():
    series = _two_token_series()
    env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=10_000.0)
    env.reset()

    seen_insts = set()
    for _ in range(400):
        action = env.action_space.sample()
        obs, reward, terminated, truncated, info = env.step(action)
        seen_insts.add(info["inst_id"])
        if terminated or truncated:
            break

    assert "BTC-USDT-SWAP" in seen_insts
    assert "XAU-USD-SWAP" in seen_insts


def test_ppo_trains_against_replay_env_without_error(tmp_path):
    series = _two_token_series()
    env = Monitor(
        ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=10_000.0)
    )
    model = PPO("MlpPolicy", env, n_steps=32, batch_size=16, verbose=0)
    model.learn(total_timesteps=64)

    model_path = tmp_path / "ppo_replay_test.zip"
    model.save(str(model_path))
    loaded = PPO.load(str(model_path))

    obs, _ = env.reset()
    action, _ = loaded.predict(obs, deterministic=True)
    assert action.shape == (2,)


def test_raises_when_no_candle_history_for_bar():
    series = [_TokenSeries(inst_id="BTC-USDT-SWAP", bars={})]
    try:
        ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP"])
        assert False, "expected ValueError for empty candle history"
    except ValueError:
        pass
