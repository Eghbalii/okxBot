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
    ACTION_DIM,
    MAX_SLTP_ADJUST_PCT,
    Observation,
    PriceContext,
    StrategySignal,
    TimeframeBlock,
    observation_features,
    observation_tail,
)

SWING_WINDOW = 20
PRICE_CONTEXT_WINDOW = 10

# Penalty weight on SL/TP adjustment churn (CLAUDE.md §15.5) — every proposed adjustment costs a
# little reward, so the agent only moves a stop when the resulting outcome pays for it rather than
# twitching it every step for free. Scaled against the normalized [-1, 1] raw outputs, not the
# post-MAX_SLTP_ADJUST_PCT fractions, so the penalty doesn't shrink if that bound is widened later.
SLTP_CHURN_PENALTY = 0.002

# Fallback SL/TP distances for a simulated position opened at a bar with no logged strategy signal
# to take them from — deliberately wide enough not to dominate outcomes, since their only job is to
# give the SL/TP-adjust action something real to ratchet against.
DEFAULT_SL_PCT = 0.02
DEFAULT_TP_PCT = 0.04


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
        taker_fee_rate: float = 0.0005,
        # One shared account across every pooled token (CLAUDE.md §15.6), matching go-engine's
        # account.initial_usd — not a per-token sub-budget.
        initial_equity_usd: float = 100.0,
        # Mirrors account.max_position_pct / account.max_total_exposure_pct on the Go side. The
        # policy is trained under the SAME caps production applies, so it doesn't spend its output
        # range learning to request sizes that rlSizing would just clamp away.
        max_position_pct: float = 0.25,
        max_total_exposure_pct: float = 0.60,
    ):
        super().__init__()
        self.token_series = [t for t in token_series if t.bars.get(bar)]
        if not self.token_series:
            raise ValueError(f"no candle history available for bar {bar!r} across the given tokens")
        self.bar = bar
        self.active_tokens = active_tokens
        self.max_leverage = max_leverage
        self.taker_fee_rate = taker_fee_rate
        self.initial_equity_usd = initial_equity_usd
        self.max_position_pct = max_position_pct
        self.max_total_exposure_pct = max_total_exposure_pct

        self._reset_state()

        # obs_dim is only known once we've built one sample observation.
        probe_obs = self._build_observation(self.token_series[0], 0)
        tail_len = len(observation_tail(probe_obs))
        feat_len = len(observation_features(probe_obs))
        self.obs_dim = tail_len + feat_len
        self.observation_space = spaces.Box(low=-np.inf, high=np.inf, shape=(self.obs_dim,), dtype=np.float32)
        # Full CLAUDE.md §15.4 action: [target_exposure, leverage_frac, sl_adjust_pct,
        # tp_adjust_pct, strategy weight slots...]. See rl_service.obs.decode_action for the
        # layout and how each component is mapped — the env and /predict MUST agree on it, which is
        # why both go through that one shared decoder rather than indexing the vector themselves.
        low = np.array([-1.0, 0.0, -1.0, -1.0] + [0.0] * (ACTION_DIM - 4), dtype=np.float32)
        high = np.ones(ACTION_DIM, dtype=np.float32)
        self.action_space = spaces.Box(low=low, high=high, dtype=np.float32)

    def _reset_state(self):
        self._token_idx = 0
        self._step_idx = 0
        self.equity = self.initial_equity_usd
        self.position_notional = 0.0
        self.leverage = 1.0
        self.entry_price = None
        self.sl_price: float | None = None
        self.tp_price: float | None = None

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
        price = float(row["close"])
        # dist_to_sl/tp must be populated here for the same reason go-engine's adjustOpenOrdersWithRL
        # sets them per-order (CLAUDE.md §15.3): they're the direct input to the sl/tp_adjust
        # decision, so a model trained with them pinned at zero would never learn to use them.
        return Observation(
            inst_id=series.inst_id,
            active_tokens=self.active_tokens,
            last_price=price,
            timeframes=[tb],
            position=1.0 if self.position_notional > 0 else (-1.0 if self.position_notional < 0 else 0.0),
            current_leverage=self.leverage,
            unrealized_pnl_pct=self._unrealized_pnl_pct(price),
            dist_to_sl_pct=(self.sl_price - price) / price if self.sl_price and price else 0.0,
            dist_to_tp_pct=(self.tp_price - price) / price if self.tp_price and price else 0.0,
            account_equity_usd=self.equity,
            account_initial_usd=self.initial_equity_usd,
            open_exposure_usd=abs(self.position_notional),
        )

    def _unrealized_pnl_pct(self, price: float) -> float:
        if self.entry_price is None or self.position_notional == 0 or not self.entry_price:
            return 0.0
        direction = 1.0 if self.position_notional > 0 else -1.0
        return direction * (price / self.entry_price - 1.0)

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
        bar_high, bar_low = float(row["high"]), float(row["low"])

        vec = np.asarray(action, dtype=np.float32).reshape(-1)
        target_exposure = float(np.clip(vec[0], -1.0, 1.0))
        leverage_frac = float(np.clip(vec[1], 0.0, 1.0))
        sl_adjust_pct = float(np.clip(vec[2], -1.0, 1.0))
        tp_adjust_pct = float(np.clip(vec[3], -1.0, 1.0))
        next_leverage = 1.0 + leverage_frac * (self.max_leverage - 1.0)
        # Size against the SHARED ACCOUNT BALANCE, under the same caps go-engine's rlSizing applies
        # (CLAUDE.md §15.6) — so the policy learns what a position of a given size actually costs
        # the account it has to keep alive, rather than sizing against a fixed notional constant.
        # Note self.equity here is pre-settlement for this bar; production sizes against the equity
        # it can read at decision time, which is likewise the last settled balance.
        equity = max(self.equity, 0.0)
        cap = equity * min(self.max_position_pct, self.max_total_exposure_pct)
        target_notional = float(np.clip(target_exposure * equity, -cap, cap))

        realized_pnl = 0.0
        exit_price = price

        # An open position's SL/TP is checked against the bar's real high/low BEFORE the new action
        # resizes anything — same ordering as live, where monitorOpenOrders' tick-driven touch check
        # runs independently of (and ahead of) the next decision. A touch closes the position at the
        # stop price and the step's exposure action then applies from flat.
        if self.position_notional != 0 and self.entry_price is not None:
            direction = 1.0 if self.position_notional > 0 else -1.0
            touched = None
            if self.sl_price is not None and (
                (direction > 0 and bar_low <= self.sl_price) or (direction < 0 and bar_high >= self.sl_price)
            ):
                touched = self.sl_price
            elif self.tp_price is not None and (
                (direction > 0 and bar_high >= self.tp_price) or (direction < 0 and bar_low <= self.tp_price)
            ):
                touched = self.tp_price

            if touched is not None:
                exit_price = touched
                realized_pnl = direction * (exit_price / self.entry_price - 1.0) * abs(self.position_notional)
                self.position_notional = 0.0
                self.entry_price = None
                self.sl_price = self.tp_price = None
            else:
                realized_pnl = direction * (price / self.entry_price - 1.0) * abs(self.position_notional)

        traded_notional = abs(target_notional - self.position_notional)
        fee_cost = traded_notional * self.taker_fee_rate

        self.equity += realized_pnl - fee_cost
        was_flat = self.position_notional == 0
        self.position_notional = target_notional
        self.leverage = next_leverage
        self.entry_price = price if target_notional != 0 else None

        if target_notional == 0:
            self.sl_price = self.tp_price = None
        elif was_flat or self.sl_price is None:
            # Opening (or re-opening) a position seeds SL/TP from the strategy signal's own
            # suggested distances where one was logged, falling back to a default band — this is the
            # order the RL agent then gets to ratchet, matching live's "strategy proposes, RL
            # adjusts" split (CLAUDE.md §15.4).
            self._seed_sltp(series, row, price, target_notional)
        else:
            self._apply_sltp_adjust(price, target_notional, sl_adjust_pct, tp_adjust_pct)

        reward = (realized_pnl - fee_cost) / self.initial_equity_usd if self.initial_equity_usd else 0.0
        # CLAUDE.md §15.5: charge for adjustment churn so every SL/TP move has to earn its keep in
        # realized outcome rather than being free to try.
        reward -= SLTP_CHURN_PENALTY * (abs(sl_adjust_pct) + abs(tp_adjust_pct))

        self._step_idx += 1
        exhausted_token = self._step_idx >= len(rows) - 1
        if exhausted_token:
            self._token_idx += 1
            self._step_idx = 0
            self._carry_account_to_next_token()

        terminated = self.equity <= 0
        truncated = self._token_idx >= len(self.token_series)

        obs_vec = self._current_obs_vec() if not (terminated or truncated) else np.zeros(self.obs_dim, dtype=np.float32)
        info = {"equity": self.equity, "inst_id": series.inst_id, "leverage": self.leverage}
        return obs_vec, float(reward), terminated, truncated, info

    def _seed_sltp(self, series: _TokenSeries, row: dict, price: float, notional: float) -> None:
        """Sets the initial SL/TP for a newly opened position from the strategy signal logged at
        this bar, if any, else DEFAULT_SL_PCT/DEFAULT_TP_PCT."""
        sl_pct, tp_pct = DEFAULT_SL_PCT, DEFAULT_TP_PCT
        sigs = series.logged_signals.get((self.bar, int(row["ts"].timestamp() * 1000)), [])
        for s in sigs:
            if s.sl_pct > 0:
                sl_pct = s.sl_pct
            if s.tp_pct > 0:
                tp_pct = s.tp_pct
            break

        direction = 1.0 if notional > 0 else -1.0
        self.sl_price = price * (1.0 - direction * sl_pct)
        self.tp_price = price * (1.0 + direction * tp_pct)

    def _apply_sltp_adjust(self, price: float, notional: float, sl_adjust_pct: float, tp_adjust_pct: float) -> None:
        """Applies the policy's SL/TP adjustment under the same ratchet rule Go enforces
        (usecase.RatchetSLTP, CLAUDE.md §15.4): an adjustment may only tighten — move SL toward the
        current price (locking in profit / cutting risk) and TP toward it (easier to reach) — never
        widen or walk back a prior tightening.

        Mirroring the clamp here matters for training, not safety: Go re-applies its own clamp
        regardless, so a policy trained against an unclamped env would spend much of its output
        range proposing adjustments production silently discards, and never see the reward
        consequence of the ones that actually land.
        """
        direction = 1.0 if notional > 0 else -1.0

        if self.sl_price is not None:
            candidate = self.sl_price * (1.0 + sl_adjust_pct * MAX_SLTP_ADJUST_PCT)
            # Tightening moves SL up for a long, down for a short — and never past the live price,
            # which would close the position instantly.
            if direction * (candidate - self.sl_price) > 0 and direction * (price - candidate) > 0:
                self.sl_price = candidate

        if self.tp_price is not None:
            candidate = self.tp_price * (1.0 + tp_adjust_pct * MAX_SLTP_ADJUST_PCT)
            # Tightening pulls TP toward price: down for a long, up for a short, never across it.
            if direction * (candidate - self.tp_price) < 0 and direction * (candidate - price) > 0:
                self.tp_price = candidate

    def _carry_account_to_next_token(self):
        """Flattens the simulated position when the rollout moves to the next token, but CARRIES
        THE ACCOUNT BALANCE FORWARD (CLAUDE.md §15.6's 2026-08-28 revision).

        This used to reset equity per token, mirroring the old per-token sub-budgets. With one
        shared account that would be wrong in a way that matters for training: the agent would learn
        that losses are wiped clean at each token boundary, i.e. that over-committing has no lasting
        consequence — the exact behavior the shared-account design needs it to feel."""
        self.position_notional = 0.0
        self.leverage = 1.0
        self.entry_price = None
        self.sl_price = None
        self.tp_price = None


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
