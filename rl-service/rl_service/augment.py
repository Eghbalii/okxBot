"""Sizing/leverage augmentation for offline pre-training (2026-09-05).

The problem this solves
-----------------------
`pretrain.py` escaped the all-skip deadlock but produced a policy whose requested position size
climbed monotonically to ~69% of equity, with no sign of levelling off. That is not a training
failure — it is the direct consequence of training data with no action variance. Measured on the
649 trainable closed trades:

    leverage 10x : 529 trades (82%)
    leverage 20x :  80
    leverage  1x :  39
    size         : effectively one value per era (rl_sizing has never been on)

A policy cannot learn "how much to commit" from data where that quantity never varies: it never
sees a large position lose more, so nothing pushes back on asking for more.

What this does
--------------
Re-derives each real trade at several (size, leverage) settings. This is arithmetic on a trade that
really happened, not simulation: entry and close prices are both recorded, so

    pnl = (close - entry)/entry * direction * size * leverage

is exact. Verified against 8 sampled rows before this was written — recomputation matched the
stored realized_pnl to 6 decimal places on every one.

What this deliberately does NOT do
----------------------------------
Entry, SL and TP are never altered, though doing so would produce far more variety. Moving the
entry changes WHETHER and WHEN the trade would have hit its stop or target, and the recorded close
price answers that question only for the entry that actually happened. A trade re-priced at a
different entry is a guess about an outcome, not a record of one — the model would be learning a
market that never existed. The size/leverage rescaling above is safe precisely because it changes
the magnitude of a known outcome without changing whether that outcome occurred.

Constraints applied (real limits, not invented ones)
----------------------------------------------------
- leverage <= 10x: the exchange's own cap on this account (CLAUDE.md §26).
- position <= account.max_position_pct (0.25) of equity, and the whole augmented set stays under
  max_total_exposure_pct (0.60) — training the policy to want sizes Go would clamp away teaches it
  to fight its own risk layer.
- realized loss capped at 15% of margin (CLAUDE.md §19.2), because in reality a stop enforces that.
  Without it, higher leverage would look like free upside in this data: the linear multiply doubles
  gains and losses alike, but never models liquidation, so nothing would discourage max leverage.
"""
from __future__ import annotations

import random
from dataclasses import dataclass, replace
from typing import Optional

from rl_service.pretrain import Trade

# The exchange's cap on this account (CLAUDE.md §26). Not a preference — orders above it are
# rejected outright, so training above it teaches the policy to ask for the impossible.
MAX_LEVERAGE = 10.0

# The per-token even share of the account: PaperTrader.dynamicNotional sizes every fixed-sizing
# order at equity/ActiveTokenCount, so with the current 10-token roster one position is expected to
# take 10% of the account. This is the budget observation schema v7 hands the model as
# max_position_pct, and the value size_pct is a fraction OF — not account.max_position_pct, which
# remains a separate hard ceiling applied afterwards in sizeFromModelAction.
ACTIVE_TOKEN_COUNT = 10
MAX_POSITION_PCT = 1.0 / ACTIVE_TOKEN_COUNT

# CLAUDE.md §19.2: a stop bounds realized loss to this fraction of margin regardless of leverage.
# Scoped to the POSITION's own margin, not the account: this deployment runs isolated margin, where
# a position cannot lose more than the margin backing it, so an account-relative cap would model a
# risk that does not exist (and would imply ~7 consecutive stops could zero the account).
MAX_LOSS_PCT = 0.15

# Sampled per variant rather than fixed, so the policy sees a continuum instead of a few spikes it
# could memorise as categories. Sizes span the full budget from near-zero to the whole even share,
# which is exactly the [0, 1] range the v7 action's size_pct now addresses.
SIZE_PCT_RANGE = (0.005, MAX_POSITION_PCT)
LEVERAGE_RANGE = (1.0, MAX_LEVERAGE)


@dataclass
class AugmentStats:
    originals: int = 0
    generated: int = 0
    loss_capped: int = 0
    skipped: int = 0

    def summary(self) -> str:
        return (
            f"{self.originals} real trades -> {self.generated} training samples "
            f"({self.loss_capped} loss-capped, {self.skipped} skipped)"
        )


def price_return(t: Trade) -> Optional[float]:
    """Signed fractional price move of the trade, or None when it cannot be derived.

    This is the one quantity carried over from reality; everything else about a variant is
    recomputed from it.
    """
    if not t.entry_px or not t.close_px or t.entry_px <= 0:
        return None
    raw = (t.close_px - t.entry_px) / t.entry_px
    side = (t.obs.signal.side if t.obs.signal else "") or _infer_side(t)
    if side == "sell":
        return -raw
    if side == "buy":
        return raw
    return None


def _infer_side(t: Trade) -> str:
    """Recovers direction from the outcome when the signal block does not carry it.

    A stop-loss close means price moved against the position and a take-profit means it moved with
    it, so the sign of the raw price move plus the close reason determines the side unambiguously.
    """
    if not t.entry_px or not t.close_px:
        return ""
    up = t.close_px >= t.entry_px
    if t.close_reason == "tp":
        return "buy" if up else "sell"
    if t.close_reason == "sl":
        return "sell" if up else "buy"
    return ""


def rescale(t: Trade, size_usd: float, leverage: float) -> tuple[float, bool]:
    """Realized PnL for this trade at a different size and leverage.

    Returns the PnL and whether the 15% loss cap bound it. The cap is what keeps leverage from
    reading as free money: without it a linear multiply makes 10x strictly better than 1x on every
    winning trade and equally worse on losers, with no liquidation risk represented anywhere.
    """
    ret = price_return(t)
    if ret is None:
        return 0.0, False
    pnl = ret * size_usd * leverage
    floor = -MAX_LOSS_PCT * size_usd
    if pnl < floor:
        return floor, True
    return pnl, False


def augment(
    trades: list[Trade],
    *,
    variants: int = 6,
    seed: int = 7,
    include_original: bool = True,
) -> tuple[list[Trade], AugmentStats]:
    """Expands each trade into several (size, leverage) variants.

    The original row is kept alongside the variants (include_original) so the real, unmodified
    experience stays represented rather than being diluted away by rescaled copies.

    Sizes are drawn against each trade's OWN recorded account equity, so a variant asking for 20%
    of equity means the same thing across trades taken at different balances — the policy sees
    size_pct, and that ratio has to stay honest.
    """
    rng = random.Random(seed)
    out: list[Trade] = []
    stats = AugmentStats(originals=len(trades))

    for t in trades:
        if price_return(t) is None:
            stats.skipped += 1
            continue

        if include_original:
            # The real row gets the v7 budget stamped on it too — it is the same account and the
            # same roster, and leaving it on the schema default would make the untouched trades
            # disagree with their own variants about what budget they were taken under.
            obs0 = t.obs.model_copy(deep=True)
            obs0.max_position_pct = MAX_POSITION_PCT
            obs0.max_leverage = MAX_LEVERAGE
            out.append(replace(t, obs=obs0))
            stats.generated += 1

        equity = float(t.obs.account_equity_usd or 0.0)
        if equity <= 0:
            # Without a known equity there is no meaningful size_pct to vary against; the original
            # is still usable, the variants are not.
            continue

        for _ in range(variants):
            size_pct = rng.uniform(*SIZE_PCT_RANGE)
            leverage = rng.uniform(*LEVERAGE_RANGE)
            size_usd = size_pct * equity
            pnl, capped = rescale(t, size_usd, leverage)
            if capped:
                stats.loss_capped += 1
            # Each variant carries the same v7 budget the live system will present, so the recorded
            # action (a fraction of that budget) means the same thing at training time as at serving
            # time. Without this the rows would default to whatever the schema's fallback is and the
            # model would learn size against a budget it never actually sees.
            vobs = t.obs.model_copy(deep=True)
            vobs.max_position_pct = MAX_POSITION_PCT
            vobs.max_leverage = MAX_LEVERAGE
            out.append(
                replace(
                    t,
                    obs=vobs,
                    size_usd=size_usd,
                    leverage=leverage,
                    realized_pnl=pnl,
                )
            )
            stats.generated += 1

    return out, stats
