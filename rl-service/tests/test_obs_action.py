"""Tests for the shared action decoding (CLAUDE.md §15.4).

These cover the path that used to be hardcoded in rl_service/serve/api.py: strategy_weights,
sl_adjust_pct and tp_adjust_pct were returned as fixed neutral values regardless of what the model
said, which meant a trained model could never actually propose an SL/TP adjustment and go-engine's
shadow-fork mechanic could never fire. decode_action is now the single place that mapping happens,
shared by /predict and the replay env so the two can't drift.
"""
from __future__ import annotations

import numpy as np

from rl_service.obs import (
    ACTION_DIM,
    MAX_SLTP_ADJUST_PCT,
    MAX_STRATEGY_SLOTS,
    Observation,
    StrategySignal,
    TimeframeBlock,
    decode_action,
    flat_action,
    ordered_strategy_ids,
)


def _obs(strategy_ids=(1, 2)):
    return Observation(
        inst_id="BTC-USDT-SWAP",
        active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"],
        last_price=100.0,
        timeframes=[
            TimeframeBlock(
                bar="1m",
                strategy_signals=[StrategySignal(strategy_id=sid, side="buy", confidence=0.5) for sid in strategy_ids],
            )
        ],
    )


def test_decodes_all_five_action_components():
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    raw[0] = 0.5    # target_exposure
    raw[1] = 0.25   # leverage_frac
    raw[2] = 1.0    # sl_adjust -> full positive adjustment
    raw[3] = -0.5   # tp_adjust
    raw[4], raw[5] = 3.0, 1.0  # strategy weight slots (pre-clip/normalize)

    action = decode_action(raw, _obs())

    assert action.target_exposure == 0.5
    assert action.leverage_frac == 0.25
    # Raw [-1, 1] outputs are scaled onto the same bounded adjustment range Go's ratchet accepts.
    assert action.sl_adjust_pct == MAX_SLTP_ADJUST_PCT
    assert action.tp_adjust_pct == -0.5 * MAX_SLTP_ADJUST_PCT
    # Both slots clip to 1.0, so they normalize to an even split rather than 3:1.
    assert action.strategy_weights == {"1": 0.5, "2": 0.5}


def test_strategy_weights_normalize_to_one_over_present_strategies():
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    raw[4], raw[5] = 0.75, 0.25

    action = decode_action(raw, _obs())

    assert set(action.strategy_weights) == {"1", "2"}
    assert abs(sum(action.strategy_weights.values()) - 1.0) < 1e-6
    assert action.strategy_weights["1"] > action.strategy_weights["2"]


def test_only_present_strategies_get_a_weight():
    # The policy always emits MAX_STRATEGY_SLOTS slots, but a request carrying one strategy must
    # come back with exactly one weight — the caller keys these by strategy id, so a slot with no
    # backing signal has no id to report under.
    raw = np.ones(ACTION_DIM, dtype=np.float32)
    action = decode_action(raw, _obs(strategy_ids=(7,)))
    assert list(action.strategy_weights) == ["7"]


def test_all_zero_weights_fall_back_to_uniform():
    # A dict of zeros reads as "no directional signal at all" downstream, which is indistinguishable
    # from a bug — an indifferent policy should look uniform instead.
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    action = decode_action(raw, _obs())
    assert action.strategy_weights == {"1": 0.5, "2": 0.5}


def test_out_of_range_outputs_are_clipped():
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    raw[0] = 5.0    # far past the [-1, 1] exposure bound
    raw[1] = -2.0   # below the [0, 1] leverage bound
    raw[2] = 10.0

    action = decode_action(raw, _obs())

    assert action.target_exposure == 1.0
    assert action.leverage_frac == 0.0
    assert action.sl_adjust_pct == MAX_SLTP_ADJUST_PCT


def test_ordered_strategy_ids_truncates_at_max_slots():
    obs = _obs(strategy_ids=tuple(range(MAX_STRATEGY_SLOTS + 5)))
    assert len(ordered_strategy_ids(obs)) == MAX_STRATEGY_SLOTS


def test_short_action_vector_is_rejected():
    # Guards against serving a model trained on the old Box(2,) space: silently reading garbage out
    # of a too-short vector is exactly the failure mode the version check exists to prevent.
    try:
        decode_action(np.zeros(2, dtype=np.float32), _obs())
        assert False, "expected ValueError for a too-short action vector"
    except ValueError:
        pass


def test_flat_action_proposes_no_risk_and_no_adjustment():
    action = flat_action()
    assert action.target_exposure == 0.0
    assert action.leverage_frac == 0.0
    assert action.sl_adjust_pct == 0.0
    assert action.tp_adjust_pct == 0.0
