"""Gymnasium environment simulating OKX perpetual-swap futures trading against historical data.

Observation: a flattened window of engineered features plus current account/position state.
Action: Box(2,) = [target_exposure in [-1, 1], leverage_frac in [0, 1]].
  - target_exposure is the desired position as a fraction of max allowed notional (sign = side).
  - leverage_frac maps linearly to [1x, max_leverage].
Reward: fee-adjusted change in equity (as a fraction of equity) minus a penalty for getting close
  to the estimated liquidation price and a penalty for drawdown from the running peak equity.
  This is what lets the agent learn "risk of opening/closing a position" implicitly, without a
  separate risk-classification model.
"""
from __future__ import annotations

import numpy as np
import gymnasium as gym
from gymnasium import spaces

from rl_service.data.features import FEATURE_COLUMNS


class OkxFuturesEnv(gym.Env):
    metadata = {"render_modes": []}

    def __init__(
        self,
        df,
        window_size: int = 32,
        max_leverage: float = 5.0,
        max_position_notional_usd: float = 1000.0,
        taker_fee_rate: float = 0.0005,
        funding_rate_per_8h: float = 0.0001,
        bars_per_funding: int = 8,  # e.g. 8 hourly bars ~= one 8h funding interval
        initial_equity_usd: float = 10_000.0,
        liquidation_maintenance_margin_pct: float = 0.5,
    ):
        super().__init__()
        self.df = df.reset_index(drop=True)
        self.window_size = window_size
        self.max_leverage = max_leverage
        self.max_position_notional_usd = max_position_notional_usd
        self.taker_fee_rate = taker_fee_rate
        self.funding_rate_per_8h = funding_rate_per_8h
        self.bars_per_funding = bars_per_funding
        self.initial_equity_usd = initial_equity_usd
        self.liq_maintenance_margin_pct = liquidation_maintenance_margin_pct

        n_features = len(FEATURE_COLUMNS)
        obs_dim = n_features * window_size + 4  # + exposure, leverage, upl_pct, equity_ratio
        self.observation_space = spaces.Box(low=-np.inf, high=np.inf, shape=(obs_dim,), dtype=np.float32)
        self.action_space = spaces.Box(low=np.array([-1.0, 0.0]), high=np.array([1.0, 1.0]), dtype=np.float32)

        self._max_step = len(self.df) - 1
        self._reset_state()

    def _reset_state(self):
        self.t = self.window_size
        self.equity = self.initial_equity_usd
        self.peak_equity = self.equity
        self.position_notional = 0.0  # signed USD notional
        self.leverage = 1.0
        self.entry_price = None

    def reset(self, *, seed=None, options=None):
        super().reset(seed=seed)
        self._reset_state()
        return self._get_obs(), {}

    def _get_obs(self) -> np.ndarray:
        window = self.df.iloc[self.t - self.window_size : self.t][FEATURE_COLUMNS].to_numpy()
        price = self.df.iloc[self.t]["close"]
        exposure = self.position_notional / self.max_position_notional_usd
        upl_pct = self._unrealized_pnl_pct(price)
        equity_ratio = self.equity / self.initial_equity_usd
        tail = np.array([exposure, self.leverage / self.max_leverage, upl_pct, equity_ratio], dtype=np.float32)
        return np.concatenate([window.flatten().astype(np.float32), tail])

    def _unrealized_pnl_pct(self, price: float) -> float:
        if self.entry_price is None or self.position_notional == 0:
            return 0.0
        direction = 1.0 if self.position_notional > 0 else -1.0
        return direction * (price / self.entry_price - 1.0) * self.leverage

    def _liquidation_buffer_pct(self, price: float) -> float:
        """Approximate distance to liquidation as a % of price, for isolated-style margining."""
        if self.leverage <= 0 or self.position_notional == 0:
            return 100.0
        # Rough approximation: liquidation happens when losses consume (1/leverage - maintenance
        # margin) of the position's notional. Good enough as a training-time risk signal, not a
        # substitute for OKX's real liquidation engine.
        return max(0.0, (1.0 / self.leverage - self.liq_maintenance_margin_pct / 100.0) * 100.0)

    def step(self, action: np.ndarray):
        target_exposure = float(np.clip(action[0], -1.0, 1.0))
        leverage_frac = float(np.clip(action[1], 0.0, 1.0))

        price = float(self.df.iloc[self.t]["close"])
        next_leverage = 1.0 + leverage_frac * (self.max_leverage - 1.0)
        target_notional = target_exposure * self.max_position_notional_usd

        # Realize PnL on the position we're closing/reducing before changing exposure.
        realized_pnl = 0.0
        if self.entry_price is not None and self.position_notional != 0:
            direction = 1.0 if self.position_notional > 0 else -1.0
            price_ret = direction * (price / self.entry_price - 1.0)
            realized_pnl = price_ret * abs(self.position_notional)

        traded_notional = abs(target_notional - self.position_notional)
        fee_cost = traded_notional * self.taker_fee_rate

        funding_cost = 0.0
        if self.t % self.bars_per_funding == 0 and self.position_notional != 0:
            funding_cost = abs(self.position_notional) * self.funding_rate_per_8h

        self.equity += realized_pnl - fee_cost - funding_cost
        self.position_notional = target_notional
        self.leverage = next_leverage
        self.entry_price = price if target_notional != 0 else None
        self.peak_equity = max(self.peak_equity, self.equity)

        drawdown_pct = 0.0 if self.peak_equity <= 0 else (self.peak_equity - self.equity) / self.peak_equity
        liq_buffer_pct = self._liquidation_buffer_pct(price)
        liq_penalty = max(0.0, (10.0 - liq_buffer_pct) / 10.0) if liq_buffer_pct < 10.0 else 0.0

        reward = (realized_pnl - fee_cost - funding_cost) / self.initial_equity_usd
        reward -= 0.5 * drawdown_pct
        reward -= 0.3 * liq_penalty

        self.t += 1
        terminated = self.equity <= 0
        truncated = self.t >= self._max_step

        info = {
            "equity": self.equity,
            "position_notional": self.position_notional,
            "leverage": self.leverage,
            "drawdown_pct": drawdown_pct,
            "liq_buffer_pct": liq_buffer_pct,
        }
        obs = self._get_obs() if not (terminated or truncated) else np.zeros(self.observation_space.shape, dtype=np.float32)
        return obs, float(reward), terminated, truncated, info
