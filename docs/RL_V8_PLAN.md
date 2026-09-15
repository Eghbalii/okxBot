# RL Observation v8 + Backtest Warm Start — implementation plan

Status: **in progress**, started 2026-09-14. Written before any code so a new session can resume.
Decisions below were made with the operator over one session; each is settled unless marked OPEN.

## Why this exists

Auditing the live observation before retraining found that the model has never seen usable market
data. Three defects compounded:

1. `to_vector` pads/truncates the feature block to whatever width the LOADED MODEL expects. The
   deployed model is 91-wide, the tail is 85, so the feature budget was **6**. Go builds 16, so the
   live forming candle's OHLC and 6 of 10 returns were silently cut on every call, from the left.
   Same class of bug as §16.9 (an 83-dim model serving an 89-dim caller), smaller blast radius.
2. `TimeframeBlock.Features` — the 10 `FEATURE_COLUMNS` (RSI, SMA ratios, volatility, volume ratio)
   — is **never populated by Go**. `git log -S "tb.Features"` returns nothing: the line was never
   written. Only `replay_env.py` fills it, and that env is off by default (§15.8). So the model has
   had no RSI, no volatility, no volume since the field was created on 2026-08-26.
3. The token one-hot is 16 slots against a roster that reached 65. Beyond index 15 every token
   one-hots to all zeros — indistinguishable from each other. And `active_tokens` is sorted, so the
   8-hourly discovery scan (§53) reassigns slot meanings whenever a new symbol sorts earlier —
   exactly what obs.py's own comment warns is unsafe.

Net effect: of 91 inputs, **6 described the market**. Everything else was identity, account state,
and position state. §54.8's entropy collapse had an independent, proven cause (alpha 1.0 -> 0.0009),
but even with that fixed the policy had almost nothing to learn from.

## Settled decisions

### Observation v8 (~83 inputs, all reaching the model)

| block | width | contents |
|---|---|---|
| token profile | 7 | typical volatility (ATR/price), log10 24h volume, volume rank in roster, log10 price, 24h range, 24h change, log1p trade count |
| account + risk | 6 | equity/peak_equity, margin exposure ratio, **leveraged** exposure ratio, per-position dollar budget (log10), max_leverage/100, open position count |
| lifecycle category | 5 | buy, sell, update, terminal flag, + 2 scalars encoding WHY (level-touch vs decision; whose decision) |
| strategy profile | 6 | win rate, log1p trade count, avg proposed R:R, avg hold duration, avg PnL per trade, log(bar minutes) |
| signal scalars | 7 | present, side, entry/sl/tp relative to price, entry distance, signal's own R:R |
| position | 10 | open, side, leverage, size/equity, age, unrealized PnL, pnl_max, pnl_min, dist_to_sl, dist_to_tp |
| self market | 26 | 10 returns, live candle OHLC (relative), swing hi/lo, **10 derived indicators** |
| BTC market | 17 | 10 BTC returns, BTC OHLC (relative), BTC swing hi/lo, **correlation with BTC** |

Removed, and why:
- **token one-hot (16)** -> token profile (7). Describes what a token IS, not which one it is.
  No ceiling; a newly discovered token is comprehensible from its first candle; experience
  transfers between similar tokens. Loses per-token quirks, which 2084 trades cannot learn anyway.
- **strategy one-hot (24)** -> strategy profile (6). Already overflowed: 26 kinds, 24 slots.
- **timeframe one-hot (16)** -> one ordered scalar, log(minutes). Timeframe is an ORDERED quantity;
  a one-hot destroys that ordering and tells the model 5m/15m/1H are unrelated.
- **`is_fork` (1)** — verified dead: 2084 paper_orders rows, all `variant='baseline'`. Always false.
- **`Confidence` (1)** — hardcoded per strategy (0.5/0.55/0.6/0.65/1.0), never computed except by
  `rsi_sma` and `rsi_sma_fuzzy`. For the other 24 it is an identity label in disguise, and it
  contradicts the measured win rate now fed alongside it (`grid_like` claims 1.0 with a 35% win
  rate and -$4.62 PnL). Field stays on `strategy.Signal`; it just stops reaching the model.

Added:
- **10 derived indicators per timeframe**, computed IN GO from existing `internal/strategy`
  indicators: `ema_ratio_5/10/20`, `volatility_5/10/20`, `rsi_14` normalized to [-1,1],
  `volume_ratio_20`, `atr_ratio_14`. `ret_1`/`log_ret_1` dropped as redundant with the returns
  window (§15.11 said so and it was never applied). `sma_*` -> `ema_*`, likewise. Sourcing from Go
  removes the two-language duplication §16.2 warns about — `features.py` leaves the live path.
- **BTC reference block.** Operator's own observation: an altcoin reverses the moment BTC's candle
  turns red. Every existing input is intra-token; the model has never seen the wider market. Always
  sent, even when the token IS BTC (duplicates harmlessly, keeps width fixed). The last return is
  the LIVE forming BTC candle, which is the "it just turned red" signal. Plus one correlation
  scalar — the model would otherwise have to infer it from two series, which 2084 trades cannot do.
- **Terminal categories widened from 3 to 5 in meaning.** `timeout` (163 trades) and `manual` (37)
  were both forced under `closed_early` (§15.14, §20) because widening the one-hot meant a schema
  bump. 358 trades — a fifth of all closes — collapsed into one label covering three different
  things. Encoded as a terminal flag plus two scalars rather than 5 one-hot slots, to save width.
  **Manual closes still deliver ZERO reward** (§15.12: attributing an operator's action to the
  policy trains on a decision it never made) — the model is told the position closed, so the
  learner's pending decision resolves rather than leaking, but no gradient follows.

OPEN / deliberately excluded:
- **Funding rate.** Data exists (`funding_rates`, 376 rows, 10 tokens) but only from 2026-09-03,
  while 5m candles go back to 08-23 — so half the backtest would carry a zero that reads as
  "neutral funding" rather than "unknown". Real rates are 0.02-0.03% per 8h against a 15% margin
  stop, and `MaxOpenDuration` is 6h so most positions never cross a funding period. Revisit if hold
  times lengthen or in a volatile regime (rates swing ~60x).
- **Time of day** (sin/cos UTC hour). Cheap and plausibly real; dropped to keep width down.

### Fixed, exact widths — no padding, no truncation, ever

Operator's explicit requirement: nothing may depend on probability or silently adjust.

- `OBSERVATION_DIM` computed from constants in `obs.py`, with a test that builds a full observation
  and asserts the count matches — so adding a field without updating the constant FAILS A TEST
  rather than silently truncating 10 numbers in production.
- `to_vector` raises on any mismatch instead of padding/truncating.
- `/predict` refuses a model whose `observation_space` differs, with 503 — mirroring the existing
  `ACTION_DIM` check that already works. `/health` reports `observation_compatible`.
- **Validation at every layer, per the operator's own framing** ("each function validates itself,
  then one level up re-checks everything"):
  - Layer 1, each builder returns `(value, error)`: `buildPriceContext` errors if the window is
    short of `priceContextWindow+1` or any candle has `close <= 0` — instead of today's `continue`,
    which silently yields 9 returns instead of 10 and changes the vector width. `buildFeatures`
    likewise. `GetAccountEquity`/`openExposure` errors stop being swallowed (today a failure leaves
    `AccountEquityUSD = 0`, which makes equity_ratio, exposure_ratio and size/equity ALL read zero
    — the model is told the account is empty, on a Warn log).
  - Layer 2: `buildObservation` returns `(Observation, error)` and calls `obs.Validate()` — block
    counts, exact per-block widths, `LastPrice > 0`, `AccountEquityUSD > 0`.
  - Layer 3: call sites SKIP `Predict` entirely on an invalid observation and bump
    `okxbot_model_observation_invalid_total{reason}`. A call that 422s would leave a pending
    decision in the learner that never receives its reward — reopening the exact gap §15.12 closed.
  - Layer 4 (Python) should never fire if 1-3 work, and exists because §16.10's lesson is that a
    documented property needs a check that would fail if it were absent.
- Accepted consequence: for the first minutes after a service start, until candle windows fill, NO
  model calls happen. Operator confirmed this is correct behavior, not a regression.

### Action v5 — split heads

Found while auditing: `decode_action` masks the action head to the legal actions for the category
(good, §16.9 added it), but **SAC trains on the raw 9-vector**. The mask exists only at serving
time. So on a `buy` call, reward is attributed to all 5 action outputs including `close`, which was
discarded — the network is trained to raise an output nothing reads. An output that receives reward
without having caused anything feels no corrective pressure and is free to drift to the tanh
bound, which is the direction §54.8 measured (all 9 outputs pinned at ±1).

Change: one open head (1 scalar, sign = open/skip) + one manage head (2 logits: none/update/close),
3 values instead of 5. Each is read only in its own category. And in `learner.py`, zero the
irrelevant head before pushing to the replay buffer so no gradient reaches it.
Bumps `ACTION_SCHEMA_VERSION` 4 -> 5. Free, since we retrain from scratch.

### Reward — one function, shared

Audit found **two independent reward functions that disagree**:

| | `learner.py` (live) | `ReplayEnv` (training) |
|---|---|---|
| denominator | position size | initial equity |
| fees | no | yes |
| churn penalty | no | yes |
| drawdown penalty | no | yes |
| liquidation penalty | no | yes |

Yesterday's commit `7b8ea9b` changed the live denominator and left ReplayEnv untouched — a ~260x
scale difference between training and serving, the §19.1 skew class. Worse, **production has no
penalties at all**: leverage is free, moving SL/TP is free. §15.13's carefully designed penalties
exist only in an env that isn't used. That is consistent with §54.9, where the model walked stops to
entry and nothing in the reward objected.

New single function used by both paths:

```
reward = (pnl - fees) / risk_taken          <- risk-adjusted return, NOT raw return on capital
       - leverage penalty                    <- leverage must cost something
       - churn penalty (on `update` only)    <- moving levels is not free
       - drawdown penalty                    <- a round trip must cost something
```

`risk_taken` rather than position size is the operator's favourite point from the audit: `pnl/size`
cannot tell apart a 5% gain made with a 1% stop from a 5% gain made with a 15% stop, though the
second is three times worse.

**Weights must be recalibrated** — the current 0.5/0.3 came from the legacy env and were never
validated (the code says so). Under the old account denominator, a typical $0.10 PnL on $2600 gave
a base reward of 0.000038 while the drawdown penalty was 0.025 — **650x larger than the reward
itself**, which plausibly contributes to the model converging on "close everything" (§42). Calibrate
from the real distribution of the 2084 closed trades, and pin the properties with tests: the same
trade at higher leverage must score strictly less; the same gain with a tighter stop must score more.

### Live paper roster: 16 slots, $40, 10x

Sizing is equity / PositionSlots (§32.4), and slots is (strategy x token) pairs. Live state found:
500 enabled assignments across 13 strategies x 23 tokens, with a $2600 paper cap. The real account
holds **$40**. At 500 slots that is $0.08 per position — below any exchange minimum, so a model
trained in paper would be unusable on the real account it is meant for.

Operator's choice: **DOGE, ZEC, PUMP, SOL x macd_momentum, inside_bar_breakout_v2,
range_breakout_v2, stepped_trailing = 16 slots**, paper cap **$40** (match the real account), max
leverage **10**.

Leverage 10 rather than MEXC's 100, decided by me at the operator's request: the destination is OKX
where the cap is 10, so training at 100 optimises in a space the risk layer overrules; at $40 a 100x
position is ~1% from liquidation; and higher leverage multiplies reward variance without adding
information. `max_leverage` is an observation INPUT, so a later move to MEXC at 100x adapts without
retraining — that is exactly why that slot exists.

Also needed: `enabled_paper` must stop being set automatically by the discovery scan, or the roster
(65 tokens today, was 55 yesterday) keeps growing and the model's budget input keeps shifting for
no economic reason.

### Backtest — new top-level component

Operator asked for this as its own named section of the codebase, explicitly for open-source value:
someone cloning the repo can train a model from scratch.

Builds the warm-start dataset by replaying stored candles:
- Walk the candle window forward from the start of history.
- Evaluate the REAL `strategy.Strategy` implementations via `EvaluateWith` — same code production
  runs, no reimplementation (the `cmd/strategy-optimizer` precedent, §16.2).
- Build the SAME v8 observation from the same Go builders — no train/serve skew by construction.
- Simulate the trade against subsequent candles until SL or TP is touched.
- Realized PnL -> reward.

Everything each observation needs comes from what the simulation has produced up to that point:
indicators and BTC block from prior candles; token profile from the same candles; **strategy win
rate from the trades the simulation itself has closed so far** — the simulation keeps its own books
from zero, exactly like a fresh install. This is why building NEW trades works where rebuilding the
2084 existing `features_json` rows does not: those need the database's state at a past instant,
which no longer exists.

The 2084 existing rows are DISCARDED. Nothing is lost: the candles they happened on are still in
the `candles` table, so the same period is replayed with complete observations — and with TODAY's
clamps applied uniformly, rather than the mix of rules that changed 3 times during those 11 days.

**Both SL and TP touched in one candle -> assume SL first.** Operator initially proposed using the
close price; I argued against it and they agreed. With 1315 SL closes vs 411 TP closes and R:R
capped at 3, stops are nearer and both-touched is not rare. Close-based judgement would record a
losing trade as a WIN (price hits the stop, reverses, closes green — but in reality that position
was already closed at a loss and never saw the high). That error is one-directional and always
optimistic, teaching the model that tight stops are safe. SL-first is conservative, which is the
right direction for money that will actually be spent.

Available history: 5m 51,009 candles (22 days), 15m 19,380 (1 month), 1H 8,379 (2.5 months), across
24 tokens. The dataset is NOT limited to the 16 live slots — no capital is at risk in simulation, so
every token with history and every registered strategy can contribute. Rough estimate: 3,000-5,000
trades on 5m alone, several times that with 15m/1H.

Honest framing: this is a WARM START, not a replacement for forward-test learning (§2). It exists to
break §16.9's structural deadlock — a randomly initialised policy skips everything, so nothing
closes, so no reward arrives, so the weights never change.

### Entropy (from the previous session's handoff, still to apply)

`target_entropy = -4.5` instead of SAC's `-dim(action_space)` default; reset alpha to 1.0 at load
(BOTH halves needed — a fixed target alone leaves the collapsed alpha restored from the checkpoint,
and resetting alpha alone gets undone by the same target); expose `entropy.coef` on `/health` so the
next collapse is visible; `gradient_steps` 4 -> 1; reconsider `snapshot_every: 25`, which overwrote
a restored backup after 31 steps before anyone could inspect it.

## Order of work (operator's instruction)

1. Fixes: observation v8, exact widths + validation, action v5 split heads, unified reward.
2. Backtest component.
3. Run the initial training, **show results to the operator**.
4. Deploy only after their approval.

## Session hygiene

- Server access via `ssh okx`, file transfer via `scp`; keep local and server in sync.
- Rebuild/restart services ONE AT A TIME, cleaning docker cache between each (§35.7: building two
  at once, or even one while monitoring runs, has killed Kafka on this 3.9GB box).
- Real trading stays stopped throughout.

---

## Progress log

### Done: schema + reward (commit c07a1e8)
- `rl_service/obs.py` rewritten for v8. OBSERVATION_DIM = **84**, ACTION_DIM = **8**, computed from
  constants and pinned by a test that builds a full observation and counts it.
- `to_vector` no longer takes an `expected_dim` and no longer pads or truncates — it raises.
- `rl_service/reward.py`: one risk-adjusted reward function replacing two that disagreed. Weights
  calibrated against the measured distribution of 2084 closed trades (median |return| 5.1%, mean
  risk 6.6%). Verified: a typical win scores 0.73, the same gain with a 15% stop 0.32, the same
  trade at 50x 0.25, with a 20% drawdown 0.67.
- 58 Python tests (40 schema, 18 reward), pinning properties rather than numbers.

### Done: Go observation building (commit d50c2a7)
- `internal/usecase/features.go`: `BuildIndicators`, `BuildMarketBlock`, `BuildReturns`,
  `BuildBTCContext`, `correlation`, `returnVolatility`.
- `internal/domain/rl.go` + `rl_validate.go`: v8 types, `Validate()`, `ValidationField()` for the
  metric label.
- `buildObservation` on both PaperTrader and RealTrader returns `(Observation, error)`; all six call
  sites skip the model on error and count `okxbot_model_calls_skipped_total{stage,reason}`.
- Legacy `Trader.step` no longer calls the model at all.
- 833 Go tests pass, go vet clean.

### Next, in order
1. **Wire BTCCandles and TokenStats in `cmd/paper-trader` and `cmd/trader`.** Both fields are
   declared and nil today, and nil disables model calls — so the engine is inert until this lands.
   BTC's candle window comes from the same store every instrument already reads; TokenStats from the
   discovery scan's `market_tokens` snapshot.
2. **`rl_service/serve/api.py`**: refuse a model whose `observation_space` ≠ OBSERVATION_DIM (503,
   mirroring the existing ACTION_DIM check), report `observation_compatible` on `/health`, and wire
   `mask_action_for_learning` into the learner so a head that decided nothing takes no gradient.
3. **`rl_service/learner.py`**: use `reward.trade_reward`, carry `risk_pct`/fees/leverage through,
   honour the zero-reward rule for manual closes.
4. **Entropy fixes** (from the previous session): `target_entropy = -4.5`, reset alpha at load,
   expose `entropy.coef` on `/health`, `gradient_steps` 1, reconsider `snapshot_every`.
5. **Backtest component** — its own top-level section, as the operator asked, for open-source value.
6. **Roster**: 16 slots (DOGE/ZEC/PUMP/SOL x macd_momentum/inside_bar_breakout_v2/
   range_breakout_v2/stepped_trailing), paper cap $40, max leverage 10. And stop the discovery scan
   from auto-enabling `enabled_paper`, or the budget input keeps shifting for no economic reason.
7. Train, **show the operator the results**, deploy only on their approval.

### Operator instruction (2026-09-14): all three RL gates ON

`rl_sizing`, `rl_sltp_adjust` and `rl_early_close` are ALL enabled — in the backtest and in live
paper trading alike. The model makes the open decision, manages the levels, and may exit early.

This reverses two long-standing defaults and the reasons they existed are worth stating, because
both are now answered rather than ignored:

- **`rl_sizing` was off** because §14's first attempt converged to 100% `skip` within 20 minutes on
  a policy with 39 completed trades. That policy had six market inputs (docs/RL_V8_PLAN.md's audit)
  and a reward of ~1e-4. Both are fixed, and the backtest warm start means the policy is no longer
  random when live trading begins — which is what §16.9's deadlock was actually about.
- **`rl_early_close` was off** because it destroys the counterfactual: an early-closed trade can
  never show what it would have done. That cost is real and accepted. It is also the only way the
  model's own exit judgement ever gets a reward attached to it, and §42 measured the model asking to
  close on 84 of 89 update calls with every one discarded — the question has been asked thousands of
  times and never answered.

The backtest must exercise the same three, or the warm start would train a policy on a lifecycle
different from the one it is then served — the train/serve skew this whole plan exists to remove.

### Done: end-to-end wiring (commit ac35956)
- `usecase.BTCReference` (shared BTC windows, own consumer group, independent of the traded roster)
  and `usecase.TokenStatsCache`, wired into both `cmd/paper-trader` and `cmd/trader` with their own
  source-level regression tests (mutation-checked).
- `/predict` refuses a model whose observation width differs (503); `/health` reports
  `observation_compatible`, `observation_dim`, `model_observation_dim`, `entropy_coef`.
- Entropy repair at load: `target_entropy = -4.5` + alpha reset to 1.0. `gradient_steps` 4 -> 1,
  `snapshot_every` 25 -> 100.
- `init_model.py` takes its width from `OBSERVATION_DIM` rather than probing.
- Removed `rl_service/env`, `train.py`, `pretrain.py`, `augment.py` — superseded, and a
  half-updated training path would leave two definitions of the observation free to disagree.

### Done: the penalties had no inputs (commit f4cf327)
Found because the operator asked whether penalties had been forgotten. Two of three could not fire:
- `sltp_adjustments` was never sent, so the churn penalty was permanently zero.
- `risk_pct` was declared and never populated, so the reward took its fallback to return on capital
  on EVERY trade — silently, because the fallback works.

Counted in the Conductor rather than read back from the database at close time. Only an APPLIED
move counts: a proposal the ratchet rejected moved nothing, and charging for it would penalise an
intention rather than an action. A real bug surfaced: `ShouldUpdate` assigned a fresh `updateState`
to advance the cadence baseline, zeroing the count on every update — a trade adjusted twenty times
would be charged for one.

Five new Python tests score a REAL terminal observation through the same call `api.py` makes. That
distinction is the lesson: the penalties were already tested in isolation and passing while two of
their inputs did not exist.

### Done: the backtest (commit 1fb4737)
`internal/backtest` + `cmd/backtest`. Real strategies, the live observation builders, the reward
pinned by number against `reward.py`. SL wins a tie when a bar spans both levels. The strategy
profile accumulates from the simulation's own books. Order ids per trade — caught by
`Observation.Validate`, which refused a terminal call without one.

### Next
1. Run the backtest against the server's real candle history; show the operator the summary.
2. Set the live roster: 16 slots, $40 cap, 10x, all three RL gates ON.
3. Train from the dataset, show results, deploy only on approval.

**SUPERSEDED — see "Session 2026-09-15" at the end of this file.** Step 1 was done and step 3 was
attempted five times; the dataset turned out to have 23 of 84 inputs at zero and an action encoding
that could not represent the strategies' own levels. Three of those defects are fixed; the action
encoding is still open and is the blocker.

---

## The strategy investigation (2026-09-14, after the backtest landed)

The backtest was built to produce a warm-start dataset. Running it produced something more
important: a measurement of whether the strategies carry any edge at all.

### What was measured

37 strategies, 10 tokens, 2 timeframes, **68,113 trades**. 36 of 37 lost money, at a 35.9% win rate.

Three diagnoses were proposed and **all three were killed by measurement**, which is the part worth
carrying forward:

| hypothesis | how it died |
|---|---|
| fees are eating the edge | computed: 1.00% of margin per round trip against 6.87% average risk — 14.6% of risk, real, but EV is still positive at 3:1 |
| the SL ratchet shrinks wins | the backtest has no ratchet at all; positions run untouched to SL or TP |
| the reward:risk floor is too low | swept it: win rate fell almost exactly along the 1/(1+R) breakeven line, PnL barely moved |

That last sweep is the actual finding:

```
min R:R 1.5 -> 35.2% win (breakeven 40.0%)   -4.8pp
min R:R 2.0 -> 32.8% win (breakeven 33.3%)   -0.5pp
min R:R 2.5 -> 28.7% win (breakeven 28.6%)   +0.1pp
```

Win rate tracking the breakeven line as the ratio changes is what a system with no edge looks like:
the entry decides WHEN, and the SL/TP geometry decides everything else.

### coin_flip, and why it is shipped code

That was an inference from an arithmetic identity, and acting on it means abandoning parameter
tuning for the whole roster — so it needed measuring. `internal/strategy/coinflip.go` fires on a
fixed bar cadence with no reference to price, alternating sides, registered as an ordinary kind so
it runs through the identical path (same sizing, clamps, fees, SL-wins-a-tie).

On real data it placed 24th of 37. But testing every gap against its own sample size showed **every
t below 1** — the whole table is one statistical cloud. Reading it as a ranking would have meant
deleting 13 strategies for noise and keeping 23 for noise. `backtest.SignificanceVsBaseline` now
reports t for every comparison, so this cannot be misread again.

### The four new kinds

All 36 existing strategies see a pattern on one chart and enter immediately. None asks whether the
market suits the pattern, none combines opinions, none looks outside its own instrument, none knows
what time it is. The four fill exactly those gaps, and each carries its own risk in its doc comment:

- **`RegimeFilter`** — a wrapper, so "does this pattern only work in a trend?" becomes one question
  for all 36 rather than 36 edits.
- **`confluence`** — the only mechanism that can amplify a small edge. Honest caveat: the members
  are not independent, so agreement may be one signal counted three times.
- **`btc_divergence`** — ships in BOTH directions (follow/fade) because which is right is empirical.
  `strategy.MarketView` gained `Reference` to carry BTC's series.
- **`session_momentum`** — the easiest to fool yourself with, so its hours are the conventional ones
  rather than hours found by searching.

**A finding the tests produced**: confluence fired ZERO times in 470 bars while its members fired
294 times between them — they never agreed on the same bar. Measured by window: 1 bar -> 0, 3 -> 1,
5 -> 37, 10 -> 203. Strategies react to one setup at different moments, so same-bar agreement asks
for a coincidence the candle boundary makes rare. Without that test the idea would have scored zero
trades and been recorded as a failure, having never run.

### Open, and the operator's own framing

The operator's instruction was to spend the time on NEW strategies rather than improving ones that
are not good — which the measurement supports: tuning parameters of a strategy with no measurable
edge cannot create one.

Still to do:
1. Score the four against coin_flip on real data with significance (running).
2. Archive, not delete, the kinds that lose to the baseline (operator's explicit preference).
3. TradingView strategies the operator finds — ask for timeframe, market, and crucially whether it
   has an entry FILTER, since that is what all 36 lack.
4. Then the warm start and training.

---

### Everything measured in this session, and what it ruled out

Seven hypotheses tested and rejected by measurement rather than argument:

| hypothesis | result |
|---|---|
| fees are eating the edge | 14.6% of risk — real, but EV still positive at 3:1 |
| the SL ratchet shrinks wins | the backtest has no ratchet at all |
| raising the reward:risk floor | win rate tracks the breakeven line; PnL barely moves |
| a higher timeframe is cheaper | per-trade loss is 60% WORSE on 1H than 5m |
| widening stoch_cross's target | win rate tracks breakeven at every setting |
| higher leverage suits scalping | 25x cost 2.8x the loss and 10 account resets vs 3 |
| the loss cap is what binds leverage | a wider cap helps win rate (32.2→34.8%) but not PnL |

The controlled pair that settles leverage: 25x/15% cap and 50x/30% cap give the SAME 0.60% stop
distance, the same trades and the same 32.2% win rate — and exactly double the loss. Leverage is a
pure multiplier on a negative expectancy.

---

## The training roster (measured 2026-09-15)

Full per-strategy and per-token tables are in **docs/STRATEGY_STATS.md**. The conclusion, here
because it is what the next session acts on:

```
strategies: pmax, trend_confluence, vwap_reversion, vwap_reversion_v2,
            ict_order_block, macd_momentum_v2, inside_bar_breakout_v2,
            keltner_trend_scalp
tokens:     DOGE, ZEC, PEPE, PUMP, BTC, SOL
timeframe:  5m
leverage:   10x
account:    $40
```

### Why these strategies

Measured over 76,245 simulated trades against `coin_flip` — a null strategy that fires on a fixed
bar cadence with no reference to price, run through the identical path.

**Only three of 44 differ from chance at all**: `pmax` (t=+3.43, the only profitable kind) and
`volume_breakout`/`range_breakout` (t=−2.47/−3.52, significantly worse). Every other gap sits under
|t|=2 — one statistical cloud.

So the eight were chosen for **variety of market read** as much as for rank: a momentum kind, a
mean-reversion kind, a structural kind, a volatility kind. With no significant gaps, taking the top
eight by PnL would be selecting on noise, and a selection model needs signals that disagree with
each other rather than eight versions of one opinion.

Excluded outright: `volume_breakout` and `range_breakout` (the only significantly negative kinds);
`sweep_reverse` (12 trades) and `gradient_ribbon` (45) — samples too small to judge, their filters
need loosening first; `stoch_cross`, whose 60% win rate is real and interesting but whose 0.67
reward:risk makes it a losing trade by construction.

### Why these tokens — and this is the stronger finding

Unlike the strategy table, **the token differences ARE significant**:

| token | pnl/trade | distance from mean |
|---|---|---|
| DOGE | −0.0288 | +3.0 se (best) |
| ZEC | −0.0310 | +2.6 se |
| PEPE | −0.0316 | +1.7 se |
| PUMP / BTC / SOL | −0.0319 … −0.0333 | mid |
| ETH / XRP | −0.0410 / −0.0412 | −2.5 se |
| **TRUMP** | **−0.0512** | **−8.2 se (worst)** |

**Dropping TRUMP is the most confident single result of the whole investigation** — stronger than
any strategy finding. And win rate does not explain the spread: PEPE has the highest win rate with
mid-table PnL, TRUMP an unremarkable one with the worst by far. The difference is move SIZE, not
direction, consistent with TRUMP being the most volatile instrument on the roster.

BTC is kept despite mid-table performance because it is the reference series the observation's
market block is built from, and the most liquid instrument available.

### Two settings, both measured rather than assumed

**Leverage 10x.** 25x produced 2.8x the loss and 10 account resets against 3. The controlled pair
settles it: 25x with a 15% cap and 50x with a 30% cap give the SAME 0.60% stop distance, the same
trades and the same 32.2% win rate — and exactly double the loss. Leverage is a pure multiplier on a
negative expectancy.

**Timeframe 5m.** Per-trade loss is 60% WORSE on 1H (−0.0084) than on 5m (−0.0052), contradicting
the fee-share argument for moving up: the fee does fall as a share of the bar, but the stop scales
with the bar too, so losses grow faster than the saving. 5m also yields the most training data,
which is this project's binding constraint (§15.11).

### The gap the model has to close

Across all 76,245 trades: **35.9% win rate against a 39.1% breakeven** at the realized 1.73:1
reward:risk — short by **3.2 percentage points**. Total PnL of −$194 over 76,245 trades is −$0.0025
each.

That is the entire deficit, and it is the size a selection model can plausibly close. The model does
not need to predict the market; it needs to decline roughly the worst fifth of signals.

### Define success before training starts

Every number above was produced with **no model in the loop** — every signal taken, none skipped.
They are the baseline to beat, not a prediction.

**The criterion**: the win rate of trades the model DECLINES must be measurably lower than those it
takes. If that difference is absent, the model has learned nothing and further training will not
help — which is the check §14's first `rl_sizing` attempt lacked, and why it ran for 14 hours
answering `skip` to everything before anyone noticed.

---

## Session 2026-09-15: the dataset was not fit to train on

The previous section defined the roster and the success criterion. This session tried to train
against it, failed five times, and only then looked at what was actually being fed to the model.
**That order was the mistake** — the defects below took three minutes to find by printing one
observation vector, and five training runs to not find by tuning hyperparameters.

Recorded in the order they were discovered, because the sequence is the lesson.

### What was built

- **`rl_service/warmstart.py`** (new) — the missing consumer of `cmd/backtest`'s JSONL. Reads the
  dataset, vectorises through the same `to_vector` `/predict` uses, fills the replay buffer, runs
  gradient steps, saves weights AND buffer together (§15.11). 10 tests, mutation-checked.
- **`rl_service/newmodel.py` DELETED.** It was a second model builder that did NOT apply
  `target_entropy` from config, so every model it produced ran SAC's default of `-8` — the exact
  value §54.8 records as having collapsed the previous model. `init_model.py` already existed and
  does it correctly. Having two ways to build a model is what let the wrong one be picked.

### Four training runs, and what they actually measured

Each: model from scratch, trained on the first 80% chronologically, scored on the unseen 20%.
`rank_t` asks whether trades the model rates higher actually do better — it survives even when the
policy declines nothing, which a skip-count cannot.

| dataset | entropy | steps | mean rank_t | sd | positive |
|---|---|---|---|---|---|
| 8×5, 5m only (4,166) | −8 | 3,332 | +0.59 | 0.70 | 4/5 |
| 8×5, 3 timeframes (6,649) | −8 | 5,319 | +0.37 | 0.77 | 2/4 |
| 44×10, 3 tf (76,305) | −8 | 20,000 | +0.56 | 3.64 | 3/5 |
| 44×10, 3 tf (76,305) | **−4.5** | 2,500 | −1.21 | 2.83 | 3/5 |

**No configuration produced a significant signal.** Every mean sits inside its own noise. Seed
variance dominates everything: on one dataset the skip count ranged 23 to 656 across seeds, and
`rank_t` from −4.72 to +3.94 — so any single run reports the seed, not the data.

A staged curve (1 → 500 → 2,500 → 5,000 → 10,000 steps on one seed) appeared to show a clean
progression from −4.77 to +1.21 and back down, suggesting an optimum near 2,500. **That curve was
not reproducible**: the same seed re-run gave +1.21 then +0.61, because `init_model` runs before the
seed is set, so each stage started from different initial weights. It was presented with confidence
and should not have been.

### Then the observation was printed, and the dataset was the problem

Dumping one 84-dim vector with named fields — three minutes of work, never done before five training
runs — found **23 of 84 inputs at exact zero**, and across 40,000 samples:

| block | finding |
|---|---|
| token profile | **5 of 7 dead**: volume, volume rank, 24h range, 24h change, trade count. The block exists specifically to replace the token one-hot so the policy can tell a BTC from a PEPE; it could see only price and volatility. |
| position (10 inputs) | **all dead on every sample** — see the update section below |
| `close_rel` (×2) | **structurally zero forever**: `_rel(close, last_price)` where `last_price` IS the close. Divides a number by itself, in both the market and BTC blocks. |
| strategy profile | correctly zero only on a strategy's first trade; 99.7% populated overall. An early report called these dead, from a single sample — wrong. |

### Fixes applied (code only — NO dataset was regenerated)

1. **Token profile now computes all five fields from the candle window.** `candles.volume` was in
   the database the whole time; the code's own comment said these "come from the discovery scan and
   have no historical record", which was true of the rank and false of the rest. Volume is quote
   volume (base × close) per §33.4's lesson that a contract count orders the market by contract
   size. Rank is median volume across the roster, computed once per run. Trade count accumulates
   from the simulation's own books, exactly like the strategy profile.
2. **`close_rel` replaced with `_body_position`** — where the close sits within its own high/low
   range, in [−1, +1]. Real information (did the bar close strong or weak) at no width cost, and the
   half of candle shape the other three OHLC slots cannot express.
3. **`update` samples added to the backtest**, with two triggers, both mirroring `conductor`:
   - **a strategy firing while that (strategy, token) already holds a position.** These were being
     DROPPED — 125,000 of them, three for every trade taken. §15.12 routes them as `update` in
     production; the backtest discarded them silently.
   - **cadence**: unrealized PnL moved ≥1% or 15 minutes elapsed, from
     `conductor.DefaultUpdatePnLThresholdPct` / `DefaultUpdateMaxInterval` rather than a local copy.
   - Age comes from the candle's own timestamp, never a bar count: a gap in the series (an exchange
     outage leaves missing candles) ages the position by real elapsed time.
   - Reward is the trade's own outcome and the `order_id` is shared: an `update` answers "keep
     holding", and only what the holding produced can judge it.
4. **Signal carry-forward on updates.** The first implementation sent `nil` on every cadence update,
   on the reasoning that a carried signal is stale. **Production does the opposite** —
   `lifecycle.go:159` sends `conductor.CarriedSignal`, because a 1H opinion stays meaningful for the
   whole hour. Sending nil would have produced a dataset where most updates carry `present=0`
   against a live path that sends `present=1`: the train/serve skew this plan exists to remove,
   reintroduced by the code meant to close it. Caught only because the operator asked why.
   `signalProfile` was extracted so the open and update paths build the signal from one function.

Three new tests (`TestRun_EmitsUpdateSamplesWithALivePositionBlock`,
`TestRun_UpdateCarriesTheSignalForwardLikeProduction`, `TestRun_UpdateAgeComesFromTimestampsNotBarCount`),
all mutation-checked. Four existing tests assumed every sample was an open decision — one crashed on
a nil `Signal`; they now select the sample they mean rather than the assumption being loosened.
23 backtest tests pass, `go build` and `go vet` clean.

### STILL OPEN — do these before generating another dataset

Found by inspecting the model's answer on a real sample. All three have one root cause: **the
backtest runs without a model, so no sizing or SL/TP decision is ever made**, and the plan's own
"all three RL gates ON" (the 2026-09-14 instruction above) is not implemented in `internal/backtest`
at all — `grep` for `rl_sizing|RLSizing|RLEarlyClose` there returns nothing.

1. **`MAX_SLTP_OFFSET_PCT = 0.10` is ~20x the real range.** Strategies on 5m propose stops at
   0.5–0.75%, so every training sample sits inside `[-0.075, +0.05]` of a `[-1, +1]` output range.
   Over 90% of the action space has never seen a sample, and the policy's output goes there: on a
   real BTC short at 77,403 the trained model answered `tp_px = 83,241` — 7.5% ABOVE entry, i.e.
   "take profit once you have lost". Nothing in the encoding prevents it.
2. **No variance in `size_pct` or `leverage`.** Every one of 76,305 samples carries `+1.0` for both,
   because the backtest opens at a fixed slot size. The model cannot learn when to size down from a
   dataset in which nothing ever did.
3. **Whether to run the backtest with a model in the loop at all.** The clean fix for 1 and 2 is for
   the backtest to ask a model for size/leverage/levels, which makes dataset generation depend on a
   model — the chicken-and-egg the warm start exists to break. Decide the approach before building:
   a fixed-range rescale (cheap, fixes 1 only), randomised sizing (rejected once already in §14 as
   noise the policy cannot learn from), or a two-pass generate-train-regenerate loop.

### Operating notes for the next session

- **The dataset is regenerated by `cmd/backtest`**, ~7 minutes for 76k samples on the server, ~12
  for the full roster across three timeframes. The candle history is the durable part; no dataset
  file is worth preserving across a schema change.
- **Train on the operator's machine, not the server.** A training run alongside the live services
  drove load average to 120, made SSH unreachable, and OOM-killed `rl-service` (recovered). §35.7's
  one-thing-at-a-time rule applies to training as much as to builds.
- **Run each seed in its OWN process.** Five seeds in one process died with no output every time —
  each SAC model plus its replay buffer stays resident.
- **Detach long server jobs with `setsid`**, or they die with the SSH session. One 40-minute
  training run was lost that way.
- `paper-trader` is the ONLY writer of the `candles` table (§14). While it is stopped, candle
  history stops accumulating even though the ingestor is publishing to Kafka normally.

### The honest summary

Nothing has been trained that is worth deploying. The five runs measured a dataset in which the
position block was entirely dead, five of seven token inputs were zero, two inputs were structurally
constant, and the action encoding could not represent what the strategies actually proposed. The
code is now fixed for the first three; the fourth is open and is the one that matters most.
