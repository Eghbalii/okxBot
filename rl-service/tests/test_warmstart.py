"""Tests for the warm-start trainer.

The property that matters here is the ROUND TRIP: the action vector this builds must decode back to
the trade that was actually taken. Getting it wrong is silent and total — the critic learns to value
an output that did not cause the reward, every number stays well-formed, and nothing downstream can
detect it. So the tests assert against decode_action rather than against a hand-written expectation,
which would only prove this file agrees with itself.
"""
from __future__ import annotations

import json

import numpy as np
import pytest

from rl_service.obs import (
    ACTION_DIM,
    MAX_SLTP_OFFSET_PCT,
    Observation,
    PositionState,
    StrategySignal,
    decode_action,
)
from rl_service.warmstart import Sample, action_for, read_samples


def _sample(
    *,
    price: float = 100.0,
    sl: float = 98.0,
    tp: float = 104.0,
    size_usd: float = 2.5,
    leverage: float = 10.0,
    equity: float = 40.0,
    max_position_pct: float = 0.25,
    max_leverage: float = 10.0,
    reward: float = 1.0,
) -> Sample:
    obs = Observation(
        inst_id="DOGE",
        last_price=price,
        category="buy",
        signal=StrategySignal(side="buy", entry_px=price, sl_px=sl, tp_px=tp),
        account_equity_usd=equity,
        max_position_pct=max_position_pct,
        max_leverage=max_leverage,
        order_id=7,
    )
    term = Observation(
        inst_id="DOGE",
        last_price=price,
        category="closed_tp",
        signal=obs.signal,
        position_state=PositionState(
            position_open=1.0, side=1.0, size_usd=size_usd, leverage=leverage
        ),
        account_equity_usd=equity,
        max_position_pct=max_position_pct,
        max_leverage=max_leverage,
        order_id=7,
    )
    return Sample(
        observation=obs,
        terminal=term,
        reward=reward,
        inst_id="DOGE",
        kind="pmax",
        close_reason="tp",
        realized_pnl=0.5,
    )


def test_action_has_the_models_own_width():
    assert action_for(_sample()).shape == (ACTION_DIM,)


def test_action_decodes_back_to_the_trade_that_was_taken():
    """The round trip: what this builds must mean what the simulation did."""
    s = _sample(price=100.0, sl=98.0, tp=104.0, size_usd=2.5, leverage=10.0)
    got = decode_action(action_for(s), s.observation)

    assert got.action == "open"
    # Levels come back as prices, against the same last_price they were measured from.
    assert got.sl_px == pytest.approx(98.0, abs=1e-6)
    assert got.tp_px == pytest.approx(104.0, abs=1e-6)
    # size_usd 2.5 of a $40 account is 6.25% of equity, against a 25% allowance = a quarter of it.
    # decode_action rescales by max_position_pct, so it must come back as the 6.25% it really was.
    assert got.size_pct == pytest.approx(2.5 / 40.0, rel=1e-5)
    # leverage_frac maps [1x, 10x] -> [0, 1], so 10x is the top of the range.
    assert got.leverage_frac == pytest.approx(1.0, abs=1e-6)


def test_open_head_is_positive_because_the_trade_was_taken():
    """Every dataset sample is a trade that happened. A negative head would decode as `skip`,
    teaching the policy that these observations were declined — the exact opposite of the truth."""
    assert action_for(_sample())[4] >= 0.0
    assert decode_action(action_for(_sample()), _sample().observation).action == "open"


def test_the_action_goes_through_the_learners_own_masking_function():
    """The manage head must not reach the buffer carrying a value on a buy/sell call: an output
    rewarded without causing anything drifts to the tanh bound unopposed (§54.8).

    This file builds from np.zeros and never writes the manage head, so the mask is currently a
    no-op on this path and asserting "the head is zero" would pass with the call removed entirely —
    an earlier version of this test did exactly that. What is worth pinning is that the SAME
    function the live learner uses is the one applied, so the two paths cannot disagree later about
    which head mattered; the assertion below fails if the call is dropped.
    """
    import rl_service.warmstart as ws

    seen: list[str] = []
    real = ws.mask_action_for_learning
    ws.mask_action_for_learning = lambda vec, cat: (seen.append(cat), real(vec, cat))[1]
    try:
        vec = action_for(_sample())
    finally:
        ws.mask_action_for_learning = real

    assert seen == ["buy"], "action_for did not route through mask_action_for_learning"
    assert np.allclose(vec[5 : 5 + 3], 0.0)
    assert vec[4] > 0.0  # the open head must SURVIVE the mask on a buy/sell call


def test_a_level_beyond_the_offset_bound_is_clamped_not_wrapped():
    """A stop further than MAX_SLTP_OFFSET_PCT cannot be represented. Clamping loses magnitude;
    wrapping would change the DIRECTION, teaching the policy a stop on the wrong side of entry."""
    s = _sample(price=100.0, sl=50.0)  # -50%, far beyond the 10% the action can express
    vec = action_for(s)
    assert vec[0] == pytest.approx(-1.0)
    assert decode_action(vec, s.observation).sl_px == pytest.approx(
        100.0 * (1.0 - MAX_SLTP_OFFSET_PCT)
    )


def test_degenerate_leverage_cap_does_not_divide_by_zero():
    s = _sample(leverage=1.0, max_leverage=1.0)
    vec = action_for(s)
    assert np.isfinite(vec[3])
    assert 0.0 <= vec[3] <= 1.0


def test_zero_equity_does_not_divide_by_zero():
    s = _sample(equity=0.0)
    vec = action_for(s)
    assert np.isfinite(vec[2])
    assert vec[2] == 0.0


def test_size_is_read_from_the_terminal_not_the_decision():
    """On a buy/sell call position_state is zero by design — the decision is whether to open at all.
    Reading size from there would record every trade as size zero, and the policy would learn that
    the reward came from opening nothing."""
    s = _sample(size_usd=2.5)
    assert s.observation.position_state.size_usd == 0.0  # the premise
    assert action_for(s)[2] > 0.0


def test_read_samples_rejects_a_malformed_line(tmp_path):
    """Fatal rather than skipped: a dataset that silently trains on 90% of itself looks exactly like
    one that trains on all of it."""
    p = tmp_path / "bad.jsonl"
    p.write_text('{"observation": {}, "terminal": {}, "reward": 0.0}\n{not json}\n')
    with pytest.raises(ValueError, match="bad.jsonl:2"):
        list(read_samples(str(p)))


def test_read_samples_parses_a_real_line(tmp_path):
    s = _sample()
    line = json.dumps(
        {
            "observation": s.observation.model_dump(),
            "terminal": s.terminal.model_dump(),
            "reward": 1.25,
            "inst_id": "DOGE",
            "kind": "pmax",
            "close_reason": "tp",
            "realized_pnl_usd": 0.5,
        }
    )
    p = tmp_path / "ok.jsonl"
    p.write_text(line + "\n\n")  # trailing blank line must not become a sample
    got = list(read_samples(str(p)))
    assert len(got) == 1
    assert got[0].reward == 1.25
    assert got[0].observation.inst_id == "DOGE"
    assert got[0].terminal.position_state.size_usd == pytest.approx(2.5)
