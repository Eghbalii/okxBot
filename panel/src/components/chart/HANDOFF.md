# OpenAlgo chart engine — handoff (2026-09-21, next session start here)

Written because this session ran low on context mid-fix. Read this before touching
`components/chart/` again — it says exactly what is done, verified, still broken, or not
attempted, so the next session doesn't re-derive it from scratch or re-report something already
fixed as new.

## CORRECTION (2026-09-21, same day, follow-up session) — the zone bug below is FIXED

The "STILL BROKEN" section below was wrong about the cause. The operator found it, not this
session: `createWidget`'s default `timezone` is the fixed IANA zone `'Asia/Kolkata'` (confirmed in
the engine's own `dist/index.d.ts` doc comment — "Absent means the shipped default ('Asia/
Kolkata')"), and nothing in the widget-creation call ever overrode it. Every axis/crosshair label
this chart printed was ~5.5h off from the actual UTC time the bar/marker/zone were really drawn
at — the box's own pixel math (already root-caused as correct earlier this session) was right the
whole time; the LABEL next to it was lying, which is what made it look mismatched. Fixed with one
line: `timezone: Intl.DateTimeFormat().resolvedOptions().timeZone` in the `createWidget` options
(`OpenAlgoCandleChart.tsx`), matching the browser's own local zone the same way
`CandleChart.tsx`'s `toLocaleString(undefined, ...)` helpers and the TV engine already do. Verified
against a fresh live position (LTC #2307, opened 18:40:01) — the box's left edge now lines up with
the entry marker and the axis correctly reads "06:40 PM".

Also removed the widget's own built-in status line (`statusline: false`) per the same report — it
duplicated this component's own OHLCV readout at the bottom of the chart in the exact same
Asia/Kolkata-mislabeled way.

The "plausibility filter" (`plausiblePositions` in `OpenAlgoCandleChart.tsx`) from earlier this
session was NOT removed — it's an unrelated, still-valid guard against the separate
`TokenChartModal` stale-candle race documented below, kept as defense-in-depth.

The debugging section below is left as-is (per this project's own practice of appending
corrections rather than rewriting history) but its root-cause guesses were superseded by the above
— don't spend time re-investigating them.

## Deployment state

Deployed to the server and running as of this session's last commit. `docker-compose.yml`'s panel
build has NO `VITE_CHART_ENGINE` build-arg — TradingView (`lightweight-charts`) is still the
build-time default and the only engine any fresh `docker compose build panel` ships alone.
OpenAlgo is reachable only via the runtime toggle (`ChartEngineToggle`, persisted to
`localStorage` as `okxbot.chartEngine.v1`, or `?engine=openalgo` in the URL) — both engines'
code is lazy-loaded as separate chunks (`CandleChart-*.js` ~58kB gzip, `OpenAlgoCandleChart-*.js`
~212kB gzip), so picking one never costs the other.

**Deploy procedure used throughout this work, keep using it**: check `ssh okx 'free -m'` first: if
under ~1.5GB free, `docker compose stop grafana prometheus` before building, `docker compose build
panel` (one service, never build two at once — see CLAUDE.md §35.7, this box has OOM'd from
concurrent builds before), `docker compose up -d panel` (not `restart` — a source change needs a
new container, not a restart of the old one), then `docker builder prune -f`, then verify
`docker inspect okxbot-panel-1 --format '{{.Image}}'` matches `docker images okxbot-panel --format
'{{.ID}}'` before declaring it deployed. Resume `grafana`/`prometheus` after.

**Browser verification note**: this session drove the server's real browser session over an SSH
`-L 18080:127.0.0.1:8080` tunnel to `okx`, using the ambient Playwright install at
`/Users/rez/.npm/_npx/705bc6b22212b352/node_modules/playwright` (no `playwright` package installed
in this repo or globally — `chromium-cli` was not available either). **The tunnel dropped
repeatedly under sustained use** (multiple `ssh -f -N -L ...` restarts were needed across this
session, each surviving maybe 5-15 requests before going dead with no warning). If picking this
back up: expect to restart the tunnel often, and don't trust a `curl` 200 five minutes ago to mean
it's still up now — check immediately before each Playwright run. The widget itself also
genuinely takes 5-10+ seconds to mount over this tunnel (fetching the 780KB raw chunk plus
`createWidget`'s own setup) — a `waitForSelector('.oac-widget', { timeout: 20000 })` was
frequently NOT enough; 40000ms was what reliably worked in this session.

## Fixed and verified this session (screenshots taken, confirmed correct)

1. **Volume histogram added to the TradingView engine** (`CandleChart.tsx`) — was entirely
   missing before this session. Overlay price scale (`priceScaleId: ''`), colored by that bar's
   own direction.
2. **OpenAlgo engine built from scratch** (`OpenAlgoCandleChart.tsx`, `OpenAlgoZones.ts`) —
   candles, volume, entry/exit markers with hover tooltip, SL/TP risk/reward zone boxes with
   drag-to-adjust (via the engine's native `chart.subscribeDrag`, not manual pointer capture like
   the TV engine needs).
3. **Dark theme** — `createWidget`'s default is `lightTheme`; built `PANEL_DARK_THEME` off the
   engine's own `darkTheme` preset with a few fields overridden to match `CandleChart.tsx`'s exact
   palette. Verified rendering dark.
4. **Indicators + drawing tools wired in** — `createWidget` (not bare `createChart`) plus
   side-effect imports `'openalgo-charts/indicators'` and `'openalgo-charts/draw'` (both
   self-register their 105 indicators / drawing tools at module load — confirmed against the
   compiled `.mjs` source, not just the README). The widget's own topbar (which is where its
   Indicators button natively lives) is kept hidden — a symbol/interval picker wired to no `feed`
   would look functional and silently do nothing — so a custom "Indicators" button calls
   `widget.openIndicatorPicker()` instead. **No external feed is used anywhere** — `createWidget`
   is called with no `feed` option, and this component still drives `widget.series`/the volume
   series itself via `setData`/`update`, exactly as before. Verified: the real 105-indicator
   picker opens and is searchable.
5. **Rail-height/scroll bug** — `.oac-widget`/`.oac-stage` never inherited a real height from
   `.candle-chart`, so the whole widget shell (including the rail) could grow taller than the
   container and get clipped by `.candle-chart`'s own `overflow: hidden` before the rail's own
   `overflow-y: auto` ever got a reason to activate. Fixed by pinning `height: 100%` down that
   chain. Verified via `element.scrollHeight > element.clientHeight` + a real scroll on the
   Trade page's shorter chart.
6. **OHLCV + volume readout** — shows the latest candle by default, follows the cursor on hover
   (via `chart.subscribeCrosshairMove`'s own `e.bar`). Found and fixed a real bug in the same pass:
   volume read 0 always because `CrosshairMoveEvent.bar` is "the hovered bar of the PRIMARY series"
   (the candlestick series, which was never given a `.volume` field) — volume is now looked up
   from the original `candles` array by matching timestamp instead.
7. **A real, reproducible race condition found and fixed at its source** (`TokenChartModal.tsx`):
   switching between two open positions' tokens in the chart strip re-filters `positions`
   synchronously, but `candles` comes from a stale-while-revalidate cache (CLAUDE.md §50) and can
   briefly still hold the PREVIOUS token's rows while the new fetch is in flight. Both chart
   engines draw whatever `positions` + `candles` say in the same render, so for that one window a
   position's real entry/SL/TP was plotted against a different token's still-loaded price/time
   axis. Fixed by blanking `shown` (the filtered positions array) until
   `liveCandles[0].InstID === instId` — i.e. until the candle series genuinely belongs to the
   token being displayed.

## STILL BROKEN — the operator's own screenshot, not yet resolved

**The zone-highlight box still renders in the wrong horizontal position on the OpenAlgo engine, at
least sometimes.** This was reported TWICE this session — once before the race-condition fix
above (root-caused and believed fixed), and again AFTER that fix and after a defensive filter was
added on top of it. The operator's own screenshot (referenced but not saved to this repo — it's in
their message, not a file here) showed the box sitting mid-chart while the real entry marker and
current price were much further right, which is the same shape of bug as before.

**What this session tried, in order, and what each one did/didn't fix:**

1. Added `autoscaleInfo()` to `OpenAlgoZones.PositionZones` so the price axis expands to include a
   position's SL/TP even when they're outside the visible candles' own high/low range. This WAS a
   real, separate bug (confirmed: without it, a box's fill/edge lines beyond the autoscaled range
   were silently clipped, which looked like "box sits in the wrong place" from a different cause
   than the one below). Fixed and verified — kept.
2. Root-caused and fixed the `TokenChartModal.tsx` stale-candle-vs-fresh-position race described
   above. Verified via a captured `[zone-debug]` console log showing `x1`/`x2`/`entryY` computed
   correctly for a real position (LTC #2056, entry 61.51, opened 12:30) — the numbers were right
   and matched a clean screenshot at the time. **But the operator reported the bug again after
   this fix was deployed**, on a DIFFERENT position, so either this fix didn't fully close the
   race, or there is a second, different cause with the same visual symptom.
3. Added a defensive "plausibility" filter in `OpenAlgoCandleChart.tsx` (`plausiblePositions`) —
   drops any position from the zone/marker draw whose `EntryPx` sits more than
   `10 × visible-candle-range` (or 50% of the visible high, whichever is larger) outside the
   currently-loaded candles' own high/low. This is a blunt, last-resort guard: it can't explain OR
   fix the root cause, it can only stop a wildly-mismatched position from ever being drawn. **Not
   yet verified against a real repro** — this session ran out of tunnel time before confirming it
   actually helps, and it's entirely possible the real bug produces a box that's mismatched but
   still WITHIN a plausible price range (e.g. a stale position on a same-priced-order-of-magnitude
   token), which this filter would not catch at all.

**What this session tried to reproduce and could NOT reproduce cleanly:**
- A position with 32-36 SL/TP adjustments (the operator's own LTC example had 36) — checked HBAR
  #2132 (32 adjustments) and it rendered correctly.
- Rapid clicking through the chart strip's chips (BTC → CL → FIL → BTC in quick succession) — one
  of these runs showed a chart with candles but genuinely NO zone box at all for the active
  token (BTC), which is a THIRD distinct symptom (missing entirely, not misplaced) that was not
  investigated further before context ran out.
- A drift test (screenshot at t=0, t=15s, t=35s on the same chart, no interaction) — showed no
  change, so it is not a pure time-based drift on an idle chart.

**Hypotheses NOT yet tested, worth trying first in the next session:**
- The bug may correlate with the SL/TP-ratchet update path specifically (`ChartAdjustPanel`'s own
  drag/update flow, or the backend's periodic RL-driven SL/TP adjustment landing while the chart is
  open) rather than a token-switch race at all — the LTC and HBAR positions that were checked both
  have many adjustments, which was the original suspicion, but neither reproduced under a passive
  15-35s observation window. Try: open a chart on a position, then trigger a manual SL/TP edit via
  `ChartAdjustPanel` (drag a handle or type a value) WHILE watching if the box's horizontal
  position (not just its price levels) jumps — this session never tested a drag/edit while
  screenshotting, only passive viewing.
- Check whether `zonesRef.current?.setPositions(...)` can ever be called with a positions array
  whose `openTime`s reference bars that have since scrolled OUT of `dataLayer`'s own tracked range
  (e.g. after `series.setData()` replaces the whole series with a NEWER, shorter window — does
  `dataLayer.timeToIndexFloat` handle a time that predates the current series' own earliest bar
  correctly, or does it produce a nonsensical index that `indexToX` then maps to a garbage pixel?
  This session assumed `timeToIndexFloat`'s own doc ("extrapolating past either edge") covers this,
  but never constructed a test where `openTime` is BEFORE the earliest currently-loaded candle,
  which is exactly what happens if the candle window is ever shorter than a position's own age).
  This is the most promising untested lead — worth instrumenting `timeToX` again (see the removed
  debug logging pattern below) and specifically capturing a case where `openTime < candles[0]`'s
  own time, not just checking a case that happened to look fine.
- Re-add the temporary debug logging that was used and removed this session (see git history /
  this file's own record below) but keep it gated behind a `?debug=1`-style flag instead of
  removing it outright, so a repro caught live can be diagned without a redeploy cycle.

**Debug logging pattern used this session** (removed before final commit, re-add similarly if
picking this back up): in `OpenAlgoZones.ts`'s `draw()`, right after computing `x1`/`x2`/`entryY`:
```ts
console.log('[zone-debug]', JSON.stringify({
  id: p.id, entry: p.entry, sl: p.sl, tp: p.tp,
  openTime: p.openTime, closeTime: p.closeTime,
  entryY, x1, x2, plotWidth, plotHeight,
  idxOpen: rc.dataLayer.timeToIndexFloat(p.openTime),
  baseIndexLen: rc.dataLayer.length,
}))
```
This was genuinely useful for confirming the MATH was right in the one case it was checked against
— the gap is that it was only checked on a couple of positions that turned out not to reproduce
the bug, not on one caught in the act of showing it wrong.

## Layout changes made this session, NOT yet visually re-verified after the last deploy

The operator's second message (with a Binance screenshot as the target layout reference) asked for
several more layout changes, implemented in this session's LAST commit before running out of
context/tunnel budget:

1. **Drawing rail hidden by default**, revealed by a small arrow button (`.oac-rail-toggle`,
   `›`/`‹`) at the chart's left edge. The rail now floats as an absolute overlay
   (`top:0;left:0;bottom:0`) when open rather than reserving a permanent 42px layout column — so
   nothing else needs to shift when it opens/closes.
2. **Indicators button moved into the SAME ROW as the timeframe tabs**, positioned right after
   them (`left: 118px`, a fixed estimate for "tabs' own rendered width + gap" since the tab set —
   5m/15m/1H — is a fixed constant, not dynamic). OHLCV readout moved down to `top: 34px` to make
   room (was where Indicators used to sit).
3. **Rail-scroll fix generalized**: `.oac-rail`'s `overflow-y: auto` should now apply to its FULL
   height (the whole `top:0/bottom:0` overlay), not just a portion — the operator's complaint that
   "scroll only seems to work for the top tools" should be addressed by this, since the rail
   previously only got whatever height leaked through `.oac-widget`/`.oac-stage`'s own
   possibly-partial height inheritance, and now gets the full container height directly via its
   own `top`/`bottom` positioning. **NOT verified with a screenshot after this specific change** —
   the tunnel died before a fresh Playwright run could confirm it visually. Typecheck and build
   both passed clean, and it was deployed (image ID confirmed matching), but "compiles and
   deploys" is not the same as "looks right" — verify this first thing next session.

**NOT attempted at all** (explicitly asked for, out of time):
- **Candle-type dropdown** ("این ریپویی که استفاده میکنیم از مدل‌های مختلف نمایش کندل هم ساپورت
  میکنه" — the repo supports multiple candle display types, e.g. bars/hollow candles/Heikin
  Ashi/etc., per the Binance reference screenshot's own "Original / TradingView / Depth" and
  candle-style icon). `openalgo-charts`' `SeriesType` union already includes
  `'candlestick' | 'hollow-candle' | 'volume-candle' | 'bar' | 'high-low' | ...` (see this
  package's own `dist/index.d.ts`) — implementing this means adding a UI control that calls
  `widget.setChartType(id)` (a real, existing method on `Widget` — confirmed in the type dump this
  session captured) and does NOT require re-architecting anything already built. This is probably
  the single highest-value next task since the API already exists and is documented.
- Re-verifying the OpenAlgo layout against the exact Binance reference screenshot's specific
  arrangement (symbol/price header row, chart-type icons in the toolbar row, etc.) beyond what was
  already done — the operator's ask was "لطفا هرکدوم از آپشن‌هایی که میشه رو پیاده کن" (implement
  whichever of these options are feasible), which was only partially started.

## Files touched this session

- `panel/src/App.css` — volume CSS (TV engine unaffected, no new rules there), all `.oac-*` rules
  (theme-independent chrome: indicators trigger, OHLCV readout, rail toggle/overlay/scroll,
  chart-bar-tabs-overlay-openalgo positioning), chart-engine-toggle styles.
- `panel/src/components/CandleChart.tsx` — volume histogram added (TV engine).
- `panel/src/components/TokenChartModal.tsx` — the `candlesMatchInstId` race-condition guard;
  `useActiveChartEngine()` wired in to conditionally offset the timeframe overlay (this offset
  logic may now be partially redundant with the rail-is-now-an-overlay change above — worth
  checking whether `chart-bar-tabs-overlay-openalgo`'s `left: 8px` override is even still needed
  since the rail no longer reserves permanent space; it's harmless either way since both resolve
  to the same 8px today, but simplify if confirmed dead).
- `panel/src/pages/TradePage.tsx` — import path only (`CandleChart, ChartEngineToggle` from
  `../components/chart` instead of `../components/CandleChart` directly).
- `panel/src/components/chart/` (all new this session):
  - `types.ts` — the `CandleChartProps` contract both engines implement.
  - `index.tsx` — `CandleChart` (the engine switch), `ChartEngineToggle`, `useActiveChartEngine`.
  - `useChartEngine.ts` — the runtime engine-preference hook (localStorage + `?engine=` override).
  - `CandleChart.tsx` is NOT in this directory — the original TV implementation stays at
    `panel/src/components/CandleChart.tsx`, untouched in location, only edited in place for volume.
  - `OpenAlgoCandleChart.tsx` — the OpenAlgo engine implementation.
  - `OpenAlgoZones.ts` — the OpenAlgo-engine port of `../PositionZones.ts`.

## Verify-first checklist for the next session

1. SSH tunnel to `okx` (`ssh -f -N -L 18080:127.0.0.1:8080 okx`), confirm `curl
   127.0.0.1:18080` returns 200 IMMEDIATELY BEFORE each Playwright run, not just once at the start.
2. Screenshot the current deployed state (Positions page, click a real open position, switch the
   engine toggle to OpenAlgo, wait for `.oac-widget` with a 40s timeout) before changing anything
   — confirm what's actually live matches this document's "fixed and verified" list, since a
   tunnel-related deploy failure could have left the server on an older image.
3. Re-attempt the zone-highlight repro using the untested hypotheses above (an in-progress SL/TP
   edit, or a position whose `openTime` predates the loaded candle window) rather than re-checking
   cases already confirmed fine.
4. If the zone bug is found: get a `[zone-debug]` console capture of the ACTUAL broken case before
   changing code — every fix attempt this session that skipped that step ended up fixing a real
   but different bug instead of the one being chased.
