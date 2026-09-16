"""Tests for the observation/action schema (CLAUDE.md §15, docs/RL_V8_PLAN.md).

Two classes of bug these exist to prevent, both of which actually happened:

1. A field is built in Go, sent over the wire, parsed by Pydantic — and never reaches the model's
   input vector. v5 found strategy signals in that state; v8's audit found the derived indicators
   there too, never populated at all since the field was created. Anything asserting a field
   REACHES the model guards that.

2. The vector silently changes width. v7's to_vector padded or truncated the feature block to fit
   whatever the loaded model wanted, so a 91-dim model against an 85-dim tail left a six-value
   feature budget and ten inputs vanished per call with no error anywhere. Every width here is
   asserted exactly, so adding a field without updating its constant fails a test instead.
"""
from __future__ import annotations

import numpy as np
import pytest

from rl_service.obs import (
    ACCOUNT_DIM,
    ACTION_DIM,
    BTC_BLOCK_DIM,
    CATEGORY_DIM,
    INDICATORS_PER_TIMEFRAME,
    MANAGE_HEAD_DIM,
    MARKET_BLOCK_DIM,
    OBSERVATION_DIM,
    OBSERVATION_SCHEMA_VERSION,
    POSITION_DIM,
    RETURNS_WINDOW,
    SIGNAL_DIM,
    STRATEGY_PROFILE_DIM,
    TERMINAL_CATEGORIES,
    TOKEN_PROFILE_DIM,
    ZERO_REWARD_CATEGORIES,
    BTCContext,
    MarketBlock,
    Observation,
    PositionState,
    SchemaError,
    StrategySignal,
    TokenProfile,
    account_block,
    btc_block,
    category_block,
    decode_action,
    mask_action_for_learning,
    market_block,
    position_block,
    signal_block,
    strategy_profile_block,
    to_vector,
    token_profile_block,
)


def _market(**overrides) -> MarketBlock:
    base = dict(
        bar="5m",
        indicators=[0.1 * (i + 1) for i in range(INDICATORS_PER_TIMEFRAME)],
        open=99.0,
        high=101.0,
        low=98.0,
        close=100.0,
        close_pct_changes=[0.001 * (i + 1) for i in range(RETURNS_WINDOW)],
        dist_to_swing_high_pct=0.02,
        dist_to_swing_low_pct=-0.03,
    )
    base.update(overrides)
    return MarketBlock(**base)


def _btc(**overrides) -> BTCContext:
    base = dict(
        open=64000.0,
        high=65500.0,
        low=63800.0,
        close=65000.0,
        close_pct_changes=[0.002 * (i + 1) for i in range(RETURNS_WINDOW)],
        dist_to_swing_high_pct=0.01,
        dist_to_swing_low_pct=-0.04,
        correlation=0.7,
    )
    base.update(overrides)
    return BTCContext(**base)


def _obs(**overrides) -> Observation:
    base = dict(
        inst_id="SOL",
        last_price=100.0,
        token_profile=TokenProfile(
            typical_volatility=0.012,
            log_volume_24h=8.5,
            volume_rank=0.8,
            log_price=2.0,
            range_24h=0.05,
            change_24h=0.02,
            log_trade_count=4.0,
        ),
        timeframes=[_market()],
        btc=_btc(),
        category="buy",
        signal=StrategySignal(
            strategy_id=7,
            side="buy",
            entry_px=100.0,
            sl_px=99.0,
            tp_px=102.0,
            kind="range_breakout_v2",
            bar="5m",
            win_rate=0.55,
            trade_count=40,
            avg_rr=2.0,
            avg_hold_hours=1.5,
            avg_pnl_per_trade=0.03,
        ),
        account_equity_usd=40.0,
        account_initial_usd=40.0,
        account_peak_usd=45.0,
        open_exposure_usd=5.0,
        open_leveraged_exposure_usd=50.0,
        open_position_count=2,
        max_position_pct=0.0625,
        max_leverage=10.0,
    )
    base.update(overrides)
    return Observation(**base)


# --- exact width -------------------------------------------------------------------------------


def test_observation_dim_matches_what_is_actually_built():
    """The check v7 did not have.

    v7's width came from the loaded model's observation_space, so the code adapted to the model
    rather than the other way round — and each time a field was added, the same number of inputs
    was silently dropped from the other end. Pinning the constant against a real build means adding
    a field without updating it fails here instead of in production.
    """
    assert to_vector(_obs()).shape == (1, OBSERVATION_DIM)


@pytest.mark.parametrize(
    "builder,want,name",
    [
        (token_profile_block, TOKEN_PROFILE_DIM, "token_profile"),
        (account_block, ACCOUNT_DIM, "account"),
        (category_block, CATEGORY_DIM, "category"),
        (strategy_profile_block, STRATEGY_PROFILE_DIM, "strategy_profile"),
        (signal_block, SIGNAL_DIM, "signal"),
        (position_block, POSITION_DIM, "position"),
        (market_block, MARKET_BLOCK_DIM, "market"),
        (btc_block, BTC_BLOCK_DIM, "btc"),
    ],
)
def test_every_block_is_exactly_its_declared_width(builder, want, name):
    assert builder(_obs()).shape[0] == want, name


def test_block_widths_sum_to_the_total():
    assert (
        TOKEN_PROFILE_DIM
        + ACCOUNT_DIM
        + CATEGORY_DIM
        + STRATEGY_PROFILE_DIM
        + SIGNAL_DIM
        + POSITION_DIM
        + MARKET_BLOCK_DIM
        + BTC_BLOCK_DIM
    ) == OBSERVATION_DIM


# --- no padding, no truncation, ever -------------------------------------------------------------


def test_short_returns_window_raises_rather_than_padding():
    """The v7 behaviour this replaces, and the operator's explicit requirement.

    A short candle window means the caller should SKIP the model call, not ask for a padded answer.
    A padded call still returns a well-formed action, so nothing downstream can tell that the model
    decided on partly-invented data.
    """
    obs = _obs(timeframes=[_market(close_pct_changes=[0.001, 0.002])])
    with pytest.raises(SchemaError, match="returns"):
        to_vector(obs)


def test_missing_indicators_raise_rather_than_padding():
    obs = _obs(timeframes=[_market(indicators=[0.1, 0.2])])
    with pytest.raises(SchemaError, match="indicators"):
        to_vector(obs)


def test_extra_returns_raise_rather_than_truncating():
    """Too much data is as much a mismatch as too little.

    Truncating is how v7 lost the live candle's OHLC — the array was longer than the budget and the
    excess was cut from the left, which happened to be the newest, most useful values.
    """
    obs = _obs(timeframes=[_market(close_pct_changes=[0.001] * (RETURNS_WINDOW + 3))])
    with pytest.raises(SchemaError, match="returns"):
        to_vector(obs)


def test_wrong_number_of_timeframe_blocks_raises():
    with pytest.raises(SchemaError, match="exactly 1 timeframe"):
        to_vector(_obs(timeframes=[_market(), _market()]))
    with pytest.raises(SchemaError, match="exactly 1 timeframe"):
        to_vector(_obs(timeframes=[]))


def test_missing_btc_returns_raise():
    """BTC is not optional. Every input except this one is intra-token, so a missing BTC block
    would leave the model blind to the market-wide move that drives most altcoin reversals."""
    with pytest.raises(SchemaError, match="btc"):
        to_vector(_obs(btc=_btc(close_pct_changes=[0.001])))


# --- fields reach the model ---------------------------------------------------------------------


def test_indicators_reach_the_model_input():
    """The v8 headline bug: ten indicators declared, parsed, and never in the vector.

    TimeframeBlock.features existed on both sides from 2026-08-26 and Go never wrote it once
    (`git log -S "tb.Features"` returns nothing), so the model had no RSI, no volatility and no
    volume for the entire life of the schema.
    """
    marker = 0.4242
    indicators = [0.0] * INDICATORS_PER_TIMEFRAME
    indicators[6] = marker  # rsi slot
    vec = to_vector(_obs(timeframes=[_market(indicators=indicators)]))[0]
    assert marker in vec


def test_live_candle_ohlc_reaches_the_model_input():
    """v7 built these four and truncated all four away, every call.

    They sit at the front of the feature array and to_vector cut from the left, so the LIVE forming
    candle — the freshest thing in the observation — was the first casualty.
    """
    base = to_vector(_obs())[0]
    moved = to_vector(_obs(timeframes=[_market(high=140.0)]))[0]
    assert not np.array_equal(base, moved)


def test_btc_moves_change_the_vector():
    """The operator's own observation: an altcoin reverses when BTC's candle turns red.

    Nothing in v7 could express that — every input was intra-token.
    """
    base = to_vector(_obs())[0]
    red = to_vector(_obs(btc=_btc(close_pct_changes=[-0.02] * RETURNS_WINDOW)))[0]
    assert not np.array_equal(base, red)


def test_btc_levels_are_relative_to_btc_not_to_the_token():
    """A BTC level divided by SOL's price is a number with no meaning.

    Regression guard: the obvious implementation reuses obs.last_price for every _rel call.
    """
    vec = btc_block(_obs())
    # BTC open 64000 against close 65000 is about -1.5%; against SOL's 100 it would be ~639.
    assert -0.05 < float(vec[0]) < 0.0


def test_token_profile_reaches_the_model_input():
    base = to_vector(_obs())[0]
    volatile = to_vector(
        _obs(token_profile=TokenProfile(typical_volatility=0.35, log_volume_24h=5.0))
    )[0]
    assert not np.array_equal(base, volatile)


def test_strategy_record_reaches_the_model_input():
    """Win rate and trade count are what replaced both the kind one-hot and the hardcoded
    confidence — if they did not reach the vector, strategy identity would carry nothing at all."""
    sig = _obs().signal
    good = to_vector(_obs(signal=sig.model_copy(update={"win_rate": 0.9})))[0]
    bad = to_vector(_obs(signal=sig.model_copy(update={"win_rate": 0.1})))[0]
    assert not np.array_equal(good, bad)


def test_absent_signal_is_distinguishable_from_a_zeroed_one():
    """`present` exists so a price-driven update cannot be confused with a signal of zero
    conviction — the model would otherwise learn from the ambiguity."""
    with_signal = signal_block(_obs(signal=StrategySignal(side="buy")))
    without = signal_block(_obs(signal=None, category="update"))
    assert with_signal[0] == 1.0
    assert without[0] == 0.0


# --- identity replaced by behaviour --------------------------------------------------------------


def test_no_ceiling_on_token_count():
    """The property the one-hot could not provide.

    65 tokens against 16 slots left most of them as indistinguishable zeros, and the roster grows
    on its own every 8 hours. A profile has no slots to run out of.
    """
    a = to_vector(_obs(inst_id="SOME_TOKEN_DISCOVERED_TOMORROW"))
    b = to_vector(_obs(inst_id="ANOTHER_ONE"))
    assert a.shape == b.shape == (1, OBSERVATION_DIM)


def test_two_tokens_with_the_same_profile_look_the_same():
    """The deliberate trade-off, asserted so it is a choice rather than a surprise.

    Identity is gone: two instruments with identical characteristics are identical to the model.
    That is the mechanism that lets experience transfer to a token discovered tomorrow, and the
    reason a per-token quirk can no longer be memorized.
    """
    p = TokenProfile(typical_volatility=0.02, log_volume_24h=7.0, log_price=1.0)
    assert np.array_equal(
        to_vector(_obs(inst_id="AAA", token_profile=p)),
        to_vector(_obs(inst_id="BBB", token_profile=p)),
    )


def test_timeframe_is_ordered_not_categorical():
    """5m < 15m < 1H is a real ordering that a one-hot destroys.

    With an ordered scalar the policy can learn "longer bar, wider stop" and interpolate to a
    timeframe it never saw; with 16 unordered slots it had to learn each in isolation.
    """
    sig = _obs().signal
    bars = ["5m", "15m", "1H", "4H"]
    vals = [
        float(strategy_profile_block(_obs(signal=sig.model_copy(update={"bar": b})))[5])
        for b in bars
    ]
    assert vals == sorted(vals)
    assert len(set(vals)) == len(bars)


# --- lifecycle categories -------------------------------------------------------------------------


def test_all_five_close_reasons_are_distinguishable():
    """§15.14 and §20 both had to force `timeout` and `manual` under `closed_early` because
    widening a one-hot meant a schema bump — collapsing 358 trades, a fifth of all closes, into one
    label covering three different things. The flag-plus-scalars encoding separates all five."""
    blocks = {
        c: tuple(category_block(_obs(category=c)).tolist())
        for c in ("closed_tp", "closed_sl", "closed_early", "closed_timeout", "closed_manual")
    }
    # tp and sl share an encoding by design — the direction is carried by realized PnL, not here.
    assert blocks["closed_tp"] == blocks["closed_sl"]
    distinct = {blocks["closed_tp"], blocks["closed_early"], blocks["closed_timeout"], blocks["closed_manual"]}
    assert len(distinct) == 4


def test_terminal_categories_set_the_terminal_flag():
    for c in TERMINAL_CATEGORIES:
        assert category_block(_obs(category=c))[3] == 1.0, c
    for c in ("buy", "sell", "update"):
        assert category_block(_obs(category=c))[3] == 0.0, c


def test_manual_close_is_marked_zero_reward():
    """§15.12: attributing an operator's action to the policy trains it on a decision it never
    made. The call still happens so the learner's pending decision resolves rather than leaking."""
    assert "closed_manual" in ZERO_REWARD_CATEGORIES
    assert "closed_sl" not in ZERO_REWARD_CATEGORIES


# --- action decoding ------------------------------------------------------------------------------


def _raw(open_head=0.0, manage=(0.0, 0.0, 0.0), sl=0.0, tp=0.0, size=0.0, lev=0.0):
    return np.array([sl, tp, size, lev, open_head, *manage], dtype=np.float32)


def test_open_head_sign_decides_open_or_skip():
    assert decode_action(_raw(open_head=0.5), _obs(category="buy")).action == "open"
    assert decode_action(_raw(open_head=-0.5), _obs(category="buy")).action == "skip"


def test_buy_call_can_never_return_a_manage_action():
    """§16.9: an unmasked head let a buy call answer `none`, so the caller had no open/skip answer
    and silently fell back to fixed sizing — the model looked uninvolved while being consulted every
    time. With split heads the manage head is not even read here."""
    a = decode_action(_raw(open_head=-1.0, manage=(9.0, 9.0, 9.0)), _obs(category="buy"))
    assert a.action == "skip"


def test_update_call_can_never_return_open_or_skip():
    a = decode_action(_raw(open_head=9.0, manage=(0.0, 0.0, 1.0)), _obs(category="update"))
    assert a.action == "close"


def test_manage_head_is_an_argmax():
    for idx, want in enumerate(("none", "update", "close")):
        manage = [0.0, 0.0, 0.0]
        manage[idx] = 1.0
        assert decode_action(_raw(manage=tuple(manage)), _obs(category="update")).action == want


def test_wrong_action_width_raises():
    with pytest.raises(SchemaError):
        decode_action(np.zeros(ACTION_DIM - 1, dtype=np.float32), _obs())


def test_size_is_a_fraction_of_the_allowed_budget():
    """Asking for 1.0 means "the most I am permitted", not the whole account — so a maximal request
    is the current fixed-sizing behaviour rather than an account-emptying one."""
    a = decode_action(_raw(size=1.0), _obs(max_position_pct=0.0625))
    assert a.size_pct == pytest.approx(0.0625)


def test_levels_are_prices_derived_from_the_live_price():
    a = decode_action(_raw(sl=-0.5, tp=0.5), _obs(last_price=100.0))
    assert a.sl_px < 100.0 < a.tp_px


def test_sell_call_mirrors_levels_to_the_short_s_profitable_side():
    """Found 2026-09-15: decode_action used to compute tp_px as last_price * (1 + tp_offset) with
    no reference to direction, so a positive tp_offset always landed ABOVE price — correct for a
    long, backwards for a short (a short's take-profit realizes a loss on touch if it sits above
    entry). Reproduced against a real BTC short whose tp_px landed 7.5% above entry. The SAME raw
    action that opens a favorable long here must open a favorable short: sl on the losing (higher)
    side, tp on the profitable (lower) side.
    """
    a = decode_action(_raw(sl=-0.5, tp=0.5), _obs(category="sell", last_price=100.0))
    assert a.tp_px < 100.0 < a.sl_px


def test_update_call_mirrors_levels_by_the_open_position_s_own_side():
    """An update call has no buy/sell category of its own — the position already open is what the
    levels must be coherent against, not the (possibly carried-forward, §15.12) signal."""
    short_position = PositionState(position_open=True, side=-1.0)
    a = decode_action(
        _raw(sl=-0.5, tp=0.5),
        _obs(category="update", last_price=100.0, position_state=short_position),
    )
    assert a.tp_px < 100.0 < a.sl_px


def test_update_call_with_a_long_position_keeps_the_long_convention():
    long_position = PositionState(position_open=True, side=1.0)
    a = decode_action(
        _raw(sl=-0.5, tp=0.5),
        _obs(category="update", last_price=100.0, position_state=long_position),
    )
    assert a.sl_px < 100.0 < a.tp_px


def test_flat_position_state_does_not_flip_the_encoding():
    """A synthetic/terminal observation with no position side recorded makes no direction claim —
    offsets pass through as the pre-fix behavior did, rather than guessing a side."""
    flat = PositionState(position_open=False, side=0.0)
    a = decode_action(
        _raw(sl=-0.5, tp=0.5),
        _obs(category="closed_tp", last_price=100.0, position_state=flat),
    )
    assert a.sl_px < 100.0 < a.tp_px


# --- learning mask ---------------------------------------------------------------------------------


def test_manage_head_takes_no_gradient_on_an_open_call():
    """The half of the split that actually fixes the problem.

    Splitting the heads aligns structure with the question; zeroing here is what stops reward
    flowing to an output that had no effect. An output that is rewarded without causing anything
    feels no corrective pressure and drifts to the tanh bound — the state §54.8 measured across all
    nine outputs.
    """
    raw = _raw(open_head=0.8, manage=(0.9, 0.9, 0.9))
    masked = mask_action_for_learning(raw, "buy")
    assert masked[4] == pytest.approx(0.8)
    assert np.all(masked[5 : 5 + MANAGE_HEAD_DIM] == 0.0)


def test_open_head_takes_no_gradient_on_an_update_call():
    raw = _raw(open_head=0.8, manage=(0.9, 0.1, 0.1))
    masked = mask_action_for_learning(raw, "update")
    assert masked[4] == 0.0
    assert masked[5] == pytest.approx(0.9)


def test_masking_leaves_the_continuous_outputs_alone():
    """sl/tp/size/leverage are acted on in every category that opens or manages, so they always
    earned their gradient."""
    raw = _raw(sl=0.3, tp=0.4, size=0.5, lev=0.6, open_head=0.7, manage=(0.8, 0.8, 0.8))
    masked = mask_action_for_learning(raw, "buy")
    assert masked[:4].tolist() == pytest.approx([0.3, 0.4, 0.5, 0.6])


def test_masking_does_not_mutate_the_caller_s_array():
    raw = _raw(open_head=0.8, manage=(0.9, 0.9, 0.9))
    mask_action_for_learning(raw, "buy")
    assert raw[5] == pytest.approx(0.9)


def test_schema_version_is_v8():
    assert OBSERVATION_SCHEMA_VERSION == 8
