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
