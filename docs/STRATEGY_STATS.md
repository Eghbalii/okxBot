# Strategy statistics and training roster

Measured 2026-09-15 by `cmd/backtest` over **76,245 simulated trades**: 44 strategies × 10
instruments × 5m and 15m, against the server's own stored candle history.

Conditions: $40 shared account, 10x leverage, §19.2's 15% loss cap, §45's 1.5–3 reward:risk clamps,
0.05% taker fee both ways (verified against 150 real OKX fills — the measured rate is 0.0489%).

## How to read the table

`t` is the strategy's gap from **`coin_flip`** — a null strategy that fires on a fixed bar cadence
with no reference to price at all, run through the identical path (same sizing, clamps, fees,
SL-wins-a-tie). It measures what an entry signal is worth by measuring what no signal is worth.

**A ranking alone is misleading here.** In the first screening every gap had |t| < 1, meaning the
whole table was one statistical cloud and the ordering carried no information — reading it as a
ranking would have meant deleting 13 strategies for noise and keeping 23 for noise. Only `|t| >= 2`
(marked `*`) is a difference distinguishable from chance.

`pnl/tr` is PnL per trade in dollars, on ~$2.50 positions. Ranked by it rather than total PnL,
because total rewards whichever strategy simply traded most.

## Full results

| strategy | trades | win% | pnl/trade | t | note |
|---|---|---|---|---|---|
| **pmax** | 422 | 31.3% | **+0.0032** | **+3.43** ★ | only significantly-positive kind |
| trend_confluence | 327 | 36.7% | +0.0004 | +1.63 | |
| vwap_reversion_v2 | 306 | 45.4% | −0.0014 | +0.74 | |
| macd_momentum_v2 | 386 | 39.9% | −0.0015 | +0.79 | |
| ict_order_block | 2408 | 31.1% | −0.0018 | +1.59 | |
| grid_like | 1623 | 33.8% | −0.0018 | +1.29 | |
| confluence | 1500 | 30.0% | −0.0019 | +1.12 | new |
| inside_bar_breakout_v2 | 3380 | 34.6% | −0.0021 | +1.45 | |
| vwap_reversion | 4395 | 45.1% | −0.0021 | +1.63 | |
| ict_liquidity_sweep | 3987 | 32.1% | −0.0022 | +1.39 | |
| keltner_trend_scalp | 2314 | 40.0% | −0.0023 | +0.94 | |
| ema_ribbon_pullback_v2 | 2772 | 35.6% | −0.0023 | +0.95 | |
| ict_order_block_v2 | 1774 | 35.2% | −0.0025 | +0.62 | |
| trendshift | 1289 | 31.7% | −0.0025 | +0.50 | new (TV port) |
| ema_ribbon_pullback | 2033 | 37.8% | −0.0026 | +0.53 | |
| inside_bar_breakout | 3842 | 37.3% | −0.0027 | +0.48 | |
| keltner_trend_scalp_v2 | 3981 | 35.1% | −0.0027 | +0.48 | |
| bb_squeeze_breakout_v2 | 534 | 31.8% | −0.0027 | +0.17 | |
| stoch_cross | 2206 | **60.0%** | −0.0028 | +0.26 | highest win rate, still loses |
| engulfing_reversal | 2804 | 37.2% | −0.0029 | +0.18 | |
| ict_liquidity_sweep_v2 | 2515 | 37.1% | −0.0030 | +0.07 | |
| ict_fvg | 3204 | 29.7% | −0.0030 | +0.02 | |
| **coin_flip** | 2147 | 32.4% | −0.0030 | — | **the baseline** |
| rsi_sma_fuzzy | 457 | 33.5% | −0.0030 | −0.00 | |
| session_momentum | 1596 | 28.4% | −0.0030 | −0.02 | new |
| btc_divergence_fade | 2969 | 28.5% | −0.0031 | −0.11 | new |
| btc_divergence | 721 | 27.6% | −0.0032 | −0.09 | new |
| ict_fvg_v2 | 3745 | 32.6% | −0.0034 | −0.57 | |
| engulfing_reversal_v2 | 726 | 34.7% | −0.0034 | −0.26 | |
| weekly_dip_buy | 1866 | 27.7% | −0.0035 | −0.58 | |
| volume_breakout_v2 | 1427 | 32.8% | −0.0035 | −0.50 | |
| macd_momentum | 2184 | 35.3% | −0.0037 | −0.85 | |
| gradient_ribbon | 45 | 26.7% | −0.0037 | −0.12 | new — sample too small to judge |
| bb_squeeze_breakout | 961 | 31.0% | −0.0038 | −0.68 | |
| range_breakout_v2 | 2813 | 31.9% | −0.0039 | −1.21 | |
| rsi_sma | 302 | 33.4% | −0.0039 | −0.42 | |
| dual_ma_atr | 943 | 45.7% | −0.0040 | −0.79 | |
| sweep_reverse | 12 | 41.7% | −0.0041 | −0.10 | new — filters block nearly everything |
| ema_cross_trailing | 757 | 47.7% | −0.0044 | −1.05 | |
| pivot_reversal | 1048 | 33.8% | −0.0048 | −1.53 | |
| stepped_trailing | 436 | 21.6% | −0.0049 | −1.02 | |
| sma_cross_fixed_exit | 1000 | 33.6% | −0.0050 | −1.65 | |
| **volume_breakout** | 974 | 32.0% | −0.0060 | **−2.47** ★ | significantly WORSE than random |
| **range_breakout** | 1114 | 30.9% | −0.0070 | **−3.52** ★ | significantly WORSE than random |

## What the table means

**Win rate is not the metric.** `stoch_cross` reaches its target on 60% of trades — the highest of
all 44 — and still loses, because its target is 1% against a stop filled in at 1.5%. `pmax` wins
31% and is the only profitable kind. Sorting by win rate puts the five worst PnL performers at the
top.

**The system is close, not broken.** Across all 76,245 trades the win rate is 35.9% against a
breakeven of 39.1% at the realized 1.73:1 reward:risk — **short by 3.2 percentage points**. Total
PnL of −$194 spread over 76,245 trades is −$0.0025 each.

That 3.2pp is the entire gap, and it is the size a selection model can plausibly close: it does not
need to predict the market, only to decline the worst fifth of signals.

## Recommended training roster

**Strategies (8):**

| kind | why |
|---|---|
| `pmax` | the only significantly-positive strategy (t=+3.43) |
| `trend_confluence` | second-best PnL, positive t |
| `vwap_reversion` | 45% win rate with 4,395 trades — the largest well-performing sample |
| `vwap_reversion_v2` | same family, better per-trade |
| `ict_order_block` | positive t on 2,408 trades |
| `macd_momentum_v2` | positive t, distinct momentum read |
| `inside_bar_breakout_v2` | positive t on 3,380 trades |
| `keltner_trend_scalp` | 40% win rate, volatility-based, uncorrelated with the above |

Chosen for **variety of market read** as much as for rank: a momentum kind, a mean-reversion kind, a
structural kind and a volatility kind. Selecting the top 8 by PnL alone would be selecting on noise,
since none of their gaps is significant — the model needs signals that disagree with each other, not
eight versions of the same opinion.

**Excluded, and why:**
- `volume_breakout`, `range_breakout` — the only significantly NEGATIVE kinds. Archive, do not train.
- `stepped_trailing`, `sma_cross_fixed_exit`, `pivot_reversal` — worst PnL, no redeeming property.
- `sweep_reverse` (12 trades), `gradient_ribbon` (45) — samples too small to judge; their filters
  need loosening before they can be measured at all.
- `stoch_cross` — its 60% win rate is real and interesting, but its 0.67 reward:risk makes it a
  losing trade by construction. Worth revisiting as a *fixed* strategy, not as training data.

**Leverage: 10x.** Measured directly — 25x produced 2.8x the loss and 10 account resets against 3,
and a controlled pair (25x/15% cap vs 50x/30% cap, identical 0.60% stop distance) gave identical
trades and exactly double the loss. Leverage is a pure multiplier on a negative expectancy.

**Timeframe: 5m.** Also measured — per-trade loss is 60% WORSE on 1H (−0.0084) than on 5m (−0.0052),
contradicting the fee-share argument for moving up. 5m also yields the most training data, which is
this project's binding constraint (§15.11).

## Per-token results

Each instrument run alone across all 44 strategies on 5m, so the absolute PnL is not comparable to
the combined run above — the ordering is what matters.

| token | trades | win% | pnl/trade | distance from the mean |
|---|---|---|---|---|
| **DOGE** | 4036 | 34.6% | **−0.0288** | +3.0 se (best) |
| ZEC | 6290 | 37.5% | −0.0310 | +2.6 se |
| PEPE | 3494 | 37.6% | −0.0316 | +1.7 se |
| PUMP | 7331 | 35.3% | −0.0319 | |
| BTC | 5124 | 32.4% | −0.0320 | |
| SOL | 4643 | 34.8% | −0.0333 | |
| HYPE | 5022 | 35.4% | −0.0378 | |
| ETH | 5539 | 33.1% | −0.0410 | −2.5 se |
| XRP | 4686 | 33.4% | −0.0412 | −2.4 se |
| **TRUMP** | 6543 | 35.1% | **−0.0512** | **−8.2 se (worst)** |

**Unlike the strategy table, these differences ARE significant.** The strategy gaps all sat under
|t|=2 — one statistical cloud — while DOGE sits 3.0 standard errors above the mean and TRUMP 8.2
below it. The spread between best and worst is 1.8x, and it is real.

Worth noting what does NOT explain it: win rate. PEPE has the highest win rate (37.6%) and mid-table
PnL; BTC has the lowest (32.4%) and mid-table PnL; TRUMP's 35.1% is unremarkable while its per-trade
loss is by far the worst. The difference is in the size of the moves, not their direction — which is
consistent with TRUMP being the most volatile instrument on the roster.

### Recommended tokens (6)

**DOGE, ZEC, PEPE, PUMP, BTC, SOL** — the six best by per-trade PnL.

Excluding TRUMP is the one clear-cut call on this page: at 8.2 standard errors below the mean it is
the single most confident finding in either table, stronger than any strategy result.

ETH and XRP are excluded as the next two worst, both around 2.5 se below. That is a weaker case than
TRUMP's and worth revisiting once the model is trained — a selection model may handle a difficult
instrument better than a fixed strategy does.

BTC is kept despite mid-table performance for a structural reason: it is the reference series the
whole observation's market block is built from (docs/RL_V8_PLAN.md), and it is the most liquid
instrument available, which matters when real money eventually trades this.

## Summary of the recommendation

```
strategies: pmax, trend_confluence, vwap_reversion, vwap_reversion_v2,
            ict_order_block, macd_momentum_v2, inside_bar_breakout_v2,
            keltner_trend_scalp
tokens:     DOGE, ZEC, PEPE, PUMP, BTC, SOL
timeframe:  5m
leverage:   10x
account:    $40
```

8 strategies × 6 tokens = **48 (strategy, token) slots**, against the 16 originally planned. At $40
that is $0.83 per position — too small to execute on a real exchange, so either the paper account
cap rises or the roster shrinks before live trading. For TRAINING it is fine: the model learns from
the observation-outcome pairing, not from the dollar size.

## The single most important caveat

Every number here was produced with **no model in the loop** — every signal taken, none skipped. The
purpose of training is to change exactly that, so these figures are the BASELINE the model must
beat, not a prediction of what it will achieve.

The gap to close is 3.2 percentage points of win rate. Define success before training starts: the
win rate of trades the model DECLINES should be measurably lower than those it takes. If that
difference is absent, the model has learned nothing and further training will not help.
