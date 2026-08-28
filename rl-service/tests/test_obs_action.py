"""Tests for the observation/action schema (CLAUDE.md §15.4, §15.10).

The bug these exist to prevent: strategy signals were built in Go, sent over the wire, and parsed
by Pydantic — but never entered the model's input vector, so the policy was asked to reason about
opinions it could not see. `recent_trades` was dropped the same way. Anything asserting that a
field REACHES the model is guarding that class of silent failure.
"""
from __future__ import annotations

import numpy as np

from rl_service.obs import (
    ACTION_DIM,
    MAX_SLTP_ADJUST_PCT,
    ORDER_ACTIONS,
    SIGNAL_CATEGORIES,
    STRATEGY_KINDS,
    Action,
    MarketContext,
    Observation,
    PositionState,
    PriceContext,
    RecentTrade,
    StrategySignal,
    TimeframeBlock,
    decode_action,
    flat_action,
    observation_tail,
    recent_trades_block,
    signal_block,
)


def _obs(**overrides):
    base = dict(
        inst_id="BTC-USDT-SWAP",
        active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"],
        last_price=100.0,
        category="buy",
        signal=StrategySignal(
            strategy_id=1, side="buy", confidence=0.8, sl_pct=0.01, tp_pct=0.02,
            kind="rsi_sma", bar="5m", win_rate=0.62, trade_count=40.0,
        ),
        account_equity_usd=100.0,
        account_initial_usd=100.0,
        timeframes=[TimeframeBlock(bar="5m", features=[0.1] * 10,
                                   price_context=PriceContext(close_pct_changes=[0.001] * 10))],
    )
    base.update(overrides)
    return Observation(**base)


# --- the bug this redesign exists to fix -------------------------------------------------------

def test_strategy_signal_reaches_the_model_input():
    """Two observations differing ONLY in the signal must produce different input vectors.

    Before §15.10 they produced identical vectors: the signal never reached the model at all.
    """
    buy = observation_tail(_obs(signal=StrategySignal(strategy_id=1, side="buy", confidence=0.9,
                                                      kind="rsi_sma", bar="5m")))
    sell = observation_tail(_obs(signal=StrategySignal(strategy_id=1, side="sell", confidence=0.9,
                                                       kind="rsi_sma", bar="5m")))
    assert not np.array_equal(buy, sell), "signal side must change the model's input"


def test_strategy_kind_reaches_the_model_input():
    # One shared policy serves every strategy, so it has to be able to tell them apart.
    a = observation_tail(_obs(signal=StrategySignal(strategy_id=1, side="buy", kind="rsi_sma", bar="5m")))
    b = observation_tail(_obs(signal=StrategySignal(strategy_id=1, side="buy", kind="macd_cross", bar="5m")))
    assert not np.array_equal(a, b)


def test_timeframe_reaches_the_model_input():
    a = observation_tail(_obs(signal=StrategySignal(strategy_id=1, side="buy", kind="rsi_sma", bar="5m")))
    b = observation_tail(_obs(signal=StrategySignal(strategy_id=1, side="buy", kind="rsi_sma", bar="1H")))
    assert not np.array_equal(a, b)


def test_recent_trades_reach_the_model_input():
    # §15.3 specified this tail so the agent can size down after a losing streak; it was being sent
    # and then dropped, exactly like the strategy signals.
    none = observation_tail(_obs(recent_trades=[]))
    losses = observation_tail(_obs(recent_trades=[RecentTrade(realized_pnl_usd=-5.0, win=False)] * 3))
    assert not np.array_equal(none, losses)


def test_category_reaches_the_model_input():
    # The category is what tells the model which decision it is being asked to make.
    opening = observation_tail(_obs(category="buy"))
    managing = observation_tail(_obs(category="update"))
    assert not np.array_equal(opening, managing)


# --- signal presence must be unambiguous -------------------------------------------------------

def test_absent_signal_is_distinguishable_from_zero_confidence():
    """A price-driven update (no strategy spoke) must not look like a signal saying zero.

    Without an explicit `present` flag the model would learn from that ambiguity.
    """
    absent = signal_block(_obs(signal=None))
    zero = signal_block(_obs(signal=StrategySignal(strategy_id=1, side="", confidence=0.0,
                                                   kind="rsi_sma", bar="5m")))
    assert not np.array_equal(absent, zero)


def test_signal_block_is_fixed_width_regardless_of_strategy_count():
    # One signal per call is what removes the ceiling on roster size (§15.10): the vector width
    # cannot depend on how many strategies happen to be registered or firing.
    widths = {len(signal_block(_obs(signal=StrategySignal(strategy_id=i, side="buy", kind=k, bar="5m"))))
              for i, k in enumerate(STRATEGY_KINDS)}
    assert len(widths) == 1, f"signal block width varies by strategy: {widths}"


def test_unknown_strategy_kind_does_not_crash():
    # A newly-registered kind the model was not trained on must degrade to "unrecognized", not raise.
    block = signal_block(_obs(signal=StrategySignal(strategy_id=1, side="buy",
                                                    kind="not_a_registered_kind", bar="5m")))
    assert len(block) == len(signal_block(_obs()))


def test_recent_trades_block_is_fixed_width():
    for n in (0, 1, 5, 25):
        trades = [RecentTrade(realized_pnl_usd=1.0, win=True)] * n
        assert len(recent_trades_block(_obs(recent_trades=trades))) == 20


def test_position_prices_are_relative_to_live_price():
    """Entry/SL/TP are fed as fractions of live price, never raw levels.

    One shared policy serves BTC at ~65000 and other tokens at ~0.15; raw levels would not
    generalize across them.
    """
    cheap = signal_block(_obs(last_price=100.0, position_state=PositionState(
        position_open=1.0, entry_px=98.0, sl_px=95.0, tp_px=110.0)))
    pricey = signal_block(_obs(last_price=65000.0, position_state=PositionState(
        position_open=1.0, entry_px=63700.0, sl_px=61750.0, tp_px=71500.0)))
    assert np.allclose(cheap, pricey, atol=1e-6), "same relative position must vectorize identically"


# --- action decoding ----------------------------------------------------------------------------

def test_decodes_scalars_and_order_action():
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    raw[0] = 0.5     # target_exposure
    raw[1] = 0.25    # leverage_frac
    raw[2] = 1.0     # sl_adjust -> full positive
    raw[3] = -0.5    # tp_adjust
    raw[4 + ORDER_ACTIONS.index("close")] = 1.0

    action = decode_action(raw, _obs())

    assert action.target_exposure == 0.5
    assert action.leverage_frac == 0.25
    assert action.sl_adjust_pct == MAX_SLTP_ADJUST_PCT
    assert action.tp_adjust_pct == -0.5 * MAX_SLTP_ADJUST_PCT
    assert action.order_action == "close"


def test_order_action_is_argmax_so_exactly_one_is_chosen():
    for want in ORDER_ACTIONS:
        raw = np.zeros(ACTION_DIM, dtype=np.float32)
        raw[4 + ORDER_ACTIONS.index(want)] = 1.0
        assert decode_action(raw, _obs()).order_action == want


def test_action_width_does_not_scale_with_strategy_count():
    # The property that lets the roster change without retraining (§15.10).
    assert ACTION_DIM == 4 + len(ORDER_ACTIONS)


def test_out_of_range_outputs_are_clipped():
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    raw[0] = 5.0
    raw[1] = -2.0
    raw[2] = 10.0

    action = decode_action(raw, _obs())

    assert action.target_exposure == 1.0
    assert action.leverage_frac == 0.0
    assert action.sl_adjust_pct == MAX_SLTP_ADJUST_PCT


def test_short_action_vector_is_rejected():
    # Guards against serving a model trained on an older, narrower action space.
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
    assert action.order_action == "none"


def test_categories_and_order_actions_are_stable_vocabularies():
    # Order defines one-hot/argmax indices; reordering silently reassigns meaning for a trained
    # model, so these lists are effectively part of the schema version.
    assert SIGNAL_CATEGORIES[:3] == ["buy", "sell", "update"]
    assert ORDER_ACTIONS == ["none", "adjust", "close"]


def test_null_lists_from_go_are_accepted():
    """Go marshals a nil slice as JSON `null`, not `[]`.

    An observation with no recent trades — every one on a fresh install — would otherwise fail
    validation and surface to the caller as a schema mismatch. Same null-vs-[] class of bug
    CLAUDE.md §14 records hitting on the panel side.
    """
    raw = (
        '{"schema_version": 5, "inst_id": "BTC-USDT-SWAP", "active_tokens": ["BTC-USDT-SWAP"],'
        ' "last_price": "100.5", "timeframes": null, "recent_trades": null, "features": null,'
        ' "category": "update"}'
    )
    obs = Observation.model_validate_json(raw)
    assert obs.timeframes == []
    assert obs.recent_trades == []
    assert obs.features == []
    # And it must still vectorize rather than blowing up downstream.
    assert len(observation_tail(obs)) > 0


def test_decimal_strings_from_go_are_coerced():
    """decimal.Decimal marshals as a JSON string; every numeric field must accept that form."""
    raw = (
        '{"schema_version": 5, "inst_id": "BTC-USDT-SWAP", "active_tokens": ["BTC-USDT-SWAP"],'
        ' "last_price": "100.5", "category": "buy",'
        ' "signal": {"strategy_id": 1, "side": "buy", "confidence": "0.8", "sl_pct": "0.01",'
        '            "tp_pct": "0.02", "kind": "rsi_sma", "bar": "5m", "win_rate": "0.62",'
        '            "trade_count": 40, "age_seconds": 120},'
        ' "position_state": {"position_open": true, "entry_px": "98", "sl_px": "95"},'
        ' "account_equity_usd": "100"}'
    )
    obs = Observation.model_validate_json(raw)
    assert obs.signal is not None and obs.signal.confidence == 0.8
    assert obs.position_state.entry_px == 98.0
    assert obs.last_price == 100.5
