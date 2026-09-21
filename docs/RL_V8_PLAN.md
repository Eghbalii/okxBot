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
  - **Revised 2026-09-21**: correlation was originally measured over the SAME 10-bar window as the
    rest of this block (the decision bar, e.g. 5m — under an hour of data), too short a window for
    "does this token currently follow BTC" to mean much. Now measured one timeframe UP from the
    decision bar instead (`usecase.nextHigherBar`/`btcCorrelationReturns`,
    `internal/usecase/features.go`/`tickfeed.go`) — e.g. 5m decisions correlate against 15m returns
    — while the rest of the BTC block (OHLC, swing, the live-forming-candle return) stays on the
    decision bar, since freshness is the point there. Resolved dynamically against whatever bars
    are actually configured/maintained (a MarketView's own `Bars` keys), never a hardcoded
    timeframe name, so it keeps tracking "one step up" automatically if the decision timeframe
    itself changes later. Falls back to the decision-bar series when no higher bar is configured or
    its window hasn't filled yet (warm-up degrades the correlation's time horizon rather than
    erroring the whole observation). Does not change `OBSERVATION_SCHEMA_VERSION` — the field's
    shape/meaning to the model is unchanged, only what it's computed FROM.
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

---

## Session 2026-09-15 (new): decode_action's side-blind TP fix, and three gates deliberately deferred

Operator asked directly about the reported bug — a short whose take-profit landed ABOVE entry, ran
to a real loss, and was scored as a legitimate trade — from the previous session. The full trace,
plus a decision on the three ON/OFF gates from 2026-09-14, both landed this session.

### The TP-above-entry bug: diagnosed, and it never reached a real order

Confirmed against the live database before writing anything: `paper_orders` (2,102 rows),
`real_orders` (150 rows), and `paper_order_adjustments` (0 `tp_px` rows, ever) all contain **zero**
wrong-side levels. Every Go call site that turns a model answer into a stored order —
`lifecycle.go` (paper open), `realtrader.go` (real open), `sltp_ratchet.go`'s `moveTP` (in-trade,
both paths) — routes the raw decode through `conductor.Clamps.Apply`, which drops (not clamps) a
level on the wrong side of entry, exactly as designed since §15.12/§29. That guard has held for
every trade in the system's history.

**The bug is real, and lives entirely in `rl_service/obs.py`'s `decode_action`**, confirmed at
obs.py:753-772 (pre-fix): `tp_px = last_price * (1 + tp_offset)`, with `tp_offset` computed from
the raw policy output and **no reference to `obs.category`/side at all**. A positive `tp_offset`
always pushed TP above price — correct for a long, backwards for a short. The 77,403 -> 83,241 BTC
short in the earlier session's finding was produced by directly inspecting the model's raw output
during dataset-quality checking (no persisted script found; nothing in `cmd/backtest` or
`warmstart.py` calls a model to decide anything — both were confirmed to use only
already-clamped, strategy-native levels). That direct-inspection path is the one place in the whole
system where nothing downstream corrects the raw answer, which is exactly why the bad number was
visible there and nowhere else.

**Fix**: `resolved_direction(obs)` (obs.py) returns `+1.0`/`-1.0` — the category itself for
`buy`/`sell` (no position exists yet to consult), the open position's own recorded side
(`obs.position_state.side`) for `update` and every terminal category, never the carried signal
(which can name a stale side across a candle boundary per §15.12's carry-forward design). A flat/
unknown position (`side == 0.0`, e.g. a synthetic observation) makes no direction claim and passes
offsets through unflipped — the exact pre-fix behavior for that one case, rather than guessing.
`decode_action` multiplies both `sl_offset` and `tp_offset` by this direction, so the SAME raw
policy output now opens a favorable position on either side: unchanged for a long, mirrored for a
short. This does not replace Go's clamp — the docstring says so — it removes the nonsensical raw
value at the one place (`/predict`, direct inspection) nothing downstream corrects it.

**`warmstart.py`'s `action_for`** (the inverse of `decode_action`, reconstructing what action vector
would explain a trade the backtest already took) needed the identical correction — it round-trips
`sig.sl_px`/`sig.tp_px` back into `vec[0]`/`vec[1]` and, being unmirrored, agreed with the OLD
buggy `decode_action` by construction. Left alone, the direction fix would have silently broken this
round trip for every short in the dataset: the module's own docstring's warning ("an action vector
that does not describe the trade... nothing downstream can detect it") would have applied to the
fix itself. Now divides by the same `resolved_direction` (self-inverse, since it's always ±1.0)
before clamping.

4 new tests (2 in `test_obs_action.py` covering `sell`/`update`-with-a-short-position/
flat-position, 2 more folded into the same file for the long/flat no-op cases; 1 round-trip test in
`test_warmstart.py` for a short). All mutation-checked: reverting `decode_action`'s multiplier
reproduces the exact reported symptom (`tp_px=105.0` above a `100.0` entry) and fails both new tests
for that reason; reverting only `action_for`'s companion fix (leaving `decode_action` fixed) fails
the round-trip test with a level on the wrong side. 88 Python tests pass (was 87).

Not yet deployed to the server — this is a Python-only, `rl_service`-scoped fix with no Go changes
and no interaction with any running service (the server's `rl-service` container serves whatever
checkpoint is currently loaded; this changes how a NEXT dataset/training run's decode behaves, not
the currently-running one). Deploy alongside whichever training-run change ships next, not as its
own hot-fix, since nothing live depends on it today.

### Operator decision: all three 2026-09-14 gates deferred, not wired into the backtest

Investigated feasibility of wiring `rl_sizing`/`rl_sltp_adjust`/`rl_early_close` into
`internal/backtest` per the 2026-09-14 instruction. Verdict, confirmed against the actual code: this
is **not a mechanical addition** for two of the three.

- **`rl_sizing`/`rl_sltp_adjust` both require an already-trained model checkpoint present DURING
  dataset generation** — the model decides size/leverage/levels, so there is nothing to imitate
  without calling one. This is the exact chicken-and-egg the plan already named (STILL OPEN item 3):
  the dataset is meant to warm-start the first model, so it cannot depend on that model already
  existing. The Go wiring cost is genuinely small (`sizeFromModelAction`, `conductor.Clamps.Apply`,
  `RatchetSLTP` are already free functions `internal/backtest` can import), but the prerequisite —
  a trained model — does not exist yet.
- **`rl_sltp_adjust` has a second, structural blocker**: live trading manages SL/TP on the **tick**
  stream (2s throttle); the backtest replays candle-only history (`domain.Candle` is OHLCV, no tick
  granularity exists in the schema at all). Approximating this at candle cadence is itself a design
  decision, not a free substitution.
- **`rl_early_close` is the smallest of the three** but still needs a model to call, and
  `internal/backtest` has no `Model` field today (`grep` confirms zero references) — adding one
  would be inert scaffolding until a checkpoint exists to plug into it.

**Operator's explicit decision, given this: defer all three.** No `Model` field added to
`internal/backtest`. Confirmed instruction: "بزار دیتا رو فیکس کنیم ببینیم چجوری میشه مدل" — fix
the dataset first, decide the model-in-the-loop question once there is something to loop in. This
supersedes the 2026-09-14 "all three RL gates ON" instruction for the CURRENT dataset/training
round; revisit once a first warm-start model exists to break the dependency.

### Re-assessed: `MAX_SLTP_OFFSET_PCT` and zero size/leverage variance are correctly deferred, not urgent

Checked both remaining STILL OPEN items against the gates decision above before touching either.
Both are about the model's OWN sl/tp/size/leverage proposals being under-trained or unrepresented
— and with `rl_sizing`/`rl_sltp_adjust` deferred, **nothing in this training round ever reads those
outputs live**: `cmd/paper-trader/main.go:189-192` only constructs an `rlclient` at all when one of
those two flags is true, so with both off the model isn't even called over the network for
open/update decisions — `lifecycle.go`'s `if e.Model == nil || !e.RLSizing { return ..., false }`
and the equivalent `RLSLTPAdjust` gate make `action.SLPx`/`SizePct`/`LeverageFrac` unreachable code
this round. A bad output there is inert dead weight, not a live-safety issue (distinct from the
`decode_action` fix above, which mattered because a human directly inspecting the model IS a real,
currently-used diagnostic path with nothing downstream to correct it). Both items are real training-
signal-quality concerns worth fixing once `rl_sizing`/`rl_sltp_adjust` are actually turned on in a
future round — not this one.

### A real bug found while verifying the prior session's claimed fixes (commit pending)

Operator asked to independently verify all four defects the prior session claimed to have fixed
(token profile, `close_rel`, update samples, signal carry-forward) before allowing dataset
generation to proceed. Three were confirmed correct by direct code reading, not just re-reading the
doc. **The fourth's own claim was false**, and led to finding a real, previously undocumented bug:

`signalProfile`'s doc comment claimed it was "shared by the open path and the busy-signal update
path so the two cannot drift" — but `grep` showed the open path (`buildObservation`, called from
`openPosition`) never called it at all. It built the signal profile inline instead, using `lv` — the
**clamped order levels** — for `SLPx`/`TPPx`. Checked against production
(`usecase.PaperTrader.evaluateStrategies`, `papertrade.go:573-583`): `obs.Signal` there is always
built from `resolved.SLPx`/`TPPx` — the strategy's **raw, unclamped** proposal, retained via
`conductor.RetainSignal` *before* the model is even asked; clamping (`EnsureStop`/`Apply`) happens
afterward and only ever bears on `order.SLPx`/`TPPx`, never retroactively updating what the model
was shown as the signal.

So every open-decision sample in the dataset was showing the model **the placed order's clamped
levels** where production always shows **the strategy's raw ask** — a genuine train/serve skew, and
not a rare one: §45/§19.2's clamps (the 3:1 reward:risk cap, the 15%-of-margin loss cap) are
documented as actively binding on a real fraction of live orders (§54.7 found 13/19 open real
positions past the ratio cap when checked), so this corrupted a meaningful share of open samples,
not an edge case.

**Fix**: `buildObservation` now calls `signalProfile` directly (dropped its now-unused `resolved`/
`lv` parameters), making the doc comment's claim actually true. 1 new test
(`TestOpenObservation_SignalCarriesTheUnclampedProposal`, mutation-checked: reverting the fix
reproduces the exact divergence — `Signal.SLPx`/`TPPx` showing the clamped price instead of the raw
one — with a stop chosen wide enough that clamping provably changed it, not a coincidence). One
existing test (`TestRun_NoSampleWithoutAStop`) had been asserting the WRONG invariant as a side
effect of the bug — it read `Observation.Signal.SLPx` expecting it always positive, which was only
true because Signal was accidentally always the clamped (always-has-a-stop) level. Since a strategy
with no structural stop (§16.8: `stoch_cross` and others) legitimately produces an absent
`Signal.SLPx` in both production and the fix, the test now asserts against
`Terminal.PositionState.RiskPct.IsPositive()` — derived from the position's actual entry-to-stop
distance, which `openPosition`'s pre-existing `errNoStop` check already unconditionally guarantees.
895 Go tests pass repo-wide (was 895 before backtest's own count went 23→24; no other package
affected), `go build`/`go vet` clean.

Synced to the server and rebuilt (isolated `docker run golang:1.26-alpine` container mounting the
source, no service touched — see below); operator approved sample inspection and a re-run of
`STRATEGY_STATS.md`'s screening scope before choosing the training roster.

### Confirmed live: production DOES re-evaluate a strategy while it already holds a position

Re-running the full `STRATEGY_STATS.md` screening scope (10 tokens x 44 firing kinds x 5m+15m) with
the fixed code produced numbers that diverged sharply from the original table — most strategies'
per-trade PnL got WORSE while win rates stayed close to the original (e.g. `pmax` 31.3%→29.3% win
rate but +0.0032→−0.00021 PnL/trade). Traced to a DIFFERENT change already inside commit `c141c25`
(the same commit this session's earlier verification pass checked three other claims from): the
busy-signal handling in `run.go`'s `runOne` now calls `evaluate(s, view)` **before** checking
whether that strategy already holds a position, where it used to skip evaluation entirely while
busy. Stateful strategies (`pmax` mutates `prevTrend`/`prevLongStop`/`prevShortStop` on every call)
now keep advancing their internal state throughout a held trade's life, changing what signal they
produce once the position closes — a real change to WHICH trades get opened, not a bookkeeping
detail.

Checked directly against `usecase.PaperTrader.evaluateStrategies` (`papertrade.go:533`,
`strategy.EvaluateWith(s, view)`) before accepting this as correct rather than reverting it:
production calls `EvaluateWith` unconditionally on every strategy assigned to the bar that just
closed, and only checks `hasOpenBaselineFor(open, a.StrategyID)` **afterward** (`papertrade.go:566`)
to decide whether the resulting signal opens a position. This is the exact same ordering the
backtest now uses. So the OLD backtest behavior (freezing a stateful strategy's state while it held
a position) was itself a train/serve skew that `STRATEGY_STATS.md`'s numbers were quietly built on;
the new behavior is the correct match to production, and the old table is retired, not the new one.

### A second, independent sizing bug found while re-running the screening (fixed, commit pending)

The re-run's numbers were suspicious for an unrelated reason too: dollar PnL per trade came out
implausibly small relative to the exchange fee even after accounting for the ordering fix above.
Traced to `cmd/backtest/main.go`'s `positionSlots()`: it hardcoded `len(instIDs) * 4` — a guess
sized for production's small live roster (a handful of assigned strategies per token) — regardless
of how many kinds a screening run actually passes. This screening run passes 44 (46 registered,
minus 2 that fire zero trades on this window), so real concurrent capacity was `10 * 44 = 440`
slots against the formula's answer of 40 — an 11x overcommitment that sized every position roughly
11x too large relative to what the $40 account could actually support if every slot filled, since
`internal/backtest.openPosition` sizes each new position as `account / PositionSlots` regardless of
how many kinds are truly running (`position.go:63`).

Confirmed this is sizing-only, not a second execution bug: a controlled pair (1 instrument, 1 kind,
`-account 40` vs `-account 4`, i.e. the same 10x ratio as the slot-count error) produced identical
win rate and PnL scaled by exactly 10x — proving trade selection is untouched and only dollar sizing
was wrong. Also independently verified the dollar-PnL FORMULA itself (`pnlPct(p, exit).Mul(p.size)`
in `backtest.go`'s `realizedPnL`) against production's `usecase.papertrade.go`'s `grossPnL` — same
formula, leverage applied exactly once in both, not a double-counting bug as first suspected and
then ruled out by direct comparison.

**Fix**: `positionSlots(instIDs, kinds []string) int` now counts `len(instIDs) * len(kinds)`,
falling back to `len(strategy.Factories)` when `-kinds` is empty (matching `main`'s own "empty
means every registered kind" convention) instead of a fixed per-token guess. Also added `-account`
(overrides `cfg.Account.InitialUSD`) so a screening run's account can scale with its own, wider
roster independently of production's real $40 — and fixed the startup log line, which was still
printing `cfg.Account.InitialUSD`/`cfg.Risk.MaxLeverage` (the pre-override config values) instead of
`runner.Cfg`'s actually-applied ones, a real bug found only by using the new `-account` flag myself
and noticing the log didn't reflect it. 3 new tests in `cmd/backtest/main_test.go`, mutation-checked
(reverting to the old `* 4` formula fails both the explicit-kinds and empty-kinds cases with the
exact wrong numbers). 898 Go tests pass repo-wide, `go build`/`go vet` clean.

### Roster decision (operator, 2026-09-15): drop V1 where a V2 exists, target $4/slot

Of the registry's 46 total kinds, 12 have a `_v2` sibling that is a data-driven revision of the same
idea (§45's history: ATR-scaled levels, a bounded reward:risk, mostly a regime filter). Operator's
instruction: exclude the 12 superseded V1s from screening/training entirely — keep them registered
(so historical/production rows referencing them still resolve, per §11.3's locked-origin rule) but
do not spend training-data budget on a strategy whose own revision is running instead. This leaves
**34 kinds**. On the 10-token roster that is `34 * 10 = 340` slots; at the operator's target of $4
per slot, the screening account is **$1,360** — independent of production's real $40, exactly what
`-account` was added for.

### Corrected screening run (2026-09-15/16): `docs/STRATEGY_STATS.md`'s table is retired

Re-ran the full screening (34 kinds excluding V1-of-V2, 10 tokens, `-account 1360`, `-dry`) with
both fixes from this session (`signalProfile`, `positionSlots`) in place — this superseded
`STRATEGY_STATS.md` outright, not just its numbers. Full per-strategy result:

```
kind                        trades    win%   pnl/tr($)       t  sig
sweep_reverse                   13   46.2%    +0.02866   +0.92
macd_momentum_v2               406   40.6%    -0.00219   +2.19  *
sma_cross_fixed_exit          1069   33.2%    -0.01126   +2.13  *
pmax                           450   29.3%    -0.01264   +1.25
rsi_sma                        316   33.9%    -0.01482   +0.86
rsi_sma_fuzzy                  476   33.6%    -0.01530   +1.00
confluence                    1707   29.5%    -0.01585   +1.79
btc_divergence                 759   28.1%    -0.01660   +1.10
ict_order_block_v2            1817   35.6%    -0.01716   +1.58
stoch_cross                   2349   60.4%    -0.01748   +1.73
btc_divergence_fade           3176   28.7%    -0.01774   +1.94
grid_like                     1678   33.1%    -0.01835   +1.29
inside_bar_breakout_v2        3501   34.8%    -0.02008   +1.38
pivot_reversal                1026   33.9%    -0.02082   +0.63
ema_ribbon_pullback_v2        2880   35.5%    -0.02114   +0.98
ict_liquidity_sweep_v2        2610   37.3%    -0.02154   +0.83
session_momentum              1418   28.7%    -0.02263   +0.42
keltner_trend_scalp_v2        4372   35.4%    -0.02372   +0.39
ict_fvg_v2                    3846   32.4%    -0.02473   +0.07
engulfing_reversal_v2          754   34.9%    -0.02495   +0.00
coin_flip                     2732   32.1%    -0.02496     n/a
volume_breakout_v2            1509   32.8%    -0.02532   -0.07
range_breakout_v2             2965   31.8%    -0.02577   -0.21
gradient_ribbon                 43   25.6%    -0.02673   -0.06
weekly_dip_buy                2002   27.7%    -0.02689   -0.41
dual_ma_atr                    877   44.8%    -0.02777   -0.40
trendshift                    1194   31.3%    -0.02779   -0.47
ema_cross_trailing             809   47.7%    -0.02913   -0.57
bb_squeeze_breakout_v2         553   30.9%    -0.03507   -1.13
stepped_trailing               471   20.4%    -0.06164   -3.79  *  (significantly WORSE)

overall: 48,441 open decisions, win_rate=34.6%, total_pnl_usd=-$1,059
```

**No kind is significantly profitable.** `macd_momentum_v2`/`sma_cross_fixed_exit` clear `|t|>=2` but
both still have negative PnL/trade — statistically distinguishable from `coin_flip`, not from
breakeven. `pmax`, the sole significant-positive kind in the OLD table (t=+3.43), is no longer
significant at all (t=+1.25) once measured correctly. Only `stepped_trailing` clears significance,
and in the wrong direction. This matches §16.1's own precedent (the first screening's whole table
sitting under |t|=1) — reading either table as a ranking would be selecting on noise.

**Operator's decision, given this: keep the "diversity over rank" selection principle** used for
`STRATEGY_STATS.md`'s original pick, re-derived from the corrected table. Final roster (8 kinds,
chosen for distinct market read since none is provably better than another):

| kind | style |
|---|---|
| `macd_momentum_v2` | momentum |
| `trend_confluence` | structural/trend |
| `vwap_reversion_v2` | mean-reversion (VWAP) |
| `sma_cross_fixed_exit` | trend-cross |
| `pmax` | volatility/trailing |
| `confluence` | multi-signal combiner |
| `stoch_cross` | oscillator (highest win rate, 60%, still loses on R:R) |
| `btc_divergence` | cross-market (BTC-relative) |

Excluded for style overlap with a kept pick: `grid_like` (mean-reversion, overlaps `vwap_reversion_v2`),
`btc_divergence_fade` (overlaps `btc_divergence`), `rsi_sma_fuzzy` (oscillator, overlaps `stoch_cross`).
`stepped_trailing` excluded as the one proven-worse-than-chance kind. Tokens unchanged from
`STRATEGY_STATS.md`'s own token-level finding (DOGE/ZEC/PEPE/PUMP/BTC/SOL, TRUMP excluded at 8.2
standard errors below the mean) — that result is about instrument volatility, not strategy
selection, and wasn't affected by either fix.

8 strategies x 6 tokens = 48 slots; at $4/slot the roster's own training account is **$192**.

### Real warm-start dataset built (2026-09-16, not yet trained on for real)

```
go run ./cmd/backtest -inst DOGE,ZEC,PEPE,PUMP,BTC,SOL -bars 5m \
  -kinds macd_momentum_v2,trend_confluence,vwap_reversion_v2,sma_cross_fixed_exit,pmax,confluence,stoch_cross,btc_divergence \
  -account 192 -out data/warmstart_v8.jsonl
```

**53,965 samples** (2,906 completed trades plus their `update` samples), built on the server in
~90 seconds, transferred to the operator's machine via gzip (254MB -> 6.4MB — JSONL compresses
extremely well on repeated field names; the uncompressed transfer over the VPN was measured at
~85KB/s and would have taken ~30 minutes, the compressed one under a minute). File lives at
`rl-service/data/warmstart_v8.jsonl`, gitignored (`/rl-service/data/`).

### Entropy-collapse re-check: PASSED across 3 seeds (2026-09-16)

Operator's specific ask before trusting any of the above: does §54.8's entropy collapse
(`target_entropy=-8` default -> alpha 1.0 -> 0.0009 in 66 steps) recur now that the dataset bugs
are fixed and `target_entropy=-4.5` is the baked-in default? Confirmed BOTH halves of the existing
fix are still in place before testing anything: `rl_service/config.py:111`
(`target_entropy: float = -4.5`) and `serve/api.py`'s `reset_entropy_coef: bool = True` default,
which resets alpha to 1.0 on every model load — both apply automatically, no action needed to
"turn them on" for this dataset.

Built `rl-service/tools/check_entropy_stability.py` (promoted from a one-off scratch script,
kept in the repo since future dataset/config changes will want this same check again) — trains a
fresh SAC model on a given dataset in stages, logging alpha at each cumulative step count. Run
against `warmstart_v8.jsonl` with 3 independent seeds (42, 1, 100), each in its own process
(§15.11's own operating note: multiple SAC models plus replay buffers must not share a process):

```
                      seed 42   seed 1   seed 100
cumulative_steps=0      1.000    1.000      1.000
             ~1000      0.744    0.744      0.744
             ~5000      0.301    0.232      0.233
            ~10000      0.086    0.069      0.070
             19100      0.065    0.056      0.061
```

**All three seeds converge to a similar, stable alpha (0.056-0.065) with a smooth decline —
no collapse.** The three seeds tracked each other almost exactly at every checkpoint (0.744 at
1000 steps for all three; within 0.001 of each other at 5000), which is itself informative: this
is not seed-sensitive behavior the way the earlier five training runs were (`rank_t` swinging
-4.72 to +3.94 across seeds on the OLD, buggy dataset). §54.8's failure mode does not recur.

Test model artifacts (`sac_v8_entropy_check_seed*.zip`, `sac_v8_test*.zip`) were diagnostic-only
and deleted after the check — not meant to be trained further or deployed; `models/` is gitignored
regardless.

### Next
1. **Train the real model** on `warmstart_v8.jsonl` (not the throwaway entropy-check runs) —
   using `rl_service.warmstart` proper (not the diagnostic tool), producing a real checkpoint,
   before any live deployment.
2. **Show the operator results before any deploy** — win rate the model would have taken vs.
   declined, per the success criterion already defined (§"Define success before training starts"):
   the win rate of DECLINED trades must be measurably lower than TAKEN ones, or the model learned
   nothing.
3. **Open question, raised by the operator 2026-09-16, not yet decided**: whether connecting to
   TradingView (its strategy/indicator library, its own backtesting) would materially help beyond
   what this project's own 34-kind measurement already found. Given the measured 3.2pp gap to
   breakeven and that NO existing strategy (including the TradingView ports already added, §30) is
   significantly profitable, the working hypothesis is that the binding constraint is signal
   SELECTION (declining the worst ~20% of takes), not signal SOURCE — more indicators from a
   different platform are not obviously the missing piece, though a genuinely novel idea (not
   another single-pattern entry rule, which is what all 34 current kinds already are) could still
   be worth adding for diversity, the same reasoning that justified the original TradingView ports.
   Revisit after step 2's result is in — if the model can't close the 3.2pp gap with the current
   roster, that's the concrete evidence for or against needing new strategy sources.
