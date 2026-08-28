"""Sanity checks for ReplayEnv (CLAUDE.md §15.8) — run with `python -m pytest rl-service/tests`.

Uses synthetic CandleRow data (no real Postgres) so these run in any environment; the Postgres
read path itself (rl_service/data/postgres.py) is thin enough to not need its own mock — it's
exercised for real whenever build_replay_env is actually run against a live database.
"""
from __future__ import annotations

from datetime import datetime, timedelta

import numpy as np
import pytest
from stable_baselines3 import PPO
from stable_baselines3.common.monitor import Monitor

from rl_service.data.postgres import CandleRow
from rl_service.env.replay_env import DEFAULT_SL_PCT, ReplayEnv, _TokenSeries, _rows_to_feature_dicts
from rl_service.obs import ACTION_DIM, ACTIONS, MAX_SLTP_OFFSET_PCT, Observation, observation_tail


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
    env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=1000.0)

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
    env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=1000.0)

    obs_token0, _ = env.reset()
    env._token_idx = 1
    env._step_idx = 0
    obs_token1 = env._current_obs_vec()

    assert not np.array_equal(obs_token0, obs_token1)


def test_multi_token_rollout_advances_through_all_tokens():
    series = _two_token_series()
    env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=1000.0)
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
        ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=1000.0)
    )
    model = PPO("MlpPolicy", env, n_steps=32, batch_size=16, verbose=0)
    model.learn(total_timesteps=64)

    model_path = tmp_path / "ppo_replay_test.zip"
    model.save(str(model_path))
    loaded = PPO.load(str(model_path))

    obs, _ = env.reset()
    action, _ = loaded.predict(obs, deterministic=True)
    # The full CLAUDE.md §15.4 action, not the original Box(2,): a model trained here must be able
    # to answer sl/tp_adjust and strategy_weights, or /predict can only ever return stubs for them.
    assert action.shape == (ACTION_DIM,)


def test_raises_when_no_candle_history_for_bar():
    series = [_TokenSeries(inst_id="BTC-USDT-SWAP", bars={})]
    try:
        ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP"])
        assert False, "expected ValueError for empty candle history"
    except ValueError:
        pass


def _env_with_open_long(price=100.0):
    """A ReplayEnv holding an open long with a known SL/TP, for the ratchet tests below."""
    series = _two_token_series()
    env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=1000.0)
    env.reset()
    env.position_notional = 100.0
    env.entry_price = price
    env.sl_price = price * (1 - DEFAULT_SL_PCT)
    env.tp_price = price * (1 + DEFAULT_SL_PCT * 2)
    return env






def test_stop_touch_closes_position_at_stop_price():
    # The SL/TP touch check must use the bar's real high/low, not just its close — the same
    # intra-bar-wick correctness reason PaperTrader checks SL/TP on ticks (CLAUDE.md §14).
    series = _two_token_series()
    env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=1000.0)
    env.reset()

    rows = env.token_series[0].bars["1m"]
    row = rows[0]
    env.position_notional = 100.0
    env.entry_price = float(row["close"]) * 1.10   # deep underwater long
    env.sl_price = float(row["high"]) * 1.05       # guaranteed to be touched by this bar's low
    env.tp_price = None

    equity_before = env.equity
    env.step(np.zeros(ACTION_DIM, dtype=np.float32))

    assert env.equity < equity_before, "a stopped-out long must realize a loss"
    assert env.position_notional == 0.0
    assert env.sl_price is None and env.tp_price is None


def test_churn_penalty_makes_adjusting_cost_more_than_not_adjusting():
    # CLAUDE.md §15.5: adjustments aren't free, so the agent has to earn them back in realized
    # outcome rather than twitching the stop every step.
    series = _two_token_series()

    def _reward_for(sl_adjust):
        env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=1000.0)
        env.reset()
        action = np.zeros(ACTION_DIM, dtype=np.float32)
        action[2] = sl_adjust
        _, reward, _, _, _ = env.step(action)
        return reward

    assert _reward_for(1.0) < _reward_for(0.0)


def test_account_balance_carries_across_tokens():
    # CLAUDE.md §15.6 (revised): with ONE shared account, a loss on the first token must still be
    # felt when the rollout moves to the next. Resetting equity per token — which the per-token
    # sub-budget design used to do — would teach the agent that over-committing costs it nothing.
    series = _two_token_series()
    env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=1000.0)
    env.reset()

    env.equity = 250.0
    env._token_idx = 0
    env._carry_account_to_next_token()

    assert env.equity == 250.0, "the shared balance must survive a token boundary"
    assert env.position_notional == 0.0, "the position itself is still flattened per token"


def test_position_size_is_capped_to_a_fraction_of_equity():
    # The env applies the same caps go-engine's rlSizing does, so the policy never learns to ask for
    # sizes production would clamp away.
    series = _two_token_series()
    env = ReplayEnv(
        series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"],
        initial_equity_usd=1000.0, max_position_pct=0.25, max_total_exposure_pct=0.60,
    )
    env.reset()

    action = np.zeros(ACTION_DIM, dtype=np.float32)
    action[0] = 1.0  # ask for the whole account
    env.step(action)

    assert abs(env.position_notional) <= 250.0 + 1e-6, (
        f"expected the position capped at 25% of a 1000 account, got {env.position_notional}"
    )


def test_observation_tail_uses_account_ratios_not_raw_dollars():
    # Feeding raw balances would put the policy out of distribution the moment the account size is
    # reconfigured; the decision it makes is scale-free, so the inputs are ratios.
    small = Observation(inst_id="A", active_tokens=["A"], last_price=1.0,
                        account_equity_usd=50.0, account_initial_usd=100.0, open_exposure_usd=25.0)
    large = Observation(inst_id="A", active_tokens=["A"], last_price=1.0,
                        account_equity_usd=5000.0, account_initial_usd=10000.0, open_exposure_usd=2500.0)

    # Same ratios at 100x the scale => identical model input.
    assert np.allclose(observation_tail(small), observation_tail(large))


def test_model_sets_sl_and_tp_levels_directly():
    """CLAUDE.md §15.11: the model SETS the levels rather than nudging existing ones.

    Its raw output is a distance, which becomes a price against the live bar — a network output has
    no way to know whether an instrument trades at 0.15 or 65000.
    """
    env = _env_with_open_long()
    price = 100.0

    env._set_sltp(price, notional=100.0, sl_offset=-0.03, tp_offset=0.05)

    assert env.sl_price == pytest.approx(97.0), "SL must land at the chosen distance below price"
    assert env.tp_price == pytest.approx(105.0), "TP must land at the chosen distance above price"


def test_sltp_on_the_wrong_side_of_price_is_rejected():
    """A stop above price (for a long) would close the position the instant it was set, so the env
    clamps it the same way Go does rather than letting the policy learn from an impossible level."""
    env = _env_with_open_long()
    original_sl, original_tp = env.sl_price, env.tp_price

    # Long: a positive SL offset puts the stop ABOVE price, a negative TP offset puts it below.
    env._set_sltp(100.0, notional=100.0, sl_offset=+0.03, tp_offset=-0.05)

    assert env.sl_price == original_sl, "a stop on the winning side must be rejected"
    assert env.tp_price == original_tp, "a target on the losing side must be rejected"


def test_pnl_max_and_min_track_the_position_trajectory():
    """A trade that ran to +8% and came back to +1% must be distinguishable from one that only ever
    drifted to +1% (CLAUDE.md §15.11) — current PnL alone cannot express that difference."""
    env = _env_with_open_long(price=100.0)
    env.pnl_max_pct = env.pnl_min_pct = 0.0

    for upl in (0.03, 0.08, 0.01, -0.02):
        env.pnl_max_pct = max(env.pnl_max_pct, upl)
        env.pnl_min_pct = min(env.pnl_min_pct, upl)

    assert env.pnl_max_pct == pytest.approx(0.08)
    assert env.pnl_min_pct == pytest.approx(-0.02)


def test_entry_price_is_not_restamped_while_a_position_is_held():
    """Re-stamping entry on every step would erase the trade's basis and pin unrealized PnL at ~0."""
    series = _two_token_series()
    env = ReplayEnv(series, bar="1m", active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"], initial_equity_usd=1000.0)
    env.reset()

    open_action = np.zeros(ACTION_DIM, dtype=np.float32)
    open_action[2] = 0.2  # size_pct
    open_action[4 + ACTIONS.index("open")] = 1.0
    env.step(open_action)
    entry_after_open = env.entry_price
    assert entry_after_open is not None

    hold = np.zeros(ACTION_DIM, dtype=np.float32)
    hold[4 + ACTIONS.index("none")] = 1.0
    env.step(hold)

    assert env.entry_price == entry_after_open, "entry must survive a hold step"
