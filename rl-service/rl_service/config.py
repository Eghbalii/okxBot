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
    # The timeframe ReplayEnv steps through. Warm-start is deliberately SINGLE-bar even though the
    # deployed agent decides on 5m/15m/1H (CLAUDE.md §9): the replay env advances one candle per
    # step, and interleaving timeframes with different step durations in one pooled sequence would
    # make "one step" mean different amounts of elapsed time depending on which bar it came from —
    # the reward signal would be inconsistent across steps. Warm-start only has to get the policy
    # off random initialization (§15.8); the multi-timeframe distribution is learned from live
    # paper-trading, which is where the real training signal comes from anyway.
    # 15m is the middle of the three decision timeframes — enough history to accumulate quickly,
    # slow enough that the price action resembles the higher bars too.
    bar: str = "15m"
    total_timesteps: int = 50_000
    model_out: str = "models/sac_global.zip"
    # CLAUDE.md §15.6 (revised 2026-08-28): ONE shared account across every pooled token, matching
    # go-engine's account.initial_usd — not a per-token sub-budget.
    initial_equity_usd: float = 100.0
    # Mirror account.max_position_pct / account.max_total_exposure_pct on the Go side, so the policy
    # trains under the same caps production enforces.
    max_position_pct: float = 0.25
    max_total_exposure_pct: float = 0.60


@dataclass
class ServeConfig:
    # The global agent's artifact (CLAUDE.md §15.1), matching warm_start.model_out — NOT the legacy
    # okx_futures_env sanity-check env's output. A default pointing at the legacy path meant a
    # deployment without an explicit config.yaml would look for a file --warm-start never writes.
    model_path: str = "models/sac_global.zip"
    host: str = "0.0.0.0"
    port: int = 8000

    # --- continuous learning (CLAUDE.md §15.11) ---
    # OFF by default. When enabled the service keeps learning from live outcomes instead of serving
    # frozen weights: terminal (closed_*) calls deliver realized PnL as reward and trigger a
    # gradient step. Freeze this for real money — a bad update would otherwise reach live trading
    # with no review gate, which is exactly what the paper -> demo -> real progression guards.
    learning_enabled: bool = False
    # Replay buffer capacity. 100k is far more than this project can fill (tens of trades/day), and
    # SB3's 1M default would reserve ~788 MB for capacity that will never be used.
    buffer_size: int = 100_000
    # No gradient steps until the buffer holds at least this many experiences — updating from a
    # nearly-empty buffer just overfits the first few trades.
    learning_starts: int = 100
    # Gradient steps per closed trade. Off-policy means each experience is reused many times, so
    # this can be >1 even though experiences arrive slowly.
    #
    # ONE, not four, as of v8. Four steps at batch 256 per closed trade replayed each experience
    # ~6 times in two hours, out of a buffer that is all one market regime and mostly losses —
    # aggressive enough to overfit a trickle, which is the opposite of what off-policy reuse is for
    # at this data volume (docs/RL_V8_PLAN.md).
    gradient_steps: int = 1
    # Snapshot cadence, in closed trades. A snapshot writes BOTH weights and the replay buffer:
    # weights alone would silently discard every collected experience on restart.
    #
    # Raised 25 -> 100 with v8. At 25 the snapshot hook OVERWROTE a deliberately restored backup
    # after 31 gradient steps, before anyone could see what had happened (§54.8's rollback) — a
    # cadence that destroys the evidence of the failure it is supposed to help diagnose.
    snapshot_every: int = 100

    # --- entropy (docs/RL_V8_PLAN.md) ---
    #
    # SAC defaults target_entropy to -dim(action_space), which is -8 here. Spread over eight
    # tanh-squashed dimensions that demands about -1.0 per dimension — a near-deterministic policy.
    # SAC then trains alpha to satisfy it, and alpha fell from 1.0 to 0.000919: with alpha at zero
    # the entropy term vanishes from the actor loss, so nothing penalises the policy for drifting
    # to the tanh bounds, and it did — all outputs pinned at ±1, returning an identical answer to
    # every input (§54.8).
    #
    # -4.5 asks for roughly half that much determinism, leaving real exploration in a policy that
    # has to keep learning from a trickle of live outcomes.
    target_entropy: float = -4.5
    # Reset alpha to 1.0 when loading a checkpoint. BOTH halves are needed and neither works alone:
    # a corrected target with the collapsed alpha restored from the checkpoint re-diverges (measured:
    # within 31 gradient steps), and resetting alpha under the old target is simply undone by the
    # same training signal that collapsed it the first time.
    reset_entropy_coef: bool = True
    # Where the replay buffer is snapshotted. Sits beside model_path by default.
    buffer_path: str = "models/sac_global_buffer.pkl"
    # Overrides the learning_rate baked into the loaded checkpoint at startup (SAC.load's own
    # learning_rate kwarg) — SAC.load otherwise restores whatever rate the checkpoint was saved
    # with, so this is the only way to change it without retraining from scratch.
    #
    # Held at SAC's default 0.0003. A 5x raise (0.0015) was tried on 2026-09-07 and REVERTED the
    # same hour: within 6 closed trades (24 gradient steps) the actor's size dimension went from
    # +0.807 to negative, which decode_action clips to zero — so size_pct became 0.0 on every call
    # and every open silently fell back to fixed sizing ("model action not sizable" in the
    # paper-trader log). Confirmed by replaying one identical observation through the on-disk
    # pre-raise checkpoint (+0.807) and the live post-raise model (negative) side by side.
    # The thin replay buffer (~4.4k real experiences) is what makes the policy this easy to move;
    # raising this again needs a way to catch the collapse automatically, not just a smaller step.
    learning_rate: float = 0.0003


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
