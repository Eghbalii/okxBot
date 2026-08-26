"""Train the global RL agent (CLAUDE.md §15).

Two distinct modes — see CLAUDE.md §15.8 for the full design and why they're not the same thing:

  --warm-start  Initialization-only training against the REPLAY env (rl_service/env/replay_env.py):
                on-policy PPO rollouts (the model's own current decisions) against REAL historical
                market conditions already persisted by go-engine in Postgres. This is NOT
                backtesting/evaluation — nothing here scores the model, its output is only ever
                used as a starting point for continued live learning, never as a performance claim.

  (default)     Legacy historical-CSV mode via okx_futures_env.py, kept as the original
                optional/off-by-default dev sanity-check tool (CLAUDE.md §2) — not the deployed
                model's training path.

Usage:
    python -m rl_service.train --warm-start --config configs/config.yaml
    python -m rl_service.train --config configs/config.yaml   # legacy CSV sanity-check env
"""
from __future__ import annotations

import argparse
import os

from stable_baselines3 import PPO
from stable_baselines3.common.env_util import make_vec_env
from stable_baselines3.common.monitor import Monitor

from rl_service.config import load_config
from rl_service.data.loader import load_features
from rl_service.data.postgres import connect
from rl_service.env.okx_futures_env import OkxFuturesEnv
from rl_service.env.replay_env import ReplayEnv, load_token_series


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


def make_warm_start_env(cfg, dsn: str | None = None):
    conn = connect(dsn)
    try:
        series = [load_token_series(conn, inst_id, [cfg.warm_start.bar]) for inst_id in cfg.warm_start.inst_ids]
    finally:
        conn.close()

    def _init():
        env = ReplayEnv(
            series,
            bar=cfg.warm_start.bar,
            active_tokens=cfg.warm_start.inst_ids,
            max_leverage=cfg.env.max_leverage,
            max_position_notional_usd=cfg.env.max_position_notional_usd,
            taker_fee_rate=cfg.env.taker_fee_rate,
            initial_equity_usd=cfg.warm_start.initial_equity_usd,
        )
        return Monitor(env)

    return _init


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--config", default=None)
    parser.add_argument(
        "--warm-start",
        action="store_true",
        help="Train against the replay env (real historical market data, model's own on-policy "
        "decisions) instead of the legacy historical-CSV sanity-check env. CLAUDE.md §15.8.",
    )
    args = parser.parse_args()

    cfg = load_config(args.config)

    if args.warm_start:
        vec_env = make_vec_env(make_warm_start_env(cfg), n_envs=1)
        total_timesteps = cfg.warm_start.total_timesteps
        model_out = cfg.warm_start.model_out
        print(
            f"Warm-start training (CLAUDE.md §15.8): {cfg.warm_start.inst_ids} @ {cfg.warm_start.bar}, "
            f"{total_timesteps} timesteps. This is initialization only, not a performance claim — "
            "the deployed model's real training signal comes from live paper-trading (CLAUDE.md §2)."
        )
    else:
        vec_env = make_vec_env(make_env(cfg), n_envs=1)
        total_timesteps = cfg.train.total_timesteps
        model_out = cfg.train.model_out

    model = PPO(
        "MlpPolicy",
        vec_env,
        verbose=1,
        tensorboard_log="data/tensorboard",
    )
    model.learn(total_timesteps=total_timesteps)

    os.makedirs(os.path.dirname(model_out), exist_ok=True)
    model.save(model_out)
    print(f"Saved trained model to {model_out}")


if __name__ == "__main__":
    main()
