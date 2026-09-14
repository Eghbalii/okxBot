"""Tests for the single reward function (docs/RL_V8_PLAN.md).

These pin PROPERTIES, not numbers. A weight can be retuned against real training curves without
breaking them; what must not change is the direction of each incentive — that is the part a future
edit could invert without anyone noticing, because a reward function has no output anyone reads.
"""
from __future__ import annotations

import pytest

from rl_service.obs import ZERO_REWARD_CATEGORIES
from rl_service.reward import (
    LIQ_BUFFER_FLOOR,
    REWARD_CLIP,
    drawdown_pct,
    liquidation_penalty,
    trade_reward,
)


def _trade(**overrides):
    """A typical real trade: +5% of margin on 6.6% risk at 10x, from the measured distribution."""
    base = dict(
        realized_pnl_usd=0.125,
        fees_usd=0.005,
        risk_pct=0.066,
        position_size_usd=2.5,
        leverage=10.0,
        equity_usd=40.0,
        peak_equity_usd=40.0,
        sltp_adjustments=0.0,
    )
    base.update(overrides)
    return trade_reward(**base)


# --- the headline property ------------------------------------------------------------------------


def test_the_same_gain_scores_higher_when_less_was_risked():
    """The change the operator singled out.

    `pnl / size` scores a 5% gain made with a 1% stop identically to one made with a 15% stop,
    though the second took three times the risk for the same result. Dividing by risk taken is what
    makes the model prefer the first.
    """
    tight = _trade(risk_pct=0.02)
    wide = _trade(risk_pct=0.15)
    assert tight.total > wide.total


def test_risk_adjustment_is_proportional_not_merely_ordered():
    """Three times the risk for the same gain should score about a third, not merely less."""
    a = _trade(risk_pct=0.05)
    b = _trade(risk_pct=0.15)
    assert b.ret == pytest.approx(a.ret / 3.0, rel=0.01)


def test_falls_back_to_return_on_capital_when_risk_is_unknown():
    """A position opened with no stop, or a historical row that never recorded one. Worse than
    risk-adjusted, but defined — and far better than scoring zero, which would teach nothing."""
    r = _trade(risk_pct=0.0)
    assert r.ret == pytest.approx(0.12 / 2.5)


def test_a_trade_with_no_size_scores_nothing():
    assert _trade(position_size_usd=0.0, risk_pct=0.0).total == 0.0


# --- leverage must cost something -------------------------------------------------------------------


def test_the_same_trade_at_higher_leverage_scores_strictly_less():
    """§15.13's own named property. Without this term the policy can hold maximum leverage
    indefinitely at no cost right up until it blows up, because the drawdown term only charges for
    losses already taken."""
    assert _trade(leverage=10.0).total > _trade(leverage=25.0).total > _trade(leverage=100.0).total


def test_leverage_is_free_below_the_buffer_floor():
    """Deliberate: at 10x a position sits exactly at the floor, and 10x is the OKX cap this project
    trades at. Penalizing the only leverage available would be a constant, not a signal."""
    assert liquidation_penalty(1.0 / LIQ_BUFFER_FLOOR) == 0.0
    assert liquidation_penalty(5.0) == 0.0


def test_leverage_penalty_ramps_rather_than_banning():
    """A genuinely good high-leverage trade can still pay for itself, which is the intent."""
    p = liquidation_penalty(20.0)
    assert 0.0 < p < 1.0
    assert _trade(leverage=20.0).total > 0.0


def test_zero_or_negative_leverage_is_not_a_penalty():
    assert liquidation_penalty(0.0) == 0.0
    assert liquidation_penalty(-1.0) == 0.0


# --- churn ------------------------------------------------------------------------------------------


def test_moving_levels_costs_something():
    """§15.5: every adjustment should earn its keep in realized outcome rather than being free to
    try. Production had no churn cost at all, and §54.9 recorded the model making 81 stop
    adjustments across 66 orders in one hour."""
    assert _trade(sltp_adjustments=0.0).total > _trade(sltp_adjustments=3.0).total


def test_churn_does_not_swamp_a_good_trade():
    """A useful adjustment must still pay. If churn could dominate the return the policy would
    simply stop managing positions, which is the opposite of what the term is for."""
    assert _trade(sltp_adjustments=5.0).total > 0.0


# --- drawdown ----------------------------------------------------------------------------------------


def test_being_under_water_costs_something():
    assert _trade(equity_usd=40.0).total > _trade(equity_usd=30.0).total


def test_drawdown_is_measured_from_the_peak_not_the_start():
    """v7 fed equity/initial, and SetAccountCap rewrites initial (§32.2) — so every cap change
    wiped the model's view of drawdown back to ~1.0."""
    assert drawdown_pct(equity=80.0, peak_equity=100.0) == pytest.approx(0.2)
    assert drawdown_pct(equity=120.0, peak_equity=100.0) == 0.0


def test_no_peak_recorded_is_not_a_drawdown():
    assert drawdown_pct(equity=40.0, peak_equity=0.0) == 0.0


# --- losses, clipping, manual closes -------------------------------------------------------------------


def test_a_loss_is_negative():
    assert _trade(realized_pnl_usd=-0.125).total < 0.0


def test_reward_is_clipped_both_ways():
    """A stop is capped at 15% of margin (§19.2) so a realistic loss cannot exceed ~1.0 here. A data
    error or a gapped fill can, and one outlier distorts every sample drawn from a small replay
    buffer afterwards."""
    assert _trade(realized_pnl_usd=1000.0).total == REWARD_CLIP
    assert _trade(realized_pnl_usd=-1000.0).total == -REWARD_CLIP


def test_a_manual_close_delivers_no_reward():
    """§15.12: attributing an operator's action to the policy would train it on a decision it never
    made. The call still happens so the learner's pending decision resolves rather than leaking."""
    r = trade_reward(realized_pnl_usd=5.0, risk_pct=0.05, position_size_usd=2.5, zero_reward=True)
    assert r.total == 0.0
    assert r.ret == 0.0


def test_fees_reduce_the_reward():
    """§41 found the live path storing gross PnL where the exchange reports the fee separately —
    every trade was overstated by a round trip's cost."""
    assert _trade(fees_usd=0.0).total > _trade(fees_usd=0.05).total


def test_every_term_is_reported_separately():
    """A reward falling because of risk and one falling because of bad entries need completely
    different fixes, and an aggregate number cannot tell them apart (§15.13)."""
    d = _trade(leverage=50.0, equity_usd=30.0, sltp_adjustments=2.0).as_dict()
    assert d["leverage_penalty"] > 0
    assert d["drawdown_penalty"] > 0
    assert d["churn_penalty"] > 0
    assert d["reward"] == pytest.approx(
        d["return"] - d["churn_penalty"] - d["leverage_penalty"] - d["drawdown_penalty"]
    )


# --- the penalties must actually be REACHED from a real observation ---------------------------
#
# These exist because the penalties were written, tested in isolation, and then two of the three
# inputs they need were never carried on the wire — so churn was permanently zero and the reward
# silently fell back to return on capital for every trade. A penalty that cannot be reached is
# indistinguishable from one that was never written, and §54.9 measured the consequence: 81 stop
# adjustments across 66 orders in one hour, with nothing in the reward objecting.


def _terminal_obs(**position_overrides):
    """A terminal observation shaped the way the Go engine actually sends one."""
    from rl_service.obs import Observation, PositionState

    ps = dict(
        position_open=1.0,
        side=1.0,
        size_usd=2.5,
        leverage=10.0,
        realized_pnl_usd=0.125,
        risk_pct=0.066,
        sltp_adjustments=0.0,
    )
    ps.update(position_overrides)
    return Observation(
        inst_id="SOL",
        last_price=100.0,
        category="closed_tp",
        order_id=1,
        position_state=PositionState(**ps),
        account_equity_usd=40.0,
        account_initial_usd=40.0,
        account_peak_usd=40.0,
    )


def _score(obs):
    """Scores an observation exactly as serve/api.py's terminal path does."""
    ps = obs.position_state
    return trade_reward(
        realized_pnl_usd=ps.realized_pnl_usd,
        risk_pct=ps.risk_pct,
        position_size_usd=ps.size_usd,
        leverage=ps.leverage,
        equity_usd=obs.account_equity_usd,
        peak_equity_usd=obs.account_peak_usd or obs.account_initial_usd,
        sltp_adjustments=ps.sltp_adjustments,
        zero_reward=obs.category in ZERO_REWARD_CATEGORIES,
    )


def test_risk_reaches_the_reward_from_an_observation():
    """risk_pct was a declared field nothing populated, so every trade scored as return on capital."""
    tight = _score(_terminal_obs(risk_pct=0.02))
    wide = _score(_terminal_obs(risk_pct=0.15))
    assert tight.total > wide.total


def test_churn_reaches_the_reward_from_an_observation():
    """sltp_adjustments likewise: the penalty had no count to charge against."""
    quiet = _score(_terminal_obs(sltp_adjustments=0.0))
    twitchy = _score(_terminal_obs(sltp_adjustments=20.0))
    assert quiet.total > twitchy.total
    assert twitchy.churn_penalty > 0


def test_leverage_reaches_the_reward_from_an_observation():
    low = _score(_terminal_obs(leverage=10.0))
    high = _score(_terminal_obs(leverage=50.0))
    assert low.total > high.total
    assert high.leverage_penalty > 0


def test_drawdown_reaches_the_reward_from_an_observation():
    obs = _terminal_obs()
    obs.account_equity_usd = 30.0
    obs.account_peak_usd = 40.0
    assert _score(obs).drawdown_penalty > 0


def test_a_manual_close_scores_nothing_even_with_a_large_gain():
    obs = _terminal_obs(realized_pnl_usd=5.0)
    obs.category = "closed_manual"
    assert _score(obs).total == 0.0
