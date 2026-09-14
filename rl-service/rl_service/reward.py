"""The one reward function, used by both the live learner and the backtest (docs/RL_V8_PLAN.md).

WHY THIS FILE EXISTS. Before v8 there were two reward functions that disagreed:

    learner.py (live)                 ReplayEnv (training)
    pnl / position_size               (pnl - fees) / initial_equity
    no fees                           fees charged
    no churn penalty                  churn penalty
    no drawdown penalty               drawdown penalty
    no liquidation penalty            liquidation penalty

Two separate problems followed. First, a scale mismatch of roughly 260x between what a model was
trained on and what it was then served — the §19.1 skew class, where the same field means different
things on two paths. Second and worse: PRODUCTION HAD NO RISK PENALTIES AT ALL. §15.13 designed
them carefully and they lived only in an env that is off by default (§15.8), so on the live path
leverage was free and moving a stop was free. That is consistent with what §54.9 observed — the
model walked stops to within 0.168% of entry, and nothing in the reward objected.

DESIGN, in the order the terms matter:

1. RISK-ADJUSTED RETURN, not return on capital. `pnl / size` cannot tell a 5% gain made with a 1%
   stop from a 5% gain made with a 15% stop, though the second took three times the risk for the
   same result. Dividing by the risk actually taken is what makes the model prefer the first — and
   it is the single change most likely to alter behaviour, because every trade is scored by what it
   risked rather than by what it committed.

2. Penalties are charged in the same units as the return, so they can be reasoned about. Measured
   from 2084 real closed trades: median |return| 5.1%, mean 6.6%, p90 15.0%; mean risk taken 6.6%
   of margin. The weights below are set against those numbers rather than carried over from an env
   with a different denominator — under the old account-normalized reward a typical trade scored
   0.000038 while the drawdown penalty charged 0.025, i.e. the penalty was 650x the reward and the
   PnL term was effectively invisible.
"""
from __future__ import annotations

from dataclasses import dataclass

# --- calibration ---------------------------------------------------------------------------------
#
# TYPICAL_RETURN anchors every weight below: it is the median absolute risk-adjusted return of a
# real closed trade, so a penalty weight of W costs "W times a typical trade" at full strength.
# Measured 2026-09-14 over 2084 closed paper_orders. Re-measure if the strategy mix changes
# materially; the weights are expressed against it so they move together.
TYPICAL_RETURN = 0.05

# Churn: moving SL/TP must earn its keep rather than being free to twitch every tick (§15.5). At
# full magnitude a single adjustment costs ~8% of a typical trade — enough that pointless movement
# accumulates into a real cost, small enough that a genuinely useful adjustment still pays.
CHURN_WEIGHT = 0.004

# Leverage: this is the term that makes leverage ITSELF expensive. The drawdown term only charges
# for losses already taken, so without this the policy could hold maximum leverage indefinitely at
# no cost right up until it blew up (§15.13). Charged on proximity to liquidation rather than on
# leverage directly, so it is free below the buffer floor and ramps steeply above it: at 10x a
# position sits right at the floor and pays nothing, which is deliberate — 10x is the OKX cap this
# project trades at, and penalizing the only leverage available would be a constant, not a signal.
LEVERAGE_WEIGHT = 0.6
# Distance to liquidation, as a fraction of price, at or beyond which leverage is free. 10% is one
# 10x position; the penalty ramps to full weight as that distance closes to zero.
LIQ_BUFFER_FLOOR = 0.10

# Drawdown: charges for distance below the account's high-water mark, which is what makes a round
# trip cost something. Reward is otherwise computed per trade, so running the account up and giving
# it all back collects the gains and pays the losses and nets to ~0 — reading as no worse than
# never having traded (§15.13). At a 20% drawdown this costs ~1.2 typical trades.
DRAWDOWN_WEIGHT = 0.3

# A single trade's reward is clipped to this. A stop is capped at 15% of margin (§19.2) so a loss
# cannot legitimately exceed ~1.0 here, but a data error, a gap, or a mis-recorded fill can produce
# an arbitrary number — and one outlier in a small replay buffer distorts every sample drawn from
# it afterwards. Clipping bounds that blast radius without hiding the sign or the magnitude of any
# realistic trade.
REWARD_CLIP = 3.0


@dataclass(frozen=True)
class RewardBreakdown:
    """Every term, kept separately.

    A falling reward caused by risk and one caused by bad entries need completely different fixes,
    and an aggregate number cannot tell them apart (§15.13). The backtest reports these per run.
    """

    total: float
    ret: float
    churn_penalty: float
    leverage_penalty: float
    drawdown_penalty: float

    def as_dict(self) -> dict:
        return {
            "reward": self.total,
            "return": self.ret,
            "churn_penalty": self.churn_penalty,
            "leverage_penalty": self.leverage_penalty,
            "drawdown_penalty": self.drawdown_penalty,
        }


def liquidation_penalty(leverage: float) -> float:
    """Penalty in [0, 1] for how close a position sits to liquidation.

    The estimate depends only on leverage: at Nx an adverse move of roughly 1/N wipes the position
    out. Approximate by design — it ignores maintenance-margin tiers — matching the same
    conservative approximation go-engine's risk manager uses (§27.2).
    """
    if leverage <= 0:
        return 0.0
    buffer = 1.0 / leverage
    if buffer >= LIQ_BUFFER_FLOOR:
        return 0.0
    return (LIQ_BUFFER_FLOOR - buffer) / LIQ_BUFFER_FLOOR


def drawdown_pct(equity: float, peak_equity: float) -> float:
    """How far below its high-water mark the account is, in [0, 1]."""
    if peak_equity <= 0:
        return 0.0
    return max(0.0, (peak_equity - equity) / peak_equity)


def trade_reward(
    realized_pnl_usd: float,
    fees_usd: float = 0.0,
    risk_pct: float = 0.0,
    position_size_usd: float = 0.0,
    leverage: float = 0.0,
    equity_usd: float = 0.0,
    peak_equity_usd: float = 0.0,
    sltp_adjustments: float = 0.0,
    zero_reward: bool = False,
) -> RewardBreakdown:
    """Scores one closed trade.

    `zero_reward` is for an operator's manual close (§15.12): the call still happens so the
    learner's pending decision resolves rather than leaking, but no gradient follows a decision the
    policy did not make.

    `risk_pct` is the entry-to-stop distance as a fraction of margin — what the trade actually put
    at risk. It falls back to position size when unknown (historical rows, or a position opened with
    no stop), which reduces this to return on capital: worse, but defined, and better than scoring
    zero.
    """
    if zero_reward:
        return RewardBreakdown(0.0, 0.0, 0.0, 0.0, 0.0)

    net_pnl = float(realized_pnl_usd) - abs(float(fees_usd))

    # Risk-adjusted: how much did this trade make relative to what it was willing to lose.
    risk = float(risk_pct or 0.0)
    size = float(position_size_usd or 0.0)
    if risk > 0 and size > 0:
        denominator = risk * size
    elif size > 0:
        denominator = size
    else:
        return RewardBreakdown(0.0, 0.0, 0.0, 0.0, 0.0)

    ret = net_pnl / denominator

    churn = CHURN_WEIGHT * max(0.0, float(sltp_adjustments))
    lev = LEVERAGE_WEIGHT * liquidation_penalty(float(leverage or 0.0))
    dd = DRAWDOWN_WEIGHT * drawdown_pct(float(equity_usd or 0.0), float(peak_equity_usd or 0.0))

    total = ret - churn - lev - dd
    total = max(-REWARD_CLIP, min(REWARD_CLIP, total))
    return RewardBreakdown(total, ret, churn, lev, dd)
