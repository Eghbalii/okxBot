# Strategy backtest/optimize pipeline — handoff (updated 2026-09-28, session 3)

Status: **deployed and running live**. Written so a new session can resume without re-deriving
context. Supersedes the previous version of this doc for everything covered below; sections not
mentioned here are unchanged from prior sessions. Session 2's own header still applies for its own
listed items.

## Session 3 (2026-09-28) — three real bugs found and fixed, all deployed and verified live

Triggered by two separate operator reports in the same session: (1) the CPU-lockup/Kafka-outage
investigation from earlier the same day (see the `strategy_optimizer` config section below —
unrelated code, same session), and (2) two direct complaints about this pipeline specifically:
"the previous session's id-suffix 'fix' for duplicate names was a lazy hack, the real fix is
making the generation number actually increment," and "the panel's live-trade numbers look wrong
— only ~5 signals total across 118 active candidates, but paper trading is clearly opening
hundreds of positions a day."

### 1. CRITICAL BUG (root cause of the id-suffix hack): every candidate landed on the same
generation number, forever

`proposeCandidate` (`internal/optimizer/loop.go`) computed a new candidate's generation as
`origin.Generation+1` — **always** the origin's own generation plus one, never the lineage's own
history. Since the origin's generation never changes, every single candidate ever proposed for one
lineage landed on the identical number (2), which is exactly what forced the previous session's
`#<id>` suffix onto `DisplayName` to disambiguate two genuinely different candidates that otherwise
rendered as the same string.

**Fixed properly**: `Store.MaxGeneration(ctx, lineage)` (new, `internal/optimizer/store.go`) reads
`MAX(generation)` across the lineage's ENTIRE history (a real aggregate, not "the most recent row",
which would be fragile to insertion order); `proposeCandidate` now uses `MaxGeneration+1`. Verified
live: freshly-proposed candidates for lineages that already had a generation-2 attempt now correctly
land on generation 3 (`keltner_trend_scalp_v2_FIL_R10_G3_B0_P0`).

**The `#<id>` suffix was then REMOVED from `DisplayName`** (explicit operator instruction, same
session, after confirming the real fix works) — it was never a version number, just a workaround for
a bug that no longer exists. `DisplayName` is now exactly
`<kind>_<instId>_R<leverage>_G<generation>_B<backtestUpdates>_P<paperUpdates>`, no trailing id.
Deployed and verified live via the real `/candidates/by-status` endpoint.

5 new Go tests (`internal/optimizer/loop_test.go`), all mutation-checked (each fails with its own
fix reverted, reproducing the exact original bug — e.g. `[2 2 2]` instead of a strictly increasing
sequence): `TestLoop_ProposeIncrementsGenerationAcrossRejectedAttempts`,
`TestStore_MaxGeneration_NoCandidatesYetReturnsOne`,
`TestStore_MaxGeneration_ReflectsTheHighestCandidateEverCreated`. Verified against a real
Postgres instance (a throwaway `timescale/timescaledb:latest-pg16` container via Colima, migrated
with the project's own embedded migrations, NOT the production database) since this package's
tests need a real database and have no rollback/isolation between runs — a real gotcha hit while
writing these: hardcoded trial IDs (101/102/103) collided across repeated runs against a persistent
test DB, producing a false failure (`[5 2 6 3 7 4]`) that had nothing to do with the fix itself;
fixed by keying the test lineage's `InstID` off `time.Now().UnixNano()`.

### 2. CRITICAL DESIGN GAP (explicit operator decision): auto-promotion never waited for real
live-trading data before replacing an active candidate

Root cause: `runAndRecord`'s auto-promote call (`internal/optimizer/loop.go`) called `Promote()` the
instant a new candidate passed its backtest, with **zero** regard for whether the currently-active
candidate had accumulated any real closed live trades of its own. A backtest pass says nothing
about whether the running strategy is actually worse — only real trading data can answer that, and
under the old behavior a strategy could be promoted and replaced again within the same hour with no
live track record ever collected.

**Fixed**: new `ValidationConfig.MinLiveTradesBeforeReplace` (migration `000041`, panel-editable,
default 30 — same default as `MinTrades`). `Loop.readyToReplaceActive` checks the active
candidate's `StrategyStatsFor` (real `paper_orders`, `Wins+Losses`) before `Promote()` is ever
called; if the active candidate hasn't cleared the threshold yet, the new candidate stays
`backtest_passed` (visible, not discarded) and the active one keeps running untouched, no matter how
good the new backtest looks. Once the threshold is cleared, replacement proceeds normally.

2 new Go tests, mutation-checked: `TestLoop_DoesNotReplaceActiveCandidateWithTooFewLiveTrades`,
`TestLoop_ReplacesActiveCandidateOnceItHasEnoughLiveTrades` (seeds real closed `paper_orders` rows
via `OpenPaperOrder`/`ClosePaperOrder` to prove the gate lifts once enough live trades exist).

**Side fix found while wiring this**: `ValidationConfig` (`internal/optimizer/store.go`) had NO
JSON tags at all, so `json.Encode` wrote capital-first Go field names (`MinTrades`, `RiskProfile`,
...) while the panel's `OptimizerValidationConfig` TypeScript type expects camelCase
(`minTrades`, `riskProfile`, ...) with no case translation anywhere in the proxy chain — the panel's
entire "Validation config" edit form had been silently reading every field as `undefined` since it
was built. Fixed by adding explicit `json:"..."` tags matching the panel's existing type.

### 3. CRITICAL DATA-INTEGRITY BUG: 914 strategies were trading live completely outside the
optimizer's own tracking — this is what made the panel's live-data numbers look wrong

Investigating the operator's "only ~5 live signals shown, but clearly way more trades are
happening" report found the panel was actually correct — the bug was on the trading side, not the
display side. `paper_orders` showed **2,429 closed positions in 24h across 225 distinct
strategy_ids**, but only **6** of the optimizer's own 118 `paper_active` candidates had any real
trade at all. The other ~2,400 trades came from **6,220 `strategies` rows** all created in a tight
~2-minute burst at 07:46–07:48 the same morning — a one-time bulk-clone event, NOT part of any code
currently in this repo (no matching commit, no matching code path found) and NOT tracked anywhere
in `strategy_candidates` at all. 914 of those 6,220 had live, enabled `strategy_assignments` and
were opening/closing real paper positions completely outside the optimizer pipeline's promotion
logic, generation numbering, or the live-trade gate from fix #2 above — invisible to the panel
because the panel (correctly) only ever reads the real `strategy_candidates` table.

**Fixed per explicit operator instruction** ("بله همه رو پاک کن و از استراتژی‌های جدید استفاده
کن" — paper trading strategy/token control belongs to the optimizer, full stop): all 914 ghost
assignments were disabled (`strategy_assignments.enabled = false`, `mode='paper'`), backed up first
to `strategy_assignments_ghost_backup_20260928` (a full row dump, not deleted — this project's own
"never delete, keep for audit" convention). `paper-trader` restarted. Verified: enabled paper
assignments dropped from ~1034 to exactly 120, of which 118 are the real optimizer candidates (the
remaining 2 are origin bootstrap rows for kinds with no tunable params — expected, not a leftover).

**Not fully root-caused** — the exact script/process that created the 6,220 rows at 07:46-07:48 was
never found in this repo's git history. Worth keeping an eye out for it if it recurs (check for
`_G1_B0_P0`-named `strategies` rows created in a tight timestamp cluster, `cloned_from` pointing at
a real `is_origin=true` row — that's the exact signature). If it does recur, the fix is the same
query pattern used this session (filter `strategies.name LIKE '%_G1_B0_P0'` + a `created_at`
cluster, disable the resulting `strategy_assignments`, never delete).

## Open items for next session

- Confirm the 118→~120 promoted-candidate paper trading volume actually ramps up over the next
  24-48h now that the ghost strategies are gone and the real pipeline is the only thing trading —
  this was NOT verified post-fix (deployed right at context limit) beyond confirming the assignment
  counts and a clean `paper-trader` restart. Check `paper_orders` closed-trade volume and distinct
  `strategy_id` count again in the next session; it should now show ~118-120 distinct ids, not 225.
- The exact source of the 6,220-row 07:46-07:48 bulk-clone burst is still unknown — see above.
- `strategy_assignments_ghost_backup_20260928` and `strategy_assignments_backup_20260928` (an
  earlier session-2 backup table, different rows) are both sitting in the production database —
  fine to leave (matches this project's audit-trail convention) but worth knowing they're there if
  disk usage on the DB ever needs auditing.

## What this session found and fixed (in order)

### 1. Panel showed no new candidates / no promotions ever happened
Root cause: promotion was entirely manual (a Promote button nobody had ever clicked), and the
panel only showed `paper_active` candidates with no way to browse `backtest_passed` ones. Fixed:

- **Auto-promotion**: `internal/optimizer/loop.go`'s `runAndRecord` now calls `Promote()`
  automatically the instant a candidate passes validation — no manual click needed.
- **`Loop.SweepUnpromoted`** (`internal/optimizer/loop.go`): a one-time startup catch-up that
  promotes the best already-`backtest_passed` candidate per lineage that has no active candidate
  yet. Runs once at `cmd/strategy-optimizer` startup, before the scheduler's tick loop.
- **`Repository.DisableOriginAssignments`** (`internal/postgres/strategies.go`): a one-time
  startup sweep disabling every enabled origin-strategy paper assignment, so paper trading stops
  running the old, untuned strategies once real candidates exist to replace them.
- New endpoint `GET /candidates/by-status?status=X&limit=N` (`internal/optimizer/store.go`'s
  `ListCandidatesByStatus`, `cmd/strategy-optimizer/handlers.go`'s `handleListByStatus`) — the
  panel needed to list across ALL lineages by status, not just one exact lineage at a time.
- New endpoint `GET /backtest-capital` — reports what dollar amount every backtest actually sizes
  one position at (`account.initial_usd × account.max_position_pct`), so the panel can show PnL %
  alongside PnL $ (operator's explicit question: "با چقدر سرمایه این سود بدست اومده؟").
- Scheduler (`cmd/strategy-optimizer/scheduler.go`) parallelized to 4 workers (`tickWorkers`) —
  was fully sequential over ~5,394 lineages, taking well over an hour per pass on this 2-core
  server, which made the panel look frozen even though the pipeline was working.

### 2. CRITICAL BUG: the sweep wrongly promoted 6,216 empty-config origin placeholders
`EnsureOriginCandidate` stamps every lineage's placeholder row `status='backtest_passed'` directly
(source: `'origin'`, config `{}`) — never a real tuned result. The first version of
`SweepUnpromoted` didn't filter these out, so it "promoted" thousands of empty placeholders as if
they were real candidates. **Fixed**: `SweepUnpromoted` now skips `c.Source == "origin"`, and
`Promote()` itself refuses to promote a `source == "origin"` candidate as a second line of defense.
Bad production data (6,216 rows) was reverted directly via SQL. Regression test:
`TestLoop_SweepUnpromoted_SkipsOriginPlaceholder`.

### 3. CRITICAL BUG: `SetAssignmentsEnabledForKinds` re-enabled origins on every restart
`cmd/paper-trader`'s startup calls this with the operator's `active_kinds` allowlist. Its own
`UPDATE ... SET enabled = (s.kind = ANY($1))` matched origin rows too (activeKinds has nothing to
do with origin-vs-promoted), silently re-enabling every origin assignment the pipeline had just
disabled, on every single `paper-trader` restart. **Fixed**
(`internal/postgres/paper_trading_config.go`): in `mode="paper"`, both the UPDATE and the INSERT
half now explicitly exclude `is_origin` rows — this function's original bootstrap behavior is
preserved for `mode="bot"` (real trading, no pipeline there yet). Regression tests:
`TestSetAssignmentsEnabledForKinds_PaperModeNeverTouchesOrigins`,
`TestSetAssignmentsEnabledForKinds_BotModeStillBootstrapsOrigins`.

### 4. CRITICAL BUG (the real root cause): `Promote()` never disabled origin on FIRST promotion
Found live, AFTER fixing #3, by observing fresh paper orders still opening under origin strategy
names. `Promote()`'s existing "demote the previous active candidate" logic
(`hadPrevious`/`ActiveCandidate`) only covers a lineage's SECOND-and-later promotion — the origin
itself is never tracked as a `paper_active` candidate, so on a lineage's first-ever promotion there
was nothing to demote, and origin + new clone traded the same lineage side by side forever.
**Fixed** (`internal/optimizer/promote.go`): `Promote()` now unconditionally calls
`disableAssignmentFor(ctx, repo, originID, ...)` on every promotion, regardless of whether there
was a previous candidate. Regression test: `TestPromote_DisablesOriginAssignmentOnFirstPromotion`
(mutation-checked: fails without the fix, passes with it — but ONLY when using a kind unique to
the test; reusing "rsi_sma" collided with loop_test.go's own seeding and produced a false
pass/fail depending on test run order — watch for this pattern in future optimizer tests).

**Verified live end-to-end**, not just via tests: after both fixes deployed, three genuine fresh
promotions (`trendshift`/BTC, `ict_liquidity_sweep_v2`/RENDER, `ict_liquidity_sweep_v2`/ONDO) were
observed, and for each, the origin's assignment was confirmed `enabled=false` in production while
only the new clone was `enabled=true`. The origin-with-real-replacement-clone count query (see
below) stayed at 0 throughout.

**Useful verification query** for "is any origin still trading despite having a real replacement":
```sql
select count(*)
from strategy_assignments sa_origin
join strategies s_origin on sa_origin.strategy_id = s_origin.id
where s_origin.is_origin = true and sa_origin.mode = 'paper' and sa_origin.enabled = true
  and exists (
    select 1 from strategy_assignments sa_clone
    join strategies s_clone on s_clone.id = sa_clone.strategy_id
    where s_clone.is_origin = false and s_clone.kind = s_origin.kind and s_clone.enabled = true
      and sa_clone.mode = 'paper' and sa_clone.enabled = true
      and sa_clone.inst_id = sa_origin.inst_id and sa_clone.bar = sa_origin.bar
      and sa_clone.exchange = sa_origin.exchange
  );
```
Should always read 0. If it doesn't, something regressed in `Promote()` or the assignment cleanup.

### 5. Panel improvements (operator's explicit list, all shipped)
- **Pagination + sorting** on every tab (25 rows/page). Active tab has 5 sortable columns split
  into backtest-time vs. live-since-promotion groups.
- **Auto-refresh confirmed working** (was suspected broken — it wasn't; the underlying data was
  genuinely static between polls because promotions are rare). Added a visible
  "checked HH:MM:SS" timestamp next to the header so this is obvious without having to trust it.
- **Removed the old "Backtested, not promoted" and "Rejected" tabs** — replaced with a single
  **"Rejected, was trading"** tab pointed at `status='paper_replaced'` (candidates that DID
  actually trade in their backtest and were later superseded by a better one) instead of the raw
  `backtest_rejected` pool, most of which never opened a single trade and told the operator
  nothing useful. `backtest_passed`/`backtest_rejected` browsing (which can run into the tens of
  thousands) is still reachable via `GET /candidates/by-status` directly if ever needed, just not
  surfaced in the panel UI anymore.
- **Leverage column replaces Risk** — `candidateView` (`cmd/strategy-optimizer/handlers.go`) gained
  a `leverage int` field (10 for "low"/OKX, 100 for "high"/MEXC), computed the same way
  `DisplayName`'s `R<leverage>` component already was, just also exposed as its own field.
- **New "Period" column** — `candidateView` also gained `backtestFrom`/`backtestTo` (already
  existed on `optimizer.Candidate`, just not exposed over the API before), rendered client-side as
  "Nd" (days between them) — answers "over how many days was this PnL earned".
- **Distinct Backtest vs. Live column styling** — `App.css` gained `.col-group-backtest`/
  `.col-group-live` (colored + text-shadow group headers) and `.col-cell-backtest`/
  `.col-cell-live` (faint background tint carried into the cells below).
- **Clicking a strategy name opens `ParamChangeModal`** (new component,
  `panel/src/components/ParamChangeModal.tsx`) — reuses the EXISTING `GET /api/strategies/:id/
  param-changes` endpoint and `ParamChange` type (§16.7's manual-edit-tracking table), not a new
  mechanism. Shows old-value vs. new-value per parameter, one row per change, newest first.
  `Promote()` now also writes to this same table on every promotion (old candidate's config as
  `OldConfig`, new candidate's as `NewConfig`, `source='optimizer'`) — this table previously only
  ever got manual-edit rows, so promoted-candidate history was empty before this change; existing
  history for candidates promoted before this fix does not retroactively exist.
- **Old `ParamChangeChart.tsx` (the price-chart-with-markers) and its `StrategiesPage.tsx`
  wrapper removed entirely** — operator's explicit "دیتای مفیدی نمیده بهمون". `StrategiesPage.tsx`
  is now just the mode tabs + `<OptimizerPanel />`, nothing else.

## Panel round 2 (same day, operator feedback on round 1's styling)

- Group header backgrounds are now solid colored + `box-shadow`, not just colored/shadowed text
  (`.col-group-backtest`/`.col-group-live` in `App.css`).
- Thick borders (`.col-edge-start`/`.col-edge-end`, applied explicitly per-cell in JSX, not via
  CSS `:first-child`/`:last-child` — every `col-cell-*` td is adjacent so those pseudo-selectors
  would key off the row's children, not the group's) make each column group read as one
  contiguous block.
- Leverage/Period columns moved under the Backtest group header (`colSpan={5}` now, was 3)
  — they describe the backtest, not the live position, per operator's explicit correction.
- Token/Bar split into two separate columns everywhere (was one combined "Token / bar" cell).
- **Rejected tab "no data" investigated and fixed**: the trade count/win rate/PnL data was
  ALWAYS present in the database and the raw API response for every `paper_replaced` candidate —
  verified directly, not assumed. What was actually missing was a reject/replace REASON column.
  `backtest_rejection_reason` only ever gets set for `backtest_rejected` candidates (never traded
  at all); a `paper_replaced` candidate traded successfully and was later superseded, so that
  column was always empty for it — reading as "no data" even though the real trade stats were
  right there. Fixed with a genuinely new, service-computed field:
  `Store.ReplacedByFor` (`internal/optimizer/store.go`) finds whichever candidate replaced a given
  `paper_replaced` one for the same lineage (by `promoted_at` ordering), and
  `handlers.go`'s `replacedReason` builds a real comparison sentence from both candidates' own
  recorded stats — e.g. live-verified: *"Replaced by X ($23.29 PnL, 46.8% win rate over 111
  trades) — this one scored $23.59 PnL, 47.2% win rate over 127 trades"*. Note this specific
  example shows the REPLACEMENT scoring slightly worse than what it replaced — worth the operator
  looking into separately (is `Promote()`'s "better candidate" comparison logic actually always
  picking the better one?), not something investigated or fixed in this pass.

## Panel round 3 (same day) — DisplayName collision bug, real root cause of "Rejected looks wrong"

The operator was right to be suspicious. **Confirmed bug, fixed**: `Candidate.DisplayName()`
(`internal/optimizer/store.go`) was built from
`Kind_InstID_R<leverage>_G<generation>_B<backtestUpdates>_P<paperUpdates>` — NONE of which are
guaranteed unique within one generation. `proposeCandidate` routinely proposes several different
tuned parameter sets in the same generation before one passes validation, so two completely
different candidate rows (different ids, different real stats) rendered as the **exact same
string**. Live-confirmed example: candidate 27617 (`paper_replaced`, $79.96 PnL, 53.3% win, 30
trades) and candidate 40200 (`paper_active`, $10.64 PnL, 46.9% win, 32 trades) are both
`coin_flip/DASH`, generation 2, `B0_P0` — both rendered as `coin_flip_DASH_R10_G2_B0_P0`, making
the `replacedReason` sentence look like it was naming the same strategy as both the "replaced" one
and the "replaced by" one, when they were actually two distinct rows the whole time (the
underlying `ReplacedByFor` SQL lookup itself was correct — verified by timestamp: both share the
exact `now()` from `PromoteCandidate`'s one transaction, confirming 40200 really is what replaced
27617). **Fix**: `DisplayName` now appends `_#<id>` (the candidate's own always-unique database
id) — minimal fix, no new counter/migration needed. Deployed same day; not yet re-verified against
a fresh live example post-fix (do this first thing next session: pull a few `paper_replaced` rows'
`replacedReason` and confirm the two names in each sentence now differ whenever the underlying ids
differ).

**Still open / worth a closer look next session**: the operator separately asked for a raw
trade-data column on the Reason side (not just embedded in the sentence) — not done this round,
deferred given the DisplayName fix was the higher-priority root cause and context ran out. Also
worth re-examining once names are trustworthy: is `Promote()`'s own "pick the best candidate"
logic ever promoting a candidate that scores WORSE than the one it replaces? The pre-fix example
above (79.96/53.3%/30 replaced by 10.64/46.9%/32) suggested this, but re-verify with the naming
bug fixed before concluding anything — it's possible what looked like "worse replaced by worse"
was actually reading two different unrelated candidates' numbers side by side due to the name
collision, not a real promotion-logic bug. Don't assume either way; check fresh.

## Known remaining gap

- **`ParamChangeModal` will show "no recorded parameter changes"** for every candidate promoted
  BEFORE this session's `Promote()` change shipped — only new promotions write to the timeline
  going forward. Not retroactively backfillable (there's no historical "what was the previous
  generation's config" data to reconstruct for old promotions beyond what `strategy_candidates`
  itself already has via `parent_candidate_id`, which a future pass could use to backfill if
  wanted).
- **Backtested/Rejected browsing by raw validation outcome** no longer has a panel view at all
  (only `paper_active` and `paper_replaced` do). If the operator wants to review why a specific
  candidate failed validation, that's `GET /api/optimizer/candidates/by-status?status=
  backtest_rejected` directly, not the panel.

## Load/resource notes (server is genuinely tight)

- **Disk repeatedly hit 90-100% full during this session's builds** (one build ran with a
  confirmed 0 bytes free for several minutes — no service crashed, Kafka's restart count never
  moved, but this was genuinely one build away from a real incident). Root cause found on the
  SECOND round of this: **`docker builder prune -f` alone does NOT reliably reclaim build cache on
  this box** — it repeatedly reported 5-6GB of cache as 0B reclaimable even right after a build
  finished. **`docker builder prune -af` (the `-a` flag) is what actually works** — one run
  recovered disk from 100% (0 free) to 75% (6.0G free) instantly. **Use `-af`, not `-f`, after
  every single build on this box, no exceptions.**
- **Kafka crash-looped at least twice this session** under combined CPU (strategy-optimizer's tick
  load) and disk pressure, both times self-recovering within seconds. `trader`/`paper-trader` both
  correctly retried with backoff per the existing §28 fix — no data loss either time, but this is
  a recurring pattern worth taking seriously if lineage count or build frequency grows further.
- Standing procedure for this box, reconfirmed: check `df -h /` and `free -h` before every build,
  stop `grafana`/`prometheus` (and `strategy-optimizer` itself, to avoid it competing with its own
  rebuild for CPU) before building, one service at a time, `docker builder prune -f` immediately
  after every single build completes, never skip this even under time pressure.

## Everything else from the previous handoff (load scoping, two-phase funnel idea, open items about
the MEXC top-20 hardcoded list, validation thresholds still at seeded defaults) is UNCHANGED —
refer to git history for the original doc if needed, or re-read this file's prior version via
`git log -p -- docs/NEXT_SESSION_STRATEGY_OPTIMIZER.md` once this update is committed.
