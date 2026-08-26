"""Small typed config loader for the rl-service (mirrors go-engine/internal/config)."""
from __future__ import annotations

import os
from dataclasses import dataclass, field

import yaml


@dataclass
class DataConfig:
    candles_csv: str = "data/BTC-USDT-SWAP_1H.csv"


@dataclass
class EnvConfig:
    window_size: int = 32
    # CLAUDE.md §15.4: raised from 5.0 to match go-engine's risk.max_leverage ceiling (100) — keep
    # both sides consistent so the historical/sanity-check env (§2) isn't training/testing against
    # a materially different leverage range than the live risk manager actually enforces.
    max_leverage: float = 100.0
    max_position_notional_usd: float = 1000.0
    taker_fee_rate: float = 0.0005
    funding_rate_per_8h: float = 0.0001
    initial_equity_usd: float = 10_000.0
    liquidation_maintenance_margin_pct: float = 0.5


@dataclass
class TrainConfig:
    algo: str = "PPO"
    total_timesteps: int = 500_000
    model_out: str = "models/ppo_okx_futures.zip"


@dataclass
class WarmStartConfig:
    """CLAUDE.md §15.8: warm-start replay training config — initialization only, not a
    backtest/evaluation. See rl_service/env/replay_env.py's module docstring."""

    inst_ids: list[str] = field(default_factory=lambda: ["BTC-USDT-SWAP"])
    bar: str = "1m"
    total_timesteps: int = 50_000
    model_out: str = "models/ppo_global.zip"
    initial_equity_usd: float = 10.0  # CLAUDE.md §15.6: matches the real per-token paper budget


@dataclass
class ServeConfig:
    model_path: str = "models/ppo_okx_futures.zip"
    host: str = "0.0.0.0"
    port: int = 8000


@dataclass
class Config:
    data: DataConfig = field(default_factory=DataConfig)
    env: EnvConfig = field(default_factory=EnvConfig)
    train: TrainConfig = field(default_factory=TrainConfig)
    warm_start: WarmStartConfig = field(default_factory=WarmStartConfig)
    serve: ServeConfig = field(default_factory=ServeConfig)


def load_config(path: str | None = None) -> Config:
    """Load config from a YAML file, falling back to CONFIG_PATH env var, then defaults."""
    path = path or os.environ.get("CONFIG_PATH")
    if not path or not os.path.exists(path):
        return Config()

    with open(path, "r", encoding="utf-8") as f:
        raw = yaml.safe_load(f) or {}

    return Config(
        data=DataConfig(**raw.get("data", {})),
        env=EnvConfig(**raw.get("env", {})),
        train=TrainConfig(**raw.get("train", {})),
        warm_start=WarmStartConfig(**raw.get("warm_start", {})),
        serve=ServeConfig(**raw.get("serve", {})),
    )
