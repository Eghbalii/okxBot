"""Tests for the observation/action schema (CLAUDE.md §15.4, §15.10).

The bug these exist to prevent: strategy signals were built in Go, sent over the wire, and parsed
by Pydantic — but never entered the model's input vector, so the policy was asked to reason about
opinions it could not see. `recent_trades` was dropped the same way. Anything asserting that a
field REACHES the model is guarding that class of silent failure.
"""
from __future__ import annotations

import numpy as np
import pytest

from rl_service.obs import (
    ACTION_DIM,
    ACTIONS,
    MAX_SLTP_OFFSET_PCT,
    MAX_STRATEGY_KIND_SLOTS,
    MAX_TIMEFRAME_SLOTS,
    SIGNAL_CATEGORIES,
    STRATEGY_KINDS,
    Observation,
    PositionState,
    PriceContext,
    StrategySignal,
    TimeframeBlock,
    decode_action,
    flat_action,
    observation_features,
    observation_tail,
    position_block,
    signal_block,
    to_vector,
)


def _obs(**overrides):
    base = dict(
        inst_id="BTC-USDT-SWAP",
        active_tokens=["BTC-USDT-SWAP", "XAU-USD-SWAP"],
        last_price=100.0,
        category="buy",
        signal=StrategySignal(
            strategy_id=1, side="buy", confidence=0.8, entry_px=99.0, sl_px=95.0, tp_px=110.0,
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



def test_prices_are_relative_to_live_price():
    """Entry/SL/TP are fed as fractions of live price, never raw levels.

    One shared policy serves BTC at ~65000 and other tokens at ~0.15; raw levels would not
    generalize across them.
    """
    cheap = signal_block(_obs(last_price=100.0, signal=StrategySignal(
        strategy_id=1, side="buy", kind="rsi_sma", bar="5m",
        entry_px=99.0, sl_px=95.0, tp_px=110.0)))
    pricey = signal_block(_obs(last_price=65000.0, signal=StrategySignal(
        strategy_id=1, side="buy", kind="rsi_sma", bar="5m",
        entry_px=64350.0, sl_px=61750.0, tp_px=71500.0)))
    assert np.allclose(cheap, pricey, atol=1e-6), "same relative levels must vectorize identically"




# --- action decoding ----------------------------------------------------------------------------

def test_decodes_scalars_and_action():
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    raw[0] = -0.5   # sl offset -> below live price
    raw[1] = 1.0    # tp offset -> full positive
    raw[2] = 0.3    # size_pct
    raw[3] = 0.25   # leverage_frac
    raw[4 + ACTIONS.index("close")] = 1.0

    # "close" is only legal on an update call — decode_action masks the action head to the
    # category's legal set, so the category has to match the action being asserted.
    action = decode_action(raw, _obs(last_price=100.0, category="update"))

    # The model chooses a DISTANCE; decode turns it into a real price level.
    # float32 round-trip, so compare approximately rather than exactly.
    assert action.sl_px == pytest.approx(100.0 * (1 - 0.5 * MAX_SLTP_OFFSET_PCT), rel=1e-6)
    assert action.tp_px == pytest.approx(100.0 * (1 + MAX_SLTP_OFFSET_PCT), rel=1e-6)
    assert action.size_pct == pytest.approx(0.3, rel=1e-6)
    assert action.leverage_frac == pytest.approx(0.25, rel=1e-6)
    assert action.action == "close"

def test_action_is_argmax_so_exactly_one_is_chosen():
    # Each action is asserted under a category where it is legal (see the masking tests below).
    for want, category in [
        ("open", "buy"),
        ("skip", "sell"),
        ("none", "update"),
        ("update", "update"),
        ("close", "update"),
    ]:
        raw = np.zeros(ACTION_DIM, dtype=np.float32)
        raw[4 + ACTIONS.index(want)] = 1.0
        assert decode_action(raw, _obs(category=category)).action == want


# --- action masking by category ----------------------------------------------------------------
#
# Regression coverage for a production bug (2026-08-29): the argmax ran over ALL five actions
# regardless of category, so a buy/sell call could answer "none" — not a decision about opening at
# all. openDecision then had no open/skip answer to act on and fell through to fixed sizing without
# a log line or metric, which made the model look like it was never consulted when in fact it was
# being asked on every single signal and answering unusably every time.

@pytest.mark.parametrize("category", ["buy", "sell"])
@pytest.mark.parametrize("forced", ["none", "update", "close"])
def test_open_categories_never_return_an_update_action(category, forced):
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    raw[4 + ACTIONS.index(forced)] = 1.0  # policy strongly prefers an illegal action
    action = decode_action(raw, _obs(category=category)).action
    assert action in ("open", "skip"), f"{category} returned {action!r}"


@pytest.mark.parametrize("forced", ["open", "skip"])
def test_update_category_never_returns_an_open_action(forced):
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    raw[4 + ACTIONS.index(forced)] = 1.0
    action = decode_action(raw, _obs(category="update")).action
    assert action in ("none", "update", "close"), f"update returned {action!r}"


def test_masking_still_prefers_the_highest_legal_logit():
    """Masking must pick the best LEGAL action, not just any legal one — otherwise the policy's
    preference between open and skip would be discarded along with the illegal options."""
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    raw[4 + ACTIONS.index("none")] = 5.0   # illegal on buy, and the global argmax
    raw[4 + ACTIONS.index("skip")] = 2.0   # legal, and the better of the two legal options
    raw[4 + ACTIONS.index("open")] = 1.0
    assert decode_action(raw, _obs(category="buy")).action == "skip"


def test_action_width_does_not_scale_with_strategy_count():
    # The property that lets the roster change without retraining (§15.10).
    assert ACTION_DIM == 4 + len(ACTIONS)


def test_out_of_range_outputs_are_clipped():
    raw = np.zeros(ACTION_DIM, dtype=np.float32)
    raw[0] = 10.0   # sl offset far past its bound
    raw[2] = 5.0    # size_pct past 1
    raw[3] = -2.0   # leverage_frac below 0

    action = decode_action(raw, _obs(last_price=100.0))

    assert action.sl_px == pytest.approx(100.0 * (1 + MAX_SLTP_OFFSET_PCT), rel=1e-6)
    assert action.size_pct == 1.0
    assert action.leverage_frac == 0.0

def test_short_action_vector_is_rejected():
    # Guards against serving a model trained on an older, narrower action space.
    try:
        decode_action(np.zeros(2, dtype=np.float32), _obs())
        assert False, "expected ValueError for a too-short action vector"
    except ValueError:
        pass


def test_flat_action_takes_no_trade():
    action = flat_action()
    assert action.action == "skip"
    assert action.size_pct == 0.0
    assert action.leverage_frac == 0.0


def test_categories_and_order_actions_are_stable_vocabularies():
    # Order defines one-hot/argmax indices; reordering silently reassigns meaning for a trained
    # model, so these lists are effectively part of the schema version.
    assert SIGNAL_CATEGORIES[:3] == ["buy", "sell", "update"]
    assert ACTIONS == ["open", "skip", "none", "update", "close"]


def test_null_lists_from_go_are_accepted():
    """Go marshals a nil slice as JSON `null`, not `[]`.

    An observation with no recent trades — every one on a fresh install — would otherwise fail
    validation and surface to the caller as a schema mismatch. Same null-vs-[] class of bug
    CLAUDE.md §14 records hitting on the panel side.
    """
    raw = (
        '{"schema_version": 6, "inst_id": "BTC-USDT-SWAP", "active_tokens": ["BTC-USDT-SWAP"],'
        ' "last_price": "100.5", "timeframes": null, "features": null, "category": "update"}'
    )
    obs = Observation.model_validate_json(raw)
    assert obs.timeframes == []
    assert obs.features == []
    # And it must still vectorize rather than blowing up downstream.
    assert len(observation_tail(obs)) > 0


def test_decimal_strings_from_go_are_coerced():
    """decimal.Decimal marshals as a JSON string; every numeric field must accept that form."""
    raw = (
        '{"schema_version": 6, "inst_id": "BTC-USDT-SWAP", "active_tokens": ["BTC-USDT-SWAP"],'
        ' "last_price": "100.5", "category": "buy",'
        ' "signal": {"strategy_id": 1, "side": "buy", "confidence": "0.8", "entry_px": "99",'
        '            "sl_px": "95", "tp_px": "110", "kind": "rsi_sma", "bar": "5m",'
        '            "win_rate": "0.62", "trade_count": 40},'
        ' "position_state": {"position_open": true, "size_usd": "25", "leverage": "10"},'
        ' "account_equity_usd": "100"}'
    )
    obs = Observation.model_validate_json(raw)
    assert obs.signal is not None and obs.signal.confidence == 0.8
    assert obs.signal.sl_px == 95.0
    assert obs.position_state.size_usd == 25.0
    assert obs.last_price == 100.5


def test_nested_null_lists_from_go_are_accepted():
    """Go marshals nil slices as null at EVERY level, not just the top.

    A timeframe block with no strategy signals -- the normal case on a quiet bar -- would otherwise
    fail validation and surface to the caller as a schema mismatch. Caught by round-tripping Go's
    real JSON rather than a hand-written sample.
    """
    raw = (
        '{"schema_version": 6, "inst_id": "BTC-USDT-SWAP", "active_tokens": ["BTC-USDT-SWAP"],'
        ' "last_price": "100.5", "category": "update",'
        ' "timeframes": [{"bar": "5m", "strategy_signals": null, "features": null,'
        '                 "price_context": {"open": "99", "close": "100",'
        '                                   "close_pct_changes": null}}]}'
    )
    obs = Observation.model_validate_json(raw)
    assert obs.timeframes[0].strategy_signals == []
    assert obs.timeframes[0].features == []
    assert obs.timeframes[0].price_context.close_pct_changes == []
    # And it must still vectorize rather than blowing up downstream.
    assert len(observation_features(obs)) > 0


def test_live_candle_ohlc_reaches_the_model():
    """The forming candle's OHLC is fed relative to live price (CLAUDE.md §15.11) -- on a 1H bar the
    last CLOSED candle can be 59 minutes stale."""
    flat = observation_features(_obs(timeframes=[TimeframeBlock(
        bar="5m", price_context=PriceContext(open=100.0, high=100.0, low=100.0, close=100.0))]))
    ranging = observation_features(_obs(timeframes=[TimeframeBlock(
        bar="5m", price_context=PriceContext(open=98.0, high=103.0, low=97.0, close=101.0))]))
    assert not np.array_equal(flat, ranging), "candle shape must change the model's input"


def test_pnl_extremes_reach_the_model():
    """A trade that ran to +8% and came back must look different from one that drifted sideways."""
    round_tripped = observation_tail(_obs(position_state=PositionState(
        position_open=1.0, unrealized_pnl_pct=0.01, pnl_max_pct=0.08, pnl_min_pct=-0.01)))
    drifted = observation_tail(_obs(position_state=PositionState(
        position_open=1.0, unrealized_pnl_pct=0.01, pnl_max_pct=0.01, pnl_min_pct=0.0)))
    assert not np.array_equal(round_tripped, drifted)


# --- to_vector: pad/truncate must always land exactly on expected_dim -------------------------
#
# Regression coverage for a real bug found 2026-08-29: a model built from a probe Observation with
# NO timeframe blocks got obs_dim == len(tail) exactly, so padded_len (expected_dim - len(tail))
# was 0. features[-padded_len:] with padded_len == 0 is a Python/NumPy footgun — arr[-0:] returns
# the WHOLE array, not an empty one, since -0 == 0 and arr[0:] is a full-array slice. So instead of
# truncating features to nothing, the old code left them untouched, overshooting expected_dim and
# raising "observation vector shape mismatch" on every real /predict call once any timeframe
# block's price_context contributed even one feature. Never caught because to_vector had zero
# direct test coverage before this.

def test_to_vector_truncates_to_zero_feature_budget():
    """expected_dim == len(tail) exactly (no room for ANY features) must produce a vector of
    exactly expected_dim, not overshoot it. This is the exact shape that crashed in production."""
    obs = _obs()
    tail_len = len(observation_tail(obs))
    vec = to_vector(obs, expected_dim=tail_len)
    assert vec.shape == (1, tail_len)


def test_to_vector_pads_a_short_feature_vector():
    obs = _obs(timeframes=[])  # no timeframe blocks -> observation_features is empty
    tail_len = len(observation_tail(obs))
    vec = to_vector(obs, expected_dim=tail_len + 10)
    assert vec.shape == (1, tail_len + 10)


def test_to_vector_truncates_an_oversized_feature_vector():
    obs = _obs()  # has real timeframe features, well over a 1-feature budget
    tail_len = len(observation_tail(obs))
    vec = to_vector(obs, expected_dim=tail_len + 1)
    assert vec.shape == (1, tail_len + 1)


def test_to_vector_matches_expected_dim_across_a_range_of_budgets():
    """Sweeps expected_dim across and past the natural feature length, including the exact
    zero-budget boundary that the bug lived at — every one of these must land exactly on
    expected_dim, never over or under."""
    obs = _obs()
    tail_len = len(observation_tail(obs))
    natural_features_len = len(observation_features(obs))
    for offset in range(-2, 5):
        expected_dim = tail_len + max(natural_features_len + offset, 0)
        vec = to_vector(obs, expected_dim=expected_dim)
        assert vec.shape == (1, expected_dim), f"failed at offset={offset}"
