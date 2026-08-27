"""Warm-start replay environment (CLAUDE.md §15.8) — plays back REAL market conditions already
persisted by go-engine (Postgres `candles`, plus any strategy signals logged in `paper_orders.
features_json` at decision time) so PPO can do its normal on-policy rollout collection against
real data before live paper-trading has produced enough experience on its own.

This is explicitly NOT backtesting/evaluation: nothing here scores or validates the model against
history, and its output is never treated as a performance claim. Only the ACTIONS taken during
these rollouts are the policy's own current choices — the market conditions are historical, the
decisions are fresh, exactly like a live rollout would be. See CLAUDE.md §15.8 for the full
warm-start-then-continued-live-learning design and why this doesn't reopen the original
no-backtesting decision (§2).

Multiple tokens are pooled into ONE sequence (CLAUDE.md §15.1's global agent, token identity as an
observation field) — the env steps through each active token's candle history in turn.
"""
from __future__ import annotations

from dataclasses import dataclass, field

import gymnasium as gym
import numpy as np
from gymnasium import spaces

from rl_service.data.features import FEATURE_COLUMNS, add_features
from rl_service.data.postgres import CandleRow, PaperOrderRow, connect, fetch_candles, fetch_paper_orders
from rl_service.obs import (
    Observation,
    PriceContext,
    StrategySignal,
    TimeframeBlock,
    observation_features,
    observation_tail,
)

SWING_WINDOW = 20
PRICE_CONTEXT_WINDOW = 10


@dataclass
class _TokenSeries:
    inst_id: str
    bars: dict[str, list[dict]]  # bar -> list of {ts, open, high, low, close, volume, *features}
    # Decision-time strategy signals actually logged for this token, keyed by (bar, ts) so a step
    # at a bar/timestamp where a paper order was opened gets the real signal instead of an empty
    # one (CLAUDE.md §15.8's "read from paper_orders.features_json where already logged" decision).
    logged_signals: dict[tuple[str, int], list[StrategySignal]] = field(default_factory=dict)


def _rows_to_feature_dicts(rows: list[CandleRow]) -> list[dict]:
    import pandas as pd

    if not rows:
        return []
    df = pd.DataFrame(
        {
            "ts": [r.ts for r in rows],
            "open": [r.open for r in rows],
            "high": [r.high for r in rows],
            "low": [r.low for r in rows],
            "close": [r.close for r in rows],
            "vol": [r.volume for r in rows],
        }
    )
    df = add_features(df)  # drops the leading rows that can't fill rolling windows yet
    return df.to_dict("records")


def _extract_logged_signals(order: PaperOrderRow) -> tuple[str, StrategySignal] | None:
    """Pulls the strategy signal this order's decision-time observation actually recorded, if any
    (features_json is None for orders opened before the schema-v3 persistence change)."""
    fj = order.features_json
    if not fj or not fj.get("timeframes"):
        return None
    tf = fj["timeframes"][0]  # PaperTrader logs exactly one timeframe block per decision
    sigs = tf.get("strategy_signals") or []
    if not sigs:
        return None
    s = sigs[0]
    return tf.get("bar", ""), StrategySignal(
        strategy_id=s.get("strategy_id", 0),
        side=s.get("side", ""),
        confidence=float(s.get("confidence", 0) or 0),
        sl_pct=float(s.get("sl_pct", 0) or 0),
        tp_pct=float(s.get("tp_pct", 0) or 0),
    )


def load_token_series(conn, inst_id: str, bars: list[str]) -> _TokenSeries:
    series = _TokenSeries(inst_id=inst_id, bars={})
    for bar in bars:
        rows = fetch_candles(conn, inst_id, bar)
        series.bars[bar] = _rows_to_feature_dicts(rows)

    for order in fetch_paper_orders(conn, inst_id=inst_id, variant="baseline"):
        extracted = _extract_logged_signals(order)
        if extracted is None:
            continue
        bar, sig = extracted
        ts_ms = int(order.opened_at.timestamp() * 1000)
        series.logged_signals.setdefault((bar, ts_ms), []).append(sig)

    return series


def _price_context(closes: list[float]) -> PriceContext:
    if len(closes) < 2:
        return PriceContext()
    window = closes[-(PRICE_CONTEXT_WINDOW + 1) :]
    pct_changes = [
        (window[i] - window[i - 1]) / window[i - 1] for i in range(1, len(window)) if window[i - 1] != 0
    ]
    swing = closes[-SWING_WINDOW:]
    last = closes[-1]
    dist_high = (max(swing) - last) / last if last else 0.0
    dist_low = (min(swing) - last) / last if last else 0.0
    return PriceContext(close_pct_changes=pct_changes, dist_to_swing_high_pct=dist_high, dist_to_swing_low_pct=dist_low)


class ReplayEnv(gym.Env):
    """Steps through pooled multi-token real candle history, building the same Observation shape
    rlclient sends live (CLAUDE.md §15.3), one step per bar close. The agent's action determines
    simulated exposure/leverage/PnL against the REAL price sequence — only the decision is the
    policy's own, the market data is historical. See module docstring for why this isn't
    backtesting."""

    metadata = {"render_modes": []}

    def __init__(
        self,
        token_series: list[_TokenSeries],
        bar: str,
        active_tokens: list[str],
        max_leverage: float = 100.0,
        max_position_notional_usd: float = 1000.0,
        taker_fee_rate: float = 0.0005,
        initial_equity_usd: float = 10.0,
    ):
        super().__init__()
        self.token_series = [t for t in token_series if t.bars.get(bar)]
        if not self.token_series:
            raise ValueError(f"no candle history available for bar {bar!r} across the given tokens")
        self.bar = bar
        self.active_tokens = active_tokens
        self.max_leverage = max_leverage
        self.max_position_notional_usd = max_position_notional_usd
        self.taker_fee_rate = taker_fee_rate
        self.initial_equity_usd = initial_equity_usd

        self._reset_state()

        # obs_dim is only known once we've built one sample observation.
        probe_obs = self._build_observation(self.token_series[0], 0)
        tail_len = len(observation_tail(probe_obs))
        feat_len = len(observation_features(probe_obs))
        self.obs_dim = tail_len + feat_len
        self.observation_space = spaces.Box(low=-np.inf, high=np.inf, shape=(self.obs_dim,), dtype=np.float32)
        self.action_space = spaces.Box(low=np.array([-1.0, 0.0]), high=np.array([1.0, 1.0]), dtype=np.float32)

    def _reset_state(self):
        self._token_idx = 0
        self._step_idx = 0
        self.equity = self.initial_equity_usd
        self.position_notional = 0.0
        self.leverage = 1.0
        self.entry_price = None

    def reset(self, *, seed=None, options=None):
        super().reset(seed=seed)
        self._reset_state()
        return self._current_obs_vec(), {}

    def _current_series(self) -> _TokenSeries:
        return self.token_series[self._token_idx % len(self.token_series)]

    def _build_observation(self, series: _TokenSeries, step_idx: int) -> Observation:
        rows = series.bars[self.bar]
        row = rows[min(step_idx, len(rows) - 1)]
        closes = [r["close"] for r in rows[: step_idx + 1]]

        sigs = series.logged_signals.get((self.bar, int(row["ts"].timestamp() * 1000)), [])
        tb = TimeframeBlock(
            bar=self.bar,
            strategy_signals=sigs,
            features=[float(row.get(c, 0.0) or 0.0) for c in FEATURE_COLUMNS],
            price_context=_price_context(closes),
        )
        return Observation(
            inst_id=series.inst_id,
            active_tokens=self.active_tokens,
            last_price=float(row["close"]),
            timeframes=[tb],
            position=1.0 if self.position_notional > 0 else (-1.0 if self.position_notional < 0 else 0.0),
            current_leverage=self.leverage,
            token_equity_usd=self.equity,
        )

    def _current_obs_vec(self) -> np.ndarray:
        series = self._current_series()
        obs = self._build_observation(series, self._step_idx)
        tail = observation_tail(obs)
        features = observation_features(obs)
        padded_len = self.obs_dim - len(tail)
        if len(features) < padded_len:
            features = np.pad(features, (padded_len - len(features), 0))
        elif len(features) > padded_len:
            features = features[-padded_len:]
        return np.concatenate([features, tail]).astype(np.float32)

    def step(self, action: np.ndarray):
        series = self._current_series()
        rows = series.bars[self.bar]
        row = rows[min(self._step_idx, len(rows) - 1)]
        price = float(row["close"])

        target_exposure = float(np.clip(action[0], -1.0, 1.0))
        leverage_frac = float(np.clip(action[1], 0.0, 1.0))
        next_leverage = 1.0 + leverage_frac * (self.max_leverage - 1.0)
        target_notional = target_exposure * self.max_position_notional_usd

        realized_pnl = 0.0
        if self.entry_price is not None and self.position_notional != 0:
            direction = 1.0 if self.position_notional > 0 else -1.0
            realized_pnl = direction * (price / self.entry_price - 1.0) * abs(self.position_notional)

        traded_notional = abs(target_notional - self.position_notional)
        fee_cost = traded_notional * self.taker_fee_rate

        self.equity += realized_pnl - fee_cost
        self.position_notional = target_notional
        self.leverage = next_leverage
        self.entry_price = price if target_notional != 0 else None

        reward = (realized_pnl - fee_cost) / self.initial_equity_usd if self.initial_equity_usd else 0.0

        self._step_idx += 1
        exhausted_token = self._step_idx >= len(rows) - 1
        if exhausted_token:
            self._token_idx += 1
            self._step_idx = 0
            self._reset_state_keep_progress()

        terminated = self.equity <= 0
        truncated = self._token_idx >= len(self.token_series)

        obs_vec = self._current_obs_vec() if not (terminated or truncated) else np.zeros(self.obs_dim, dtype=np.float32)
        info = {"equity": self.equity, "inst_id": series.inst_id, "leverage": self.leverage}
        return obs_vec, float(reward), terminated, truncated, info

    def _reset_state_keep_progress(self):
        """Resets simulated position/equity when moving to the next token, but keeps
        _token_idx/_step_idx (advanced by the caller) — each token starts its own simulated
        position from flat, matching CLAUDE.md §15.6's per-token budget isolation."""
        self.equity = self.initial_equity_usd
        self.position_notional = 0.0
        self.leverage = 1.0
        self.entry_price = None


def build_replay_env(
    inst_ids: list[str],
    bar: str,
    dsn: str | None = None,
    **env_kwargs,
) -> ReplayEnv:
    """Convenience constructor: connects to Postgres, loads each token's candle history +
    logged strategy signals, and builds the pooled ReplayEnv. CLAUDE.md §15.8."""
    conn = connect(dsn)
    try:
        series = [load_token_series(conn, inst_id, [bar]) for inst_id in inst_ids]
    finally:
        conn.close()
    return ReplayEnv(series, bar=bar, active_tokens=inst_ids, **env_kwargs)
