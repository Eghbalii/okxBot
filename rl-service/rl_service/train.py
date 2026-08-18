"""Train a PPO agent on the OKX futures trading environment using historical candle data.

Usage:
    python -m rl_service.train --config configs/config.yaml
"""
from __future__ import annotations

import argparse
import os

from stable_baselines3 import PPO
from stable_baselines3.common.env_util import make_vec_env
from stable_baselines3.common.monitor import Monitor

from rl_service.config import load_config
from rl_service.data.loader import load_features
from rl_service.env.okx_futures_env import OkxFuturesEnv


def make_env(cfg):
    df = load_features(cfg.data.candles_csv)

    def _init():
        env = OkxFuturesEnv(
            df,
            window_size=cfg.env.window_size,
            max_leverage=cfg.env.max_leverage,
            max_position_notional_usd=cfg.env.max_position_notional_usd,
            taker_fee_rate=cfg.env.taker_fee_rate,
            funding_rate_per_8h=cfg.env.funding_rate_per_8h,
            initial_equity_usd=cfg.env.initial_equity_usd,
            liquidation_maintenance_margin_pct=cfg.env.liquidation_maintenance_margin_pct,
        )
        return Monitor(env)

    return _init


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", default=None)
    args = parser.parse_args()

    cfg = load_config(args.config)

    vec_env = make_vec_env(make_env(cfg), n_envs=1)

    model = PPO(
        "MlpPolicy",
        vec_env,
        verbose=1,
        tensorboard_log="data/tensorboard",
    )
    model.learn(total_timesteps=cfg.train.total_timesteps)

    os.makedirs(os.path.dirname(cfg.train.model_out), exist_ok=True)
    model.save(cfg.train.model_out)
    print(f"Saved trained model to {cfg.train.model_out}")


if __name__ == "__main__":
    main()
