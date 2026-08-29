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

from stable_baselines3 import PPO, SAC
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
            taker_fee_rate=cfg.env.taker_fee_rate,
            initial_equity_usd=cfg.warm_start.initial_equity_usd,
            max_position_pct=cfg.warm_start.max_position_pct,
            max_total_exposure_pct=cfg.warm_start.max_total_exposure_pct,
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

    if args.warm_start:
        # SAC, not PPO (CLAUDE.md §15.11): the deployed model has to keep learning from live
        # outcomes, and PPO's on-policy batches cannot be filled at this project's trade volume.
        # Warm-start therefore has to produce an artifact the serving path can continue training.
        model = SAC(
            "MlpPolicy",
            vec_env,
            buffer_size=cfg.serve.buffer_size,
            verbose=1,
        )
    else:
        # The legacy historical-CSV sanity-check path (CLAUDE.md §2) stays on PPO — it exists to
        # validate env/reward changes quickly, never to produce a deployed model.
        model = PPO(
            "MlpPolicy",
            vec_env,
            verbose=1,
        )

    model.learn(total_timesteps=total_timesteps)

    os.makedirs(os.path.dirname(model_out) or ".", exist_ok=True)
    model.save(model_out)
    print(f"Saved trained model to {model_out}")

    if args.warm_start:
        # Save the buffer too, so the serving path resumes with the warm-start experience rather
        # than an empty buffer (CLAUDE.md §15.11) — weights alone would discard it.
        buffer_out = cfg.serve.buffer_path
        os.makedirs(os.path.dirname(buffer_out) or ".", exist_ok=True)
        model.save_replay_buffer(buffer_out)
        print(f"Saved replay buffer to {buffer_out}")


if __name__ == "__main__":
    main()
