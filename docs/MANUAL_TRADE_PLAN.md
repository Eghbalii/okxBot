# Manual futures trading page ("Trade" tab) — implementation plan

Status: **in progress**, started 2026-09-19. All operator decisions in §8 are settled (Market+Limit
both in scope, USD notional sizing, manual bypasses `enabled_real`, a strategy-held position warns
but never blocks a manual order). Only §8.5 (orderbook depth) remains open, defaulted to `books5`
until told otherwise. §9 build-order steps 1-3 are DONE (schema, `GET /api/manual/instruments`,
`usecase.ManualTrader`'s core lifecycle) — see step 3's own note for one deliberate deviation from
the original reconcile-driver plan, and a real open gap it leaves (no drift/protection
re-verification for manual positions yet). Steps 4-6 (the rest of the `cmd/api` surface, the panel
page, the orderbook WebSocket) are not started. Requested
by the operator: a discretionary, exchange-style manual trading page — 3-column layout (chart |
orderbook | order ticket), token picker, leverage, price/SL/TP by price-or-percent, update/close,
own database table fully independent of strategy-driven trading, orderbook WebSocket scoped to only
the currently-viewed token, chart+position-switch caching reused from the existing pattern, and a
"Trade" button on the Home market table and on `TokenChartModal` that deep-links here with the
clicked token preselected. Spot is explicitly out of scope for this pass (futures only); the design
should not make spot harder to add later, but should not delay futures to accommodate it.

This plan was written after a full read-only audit of the codebase (not from memory of what CLAUDE.md
claims exists) — every "already have it" / "need to build it" call below is code-verified. The
operator's own framing ("we already have all the tools, tested") is **accurate for the exchange
adapter layer and the `real_orders` schema**, but **understates the gap at the `cmd/api` HTTP
surface and the orderbook feed**, both of which are genuinely new work. See §0.

## 0. What already exists vs. what is new (the audit's headline findings)

**Already built, reusable as-is:**
- `port.ExchangeClient` has full REST ↔ `gatewayclient` ↔ `cmd/okx-gateway` route parity for every
  order primitive needed: `PlaceOrder`, `CancelOrder`, `GetOrder`, `SetLeverage`, `GetInstrument`,
  and all four algo-order methods (`PlaceAlgoOrder`/`AmendAlgoOrder`/`CancelAlgoOrder`/
  `GetAlgoOrder`) — verified end to end, `internal/okx/rest` → `internal/gatewayclient` →
  `cmd/okx-gateway/handlers.go`.
- `real_orders` / `real_order_adjustments` tables (migrations `000019`, `000021`, `000027`,
  `000028`, `000029`) and their full `internal/postgres` implementation (`real_orders.go`,
  `real_order_adjustments.go`) are mature and already carry everything a manual order needs:
  entry/SL/TP prices, leverage, size, contracts, status lifecycle, exchange order/algo IDs, realized
  PnL, fee, adjustment history. `strategy_id` is nullable — a manual order can be written with it
  NULL.
- The panel's adjust/close UX is already built and sophisticated: `ChartAdjustPanel.tsx` (price ⇄
  percent toggle, live chart preview, decimals-from-entry-price), `AdjustPositionForm.tsx` (the
  table's version), `PositionZones.ts` (entry/SL/TP rendering), `useCachedResource` (stale-while-
  revalidate cache used for candles and position lists today).
- `GET /api/instruments?mode=real` already gives a real-mode token roster with `execInstId`.

**Does NOT exist yet — this is the actual new-build list:**
1. **Orderbook/depth WebSocket — zero code anywhere.** Confirmed by exhaustive grep: only `tickers`
   and `candle{bar}` are subscribed on the public OKX WS today. No `books` channel, no snapshot+delta
   merge, no checksum validation, no decoder, no Kafka topic or panel-facing push, no panel type.
   This is the single largest piece of new work in this plan.
2. **No HTTP endpoint anywhere can open a position.** Every existing `PlaceOrder` call for real
   money goes through `usecase.RealTrader.openReal`, which is hard-gated on a loaded RL model, a
   strategy signal, `RiskManager.Approve`, and a one-open-position-per-token check. `cmd/trader`'s
   entire HTTP surface is `POST /restart` + `GET /healthz`. A manual "place this order now" path is
   new code, not a wiring change.
3. **Order + SL/TP is a two-call sequence, not one.** OKX's `attachAlgoOrds` (place-with-attached-
   SL/TP in one call) is not modeled anywhere in this codebase. The safe sequence (leverage → read
   instrument → size→contracts → place entry → poll until filled → place protection → close-if-
   protection-fails) exists only as inline code inside `RealTrader.openReal` and is not extracted
   into anything callable from `cmd/api`.
4. **`RealTrader`'s reconcile loop halts real trading on an untracked exchange position.** This is
   the most dangerous gap. A manual order placed from a process other than the instrument's own
   `RealTrader` will, on the very next reconcile pass, look exactly like an unexplained position that
   appeared on the exchange — which is precisely the condition `RiskManager.Halt` exists to catch
   (CLAUDE.md §48). The existing in-flight-suppression mechanism is internal to `RealTrader`'s own
   open path and does not know a manual order is coming. **This must be solved with a DB-mediated
   handshake, not ignored** — see §4.
5. **No tick size / lot size / min size / contract value reaches the panel.** `GetInstrument` exists
   on the exchange client but is not exposed over HTTP, and the `instruments` roster table persists
   none of these fields. A manual order form cannot validate/round inputs today.
6. **`SetLeverage` and `PlaceAlgoOrder`/`CancelAlgoOrder` are not reachable from `cmd/api`.** They
   exist on the gateway and `cmd/api` already holds a `gatewayclient`, but it's type-narrowed to
   `protectionAmender` (`AmendAlgoOrder` + `GetInstrument` only). Widening this is small but real.
7. **The manual close path (`RequestRealManualClose`) is asynchronous intent, not action** — the
   actual flatten happens on `RealTrader`'s own next tick for that instrument. A manual position
   needs to be inside a process that is actually watching its price feed, or nothing ever closes it.
8. **`real_orders` has no explicit "this was opened manually" marker** and `close_reason`'s CHECK
   constraint has no manual-open-appropriate vocabulary beyond what already exists — needs a small
   migration.

## 1. Core architectural decision: a new `ManualTrader`, not a bolt-on to `RealTrader`

Given #2/#3/#4 above, the manual-trade lifecycle needs its own owner process/type, deliberately
**separate from `usecase.RealTrader`**, for reasons worth stating precisely (this is the decision a
future session most needs to not re-litigate from scratch):

- `RealTrader` is one-per-configured-instrument, built at `cmd/trader` startup from the strategy
  roster, and its entire safety machinery (reconcile, protection, halt) is scoped to the tokens it
  was told to watch via config/DB roster. A manual trade can be opened on **any** token the operator
  picks from a live search, not just the pre-configured roster — forcing every manual token through
  `RealTrader`'s existing per-instrument construction would mean dynamically spinning up/tearing down
  `RealTrader` instances from an HTTP request, which is a much bigger and riskier change than writing
  a second, narrower trader.
- **Decision: `ManualTrader` lives in `cmd/trader` (the same process as `RealTrader`, not
  `cmd/api`)**, because it needs the same things `RealTrader` needs to be safe: a live tick/price
  feed for its own SL/TP monitoring backup, the `okx-gateway` client, and — critically — it must
  share the **same reconcile pass** so a manual position and a strategy position on the same
  instrument are reconciled together, never as two competing views of one exchange account (this is
  exactly the account-wide-not-per-engine lesson CLAUDE.md §39 already learned once for
  `RealTrader`'s own reconcile).
- `cmd/api` gains new HTTP handlers (`POST /api/manual/orders`, etc.) that do **not** call the
  exchange directly. They write an **intent row** to a new table and `cmd/trader`'s `ManualTrader`
  picks it up — the same "DB-mediated, not direct" pattern `RequestRealManualClose` already
  established for close-intent, extended to open-intent. This keeps the "only one process holds
  credentials and talks to the exchange" property (CLAUDE.md §27.1) intact — `cmd/api` still never
  calls OKX directly, `cmd/okx-gateway` remains the sole credential holder, and `cmd/trader` remains
  the sole *decision* maker about what hits the exchange, whether that decision came from a strategy
  signal or an operator's click.

**Rejected alternative**: let `cmd/api` call `gatewayclient` directly to place manual orders. This
was seriously considered — the audit found `cmd/api` already holds a full `gatewayclient` instance
narrowed away to `protectionAmender`, so a manual place-order path from HTTP would need no cross-
process handshake at all. Rejected because it reintroduces exactly gap #4 in the most direct way
possible: `cmd/api` placing an order with no `RealTrader`/`ManualTrader` in the loop means the very
next reconcile pass sees an untracked position and halts real trading — the bug is not hypothetical,
it is the documented failure mode of the existing code (CLAUDE.md §48). The DB-intent handshake adds
latency (an order takes one poll cycle, not zero, to be picked up) but is the only design that keeps
the halt-on-untracked-position safety net meaningful rather than something to route around.

## 2. New database schema

### 2.1 `manual_orders` (fully independent of `real_orders`/`paper_orders`)

Per the operator's explicit requirement ("تاریخچه پوزیشن‌هاش کاملا مستقل از چیزی باشه که با
استراتژی‌ها ترید میشه... یعنی جدول دیتابیس جدا"), a **new, standalone table**, not a `strategy_id
IS NULL` row in `real_orders`. Reasons this is the right call, not just deference to the ask: a
manual order has no strategy signal, no conductor category, no observation vector, and should never
be picked up by anything that iterates `real_orders` expecting those things (the RL reward pipeline,
`StrategyStatsFor`, the SL/TP-adjustment A/B comparison, `conductor.SignalConductor`'s update
cadence). Reusing `real_orders` would mean auditing every one of those readers for "does a NULL
strategy_id row break this," which is more fragile than a clean second table with its own narrower
contract.

Columns (draft — finalize at implementation, mirroring `real_orders`' proven shape rather than
inventing a new one):

```
manual_orders
  id                     bigserial PK
  inst_id                text        -- short symbol, e.g. "BTC" (CLAUDE.md §33.4 convention)
  exec_inst_id            text        -- resolved OKX instId at open time, for audit/display
  side                    text CHECK ('buy','sell')  -- matches real_orders.side's own convention,
                                                      -- not PosSide's separate long/short vocabulary
  order_type              text CHECK ('market','limit') NOT NULL DEFAULT 'market'   -- §8.1
  limit_px                numeric NULL   -- the operator-given resting price, market orders leave NULL
  status                  text CHECK ('pending','opening','resting','partial','filled','closing','closed','canceled')
    -- 'resting' = a limit order accepted by the exchange but not yet filled (§8.1) — distinct from
    -- 'pending' (this process hasn't finished submitting it yet)
  entry_px                numeric NULL   -- NULL while resting/unfilled; set once the entry actually fills
  sl_px                   numeric NULL
  tp_px                   numeric NULL
  size                    numeric        -- USD notional requested (§8.2)
  leverage                numeric
  contracts               numeric NULL   -- actual filled contracts, set on fill confirmation
  protected_by_strategy   boolean NOT NULL DEFAULT false
    -- true when RealTrader already held a protective algo order on this token at open time, so
    -- ManualTrader deliberately did not place a second one (§8.4) — exchange_algo_order_id stays
    -- NULL in that case, and the panel must say so rather than imply an independent SL/TP exists
  opened_at               timestamptz NULL
  closed_at               timestamptz NULL
  close_reason            text CHECK ('sl','tp','manual','liquidation','canceled') NULL
  close_px                numeric NULL
  realized_pnl            numeric NULL
  exchange_order_id       text NULL
  exchange_algo_order_id  text NULL
  exchange_close_order_id text NULL
  exchange_fee            numeric NULL
  manual_close_requested  boolean NOT NULL DEFAULT false   -- same async-intent pattern as real_orders
  last_error              text NULL
  last_error_at           timestamptz NULL
  created_at              timestamptz NOT NULL DEFAULT now()
```

No `mode` column — this table only ever holds real-money manual orders (paper manual trading is not
requested; if wanted later it is a second, identically-shaped table, not a mode column here, per
`real_orders`' own established precedent of "the table is the discriminator").

### 2.2 `manual_order_adjustments`

Same shape as `real_order_adjustments` (`{id, order_id FK, field CHECK ('sl','tp'), old_value,
new_value, created_at}`) — no `source` column needed since every adjustment here is manual by
construction.

### 2.3 `manual_order_intents` (the open-order handshake table, §1/§4)

This is the mechanism that solves gap #4. `cmd/api` writes a row here on `POST
/api/manual/orders`; `cmd/trader`'s `ManualTrader` polls it (or is pushed to via the account-events
Kafka-adjacent mechanism `okx-gateway`'s private WS already uses, CLAUDE.md §35.4 — reuse that
transport rather than a new poll if the timing works out) and is the ONLY thing that ever calls
`PlaceOrder` for a manual trade.

```
manual_order_intents
  id            bigserial PK
  requested_at  timestamptz NOT NULL DEFAULT now()
  inst_id       text
  side          text CHECK ('buy','sell')
  order_type    text CHECK ('market','limit') NOT NULL DEFAULT 'market'  -- §8.1
  limit_px      numeric NULL   -- required when order_type='limit'
  size_usd      numeric
  leverage      numeric
  sl_px         numeric NULL
  tp_px         numeric NULL
  status        text CHECK ('pending','claimed','done','failed') NOT NULL DEFAULT 'pending'
  manual_order_id  bigint NULL REFERENCES manual_orders(id)  -- set once claimed and a row exists
  error         text NULL
  claimed_at    timestamptz NULL
```

`cmd/api`'s `POST /api/manual/orders` handler inserts a `pending` intent and returns its id
immediately (this is an async accept, matching `handleClosePosition`'s existing `202` pattern) —
the panel then polls `GET /api/manual/orders/intent/{id}` (or the manual_orders list) for the
resulting order the same way an in-flight real order already shows as `status=pending` today.

**Why a separate intent table rather than writing directly into `manual_orders` with
`status=pending` and having `ManualTrader` claim rows from that table directly:** it keeps the
"requested" and "actually happened" halves cleanly separable for audit — an intent that failed
validation (bad instrument, leverage too high) never needs a `manual_orders` row invented for it,
whereas conflating the two tables means either inventing rows for failed requests or making
`manual_orders`'s own status vocabulary carry "was never actually attempted," which pollutes the
history table `real_orders`' own design deliberately avoided.

## 3. `cmd/api` — new HTTP surface

All under a `manual-orders` handler file, mirroring `internal/api/positions.go`'s existing shape.

- `GET /api/manual/instruments?q=<search>` — token search/picker. Reads the existing
  `GET /api/instruments`-backed roster (real-mode) PLUS, if the operator wants to trade a token not
  yet in the roster, falls through to a live `GetInstrument` call (via the widened gateway client,
  see below) so the picker is not limited to the pre-scanned roster. Returns `{symbol, execInstId,
  tickSz, lotSz, minSz, ctVal}` — this is also what finally exposes instrument metadata to the panel
  (gap #5). Cache this response client-side (§6) since tick size doesn't change bar-to-bar.
- `POST /api/manual/leverage` — `{instId, leverage}` → calls `SetLeverage` through the gateway
  client (widen `protectionAmender` or add a sibling interface with `SetLeverage`/`PlaceOrder`/
  `PlaceAlgoOrder`/`CancelAlgoOrder`, gap #6/#7). Real-money leverage changes are visible/logged like
  every other real-mode mutation.
- `POST /api/manual/orders` — the open-order intent write described in §2.3. Body: `{instId, side,
  sizeUsd, leverage, slPx?, slPct?, tpPx?, tpPct?}` — accept both price and percent per the
  operator's requirement, converting percent→price server-side with the exact same
  `priceFromMarginPct` helper `handleAdjustPosition` already uses (`server.go:913-929`), so the two
  entry points can never disagree about what a given percent means.
- `GET /api/manual/orders?status=open|closed` — list, mirroring `handleListPositions`'s shape
  closely enough that the panel can reuse the same table-rendering component where practical.
- `POST /api/manual/orders/{id}/close` — sets `manual_close_requested`, same async pattern as real.
- `POST /api/manual/orders/{id}/adjust` — same request/response shape as `handleAdjustPosition`
  (`{slPct?, tpPct?}` or price-based, see the price-vs-percent note above), writing to
  `manual_order_adjustments` and amending the exchange-side algo order via the now-widened gateway
  client.
- `GET /api/manual/orders/{id}/adjustments` — history, mirroring the existing pattern exactly.

## 4. `cmd/trader` — the `ManualTrader` type

New `usecase.ManualTrader`, deliberately **not** embedded in or sharing state with
`usecase.RealTrader` beyond the pure helper functions both already reuse from CLAUDE.md §27.7's
commit-2 extraction (`sizeFromModelAction`'s sibling for a fixed, operator-given size rather than a
model action; `computeAdjustedLevels`). Responsibilities:

1. **Poll `manual_order_intents`** for `pending` rows (interval: fast, e.g. 1-2s — an operator
   clicking "Buy" expects near-immediate feedback, unlike the model's own decision cadence).
2. **Claim** a row (`UPDATE ... SET status='claimed' WHERE id=$1 AND status='pending'`, the same
   conditional-UPDATE-not-mutex pattern CLAUDE.md §37.1 already established for exactly this kind of
   race) then run the full open sequence: resolve `execInstId`, `SetLeverage`, `GetInstrument` for
   sizing/rounding, `PlaceOrder`, poll `GetOrder` until filled/timeout (reuse `RealTrader`'s own
   `waitForFill` logic — this is a good candidate to actually extract into a shared free function
   given it's now needed by two callers, unlike most of `openReal` which only ManualTrader needs a
   variant of).
3. **Check for an existing `RealTrader`-owned protective algo order on this token first** (§8.4's
   settled behavior: no PosMode change, a manual order can share a net exchange position with a
   strategy-held one). If `RealTrader` already has a live algo order on this `execInstId`,
   `ManualTrader` does NOT place a second one — it records the manual order as
   protected-by-the-strategy's-order (a boolean/reference on the `manual_orders` row) and the panel
   surfaces this explicitly rather than implying the manual slice has its own independent SL/TP.
   Otherwise, **place exchange-side protection** (`PlaceAlgoOrder`) immediately after fill, exactly
   mirroring `RealTrader.openReal`'s own close-the-position-if-protection-fails safety net (gap #4's
   other half) — a manual position must never end up live on the exchange with no stop, any more
   than a strategy-opened one may, UNLESS it is deliberately relying on the strategy's own stop per
   the case above.
4. **Mark the intent `done`** (or `failed` with an error message the panel surfaces) and write the
   resulting `manual_orders` row.
5. **Own its own tick-based SL/TP backup monitor** for every open manual order, the same in-process
   watch `RealTrader.monitorOpenPositions` already does — exchange-side protection is primary, this
   is the documented defence-in-depth backup (CLAUDE.md §35.3).
6. **Participate in the shared reconcile pass** (CLAUDE.md §39's `ReconcileDriver`/
   `AccountSnapshot`): `ManualTrader` registers itself with the same driver `RealTrader` instances
   use, so one account-wide `GetPositions`/`GetBalance` poll reconciles BOTH strategy and manual
   positions together — this is what actually closes gap #4: an untracked-position halt now only
   fires for a position that is untracked by *either* trader, not falsely triggered by a manual
   position `RealTrader` doesn't know about.
7. **Manual close**: watches `manual_orders.manual_close_requested`, flattens on the next tick,
   cancels the resting algo order — identical shape to `RealTrader`'s own manual-close handling.

## 5. Panel — new `/trade` route

### 5.1 Layout: 3 columns, per the operator's explicit spec

```
+------------------+------------------+------------------+
|                  |                  |                  |
|   Chart           |   Orderbook       |   Order ticket    |
|   (left, largest)|   (middle)       |   (right)        |
|                  |                  |                  |
+------------------+------------------+------------------+
```

- **Left — chart**: reuse `TokenChartModal`'s chart internals (candles + `useLiveCandles` +
  `PositionZones` for entry/SL/TP lines) but as a full page panel rather than a modal, with the
  token switcher visible above it (search/select, not just the fixed roster strip the modal uses
  today, since Trade should support any tradeable token per §3's instrument search).
- **Middle — orderbook**: new component, subscribes to the new orderbook WS (§7) for **only the
  currently selected `execInstId`** (the operator's explicit requirement — no extra subscriptions).
  Standard bid/ask ladder with cumulative-size bars, matching "اکثر صرافی‌ها" (most exchanges').
- **Right — order ticket**: side toggle (long/short), leverage slider/input, order type toggle
  (Market / Limit — both in scope from day one, see §8.1), a price field (disabled/hidden for
  Market, required for Limit), size in **USD notional** (§8.2), SL/TP each with a price/percent
  toggle reusing `ChartAdjustPanel`'s existing `LevelMode` toggle component directly rather than
  reimplementing it, submit button, and — once a position is open on the selected token — the
  existing Update/Close controls (`ChartAdjustPanel`/close button) render here instead of a fresh
  order form. If a STRATEGY already holds an open position on the selected token, the ticket shows
  an inline notice ("این توکن یک پوزیشن باز از استراتژی دارد") rather than blocking the page —
  the operator can still choose to place a manual order, informed rather than surprised (§8.4).

### 5.2 Caching (operator's explicit requirement: "از کش برای چارت و سوییچ کردن بین پوزیشن‌ها استفاده کن")

Reuse `useCachedResource` exactly as `TokenChartModal` does today:
- Candle cache key `` `candles:${instId}:${bar}` `` — identical to the existing modal, so switching
  between Trade and a Home-page chart modal for the same token shares one cache entry rather than
  refetching.
- A new cache entry for the manual-orders list, keyed `` `manual-orders:open` ``, so switching which
  open manual position is displayed in the order ticket (if the operator holds several at once) reads
  instantly from cache the same way `listPositions` already does for the Positions page (CLAUDE.md
  §50.2b's fix for exactly this pattern).
- Instrument metadata (§3's tick size etc.) cached per `execInstId` with a long `maxAgeMs` (it barely
  changes) — a new cache key, `` `instrument-meta:${execInstId}` ``.

### 5.3 Deep-linking from Home ("Trade" button)

Per the operator's explicit requirement: a "Trade" button on both the Home page's market table rows
and inside `TokenChartModal`, navigating to `/trade?symbol=<SYMBOL>` (or `/trade/:symbol`, decide at
implementation — `:symbol` is more consistent with this app's existing `/positions/:mode` pattern)
with that token preselected. The Trade page reads the route param the same way `PositionsPage` reads
`:mode` today (CLAUDE.md's own note: "Mode is derived from the URL, never held in state, so header
and page can't disagree") — apply the identical principle here so the URL is always the source of
truth for which token Trade is showing, not React state that could drift from it.

### 5.4 Route registration

Add `/trade/:symbol?` to `App.tsx`'s route table alongside the existing five, with `:symbol`
optional and defaulting to `BTC` per the operator's explicit "پیش‌فرض... مثل btc انتخاب شده" — this
default must live in the route/page, not be silently assumed by a component that receives no symbol.

## 6. Instrument metadata exposure (closing gap #5)

`GET /api/manual/instruments` (§3) is the vehicle, but the underlying capability — `GetInstrument`
reachable from `cmd/api` — is reusable well beyond the manual-trade page (the existing
`ChartAdjustPanel`'s `decimalsFor(entry)` heuristic could eventually be replaced with the real tick
size once this exists, though that is out of scope for this task and left as a follow-up, not
bundled in here). Implementation: widen `cmd/api`'s gateway-client interface (currently
`protectionAmender`, `internal/api/protection.go:13-18`) to also expose `GetInstrument`,
`SetLeverage`, `PlaceOrder`, `PlaceAlgoOrder`, `CancelOrder`, `CancelAlgoOrder` — either by growing
that interface or adding a sibling `manualTradeClient` interface, decide at implementation based on
which reads cleaner against the existing `protection.go` file's own narrow-interface convention.

## 7. Orderbook WebSocket — the largest new piece (closing gap #1)

This needs its own design pass at implementation time; sketched here so the shape is not invented
from nothing in a future session.

- **OKX side**: subscribe to the `books` (or `books5`, decide based on required depth — `books5` is
  lighter and sufficient for a manual order ticket's visual ladder, full `books` needed only if deeper
  book analysis is ever wanted) channel on the existing public WS transport (`internal/okx/ws/
  public.go` is already generic over channel + instIDs, per the audit — this part is a subscription
  addition, not new transport code).
- **Snapshot + delta merge**: OKX's `books` channel pushes an initial snapshot then incremental
  updates; the decoder must merge these into a live in-memory book, including OKX's own checksum
  field for validating the merge stayed correct (drop and resubscribe on checksum mismatch — do not
  silently serve a possibly-corrupt book to the panel).
- **Scoping to one instrument at a time (operator's explicit requirement)**: unlike `tickers`/
  `candle{bar}` which the ingestor subscribes to for the whole roster permanently, the orderbook
  subscription must be **dynamic** — subscribe when a client opens `/trade` for instrument X,
  unsubscribe when they navigate away or switch to instrument Y. This is a genuinely different
  lifecycle from every existing WS consumer in this codebase (all of which are "subscribe once at
  process startup for a fixed roster") and needs its own subscribe/unsubscribe management, likely in
  a dedicated small service or a new responsibility on an existing one — decide at implementation
  whether this lives in `cmd/ingestor` (extending its existing OKX WS ownership) or a new minimal
  service; `cmd/ingestor` is the more consistent choice since it already owns every other OKX public
  WS subscription, but its current design assumes a static roster and will need a change to accept
  dynamic add/remove requests (likely via a small HTTP control endpoint or a Kafka control topic,
  itself needing a delivery decision).
- **Delivery to the panel**: does NOT need Kafka/Postgres in the loop the way candles do (this is
  ephemeral, sub-second display data, never persisted, never trained on) — a direct WebSocket from
  `cmd/api` (or a dedicated small gateway) to the panel is the right shape, matching
  `internal/api/ws.go`'s existing hand-rolled hub pattern (CLAUDE.md §11.4) rather than round-
  tripping through Kafka + Postgres for data that's stale the instant it's written.
- **Panel side**: new hook (`useOrderbook(execInstId)`) that opens/closes the WS subscription as the
  selected token changes — mirroring `useLiveCandles`'s existing shape where reasonable, but with its
  own connect/reconnect lifecycle since it's inherently per-token rather than per-page.

## 8. Decisions from the operator (settled 2026-09-19)

### 8.1 Market AND Limit orders, both from day one

Changes `ManualTrader`'s open sequence from a single shape into two:
- **Market**: identical to `RealTrader.openReal`'s existing sequence — place, poll `GetOrder` until
  filled/timeout, then protect. `manual_orders.status` moves `pending → opening → filled`.
- **Limit**: `PlaceOrder` with `OrdType: "limit"` and the operator's given price returns immediately
  with the order resting unfilled on the exchange — there is no fill to wait for yet. This is a
  genuinely new state this codebase's fill-confirmation logic has never had to model (every existing
  automated order in this codebase is market-only, confirmed by the audit). Consequences to design
  for explicitly:
  - `manual_orders.status` needs a `'resting'` state (between `opening` and `filled`) distinct from
    the market path's `pending`, so the panel can show "order placed, waiting to fill" rather than
    implying something went wrong.
  - `ManualTrader` must poll `GetOrder` for resting limit orders **indefinitely** (or until a
    cancel/timeout the operator controls), not the short fixed timeout `waitForFill` uses for market
    orders — a limit order sitting unfilled for minutes/hours is normal, not a failure.
  - **Cancel-resting-order** becomes a real, separate operator action (distinct from closing a FILLED
    position) — `POST /api/manual/orders/{id}/cancel` for a still-resting limit order, versus
    `POST /api/manual/orders/{id}/close` for an already-filled one. The panel must show different
    controls depending on `status`.
  - Protection (`PlaceAlgoOrder`) can only be placed once the entry actually fills — for a limit
    order this means `ManualTrader`'s poll loop transitions `resting → filled → place protection`,
    the same transition the market path does immediately, just on its own timeline.
  - A limit order can be **partially filled** while resting (OKX supports this) — reuse
    `RealTrader.openReal`'s existing partial-fill handling (scale `size`/`entryPx` to the confirmed
    `AccFillSz`/`AvgPx`, CLAUDE.md §27.7's commit on this) rather than inventing new logic.

### 8.2 Size in USD notional

Matches every other sizing decision in this codebase (CLAUDE.md §26/§32.4) — no special case needed.
The order ticket UI should show the equivalent contract count live (using the now-exposed
tick/lot/ctVal metadata from §6) so the operator can sanity-check against the exchange's own display
convention, but the wire format and `manual_orders.size` column stay USD.

### 8.3 Manual trading bypasses `enabled_real`

A human clicking "buy" on a specific token is trusted intent, not something the automated-assignment
gate exists to police — matches the existing `handleCreateInstrument` precedent (CLAUDE.md §53.1:
"a person acting directly is trusted, a scan is not"). `GET /api/manual/instruments` (§3) does not
filter by `enabled_real`; it returns every instrument the roster/live search can resolve, regardless
of that flag. This also means the manual-trade token search (§3) needs its live-`GetInstrument`
fallback to genuinely work for a token that has never been in the roster at all, not just for
roster tokens with the flag off.

### 8.4 A strategy-held position on the same token does NOT block a manual order — it warns

Simpler than the hedge-mode alternative considered in the original draft, and the operator's explicit
choice: no `PosMode` change, no netting logic to build. Concretely:
- Before submit, the order ticket checks whether `RealTrader`'s roster currently shows an open
  position on the selected token (a simple read against `real_orders`/`ListRealPositions`, already
  exposed via the existing positions endpoint — no new backend logic needed for the check itself)
  and shows an inline warning ("این توکن یک پوزیشن باز از استراتژی دارد") if so.
- The operator can still submit. **What actually happens on OKX when both a strategy position and a
  manual position exist on the same token in NET mode needs to be stated plainly, not glossed over**:
  in net mode there is only ever ONE net position per instrument on the exchange — a manual buy on a
  token where the strategy already holds a long simply **adds to that same net position** (increasing
  its size), and a manual sell against an existing strategy long **reduces/flips** it. This is true
  regardless of which local table (`real_orders` vs `manual_orders`) "thinks" it owns which slice —
  the exchange does not know about the distinction, only this codebase's bookkeeping does.
  - **This has a real consequence for the reconcile/protection design (§4)**: if `RealTrader` holds
    a protective algo order for its own position and `ManualTrader` then places a SECOND algo order
    for "its" slice of what is actually the same net exchange position, the two algo orders don't
    stack cleanly — OKX's conditional/algo orders for a position, closing one leg does not partially
    close the other's intent. This needs a concrete resolution, not left implicit:
    **decision for implementation**: `ManualTrader` checks for an existing `RealTrader`-owned
    protective algo order on the token before placing its own. If one exists, the manual order does
    NOT place a second algo order — it relies on the strategy's existing protection and surfaces this
    clearly in the panel ("SL/TP از پوزیشن استراتژی محافظت می‌شود؛ برای این توکن SL/TP جدا نمی‌سازیم")
    rather than silently creating a conflicting second one. If the operator wants independent SL/TP
    for their manual slice, they should close or take over the strategy's position first — this is a
    real limitation of net mode, not a bug, and the panel should say so rather than let the operator
    discover it by having one of two SL/TPs silently not do what they expected.
  - This also means `close`/`adjust` on a `manual_orders` row for a token that ALSO has a
    `real_orders` position must be careful: closing "the manual order" can only mean placing a REAL
    flatten/reduce order against the shared net position, sized to the manual slice's own notional —
    it does not and cannot close only "the strategy's part" or only "the manual part" at the exchange
    level. Document this exact behavior in the panel's copy near the close button when both exist,
    not just in this plan.

### 8.5 Still open: orderbook depth

`books5` (5 levels, lightweight) vs. full `books` (400 levels, checksummed, heavier) — not yet
decided. Default to `books5` at implementation start (sufficient for a manual order ticket's visual
ladder, matches this plan's "ship the riskiest/heaviest piece last and start simple" posture in §9)
and revisit only if the operator asks for more depth after using it.

## 9. Suggested build order

Roughly in dependency order, each independently shippable/testable:

1. **DONE (2026-09-19).** Migrations: `manual_orders`, `manual_order_adjustments`,
   `manual_order_intents`.
2. **DONE (2026-09-19).** Widened `cmd/api`'s gateway-client interface (`manualTradeClient`,
   `internal/api/manual_orders.go`) + `GET /api/manual/instruments` (tick/lot/min size, contract
   value — closes gap #5, deliberately bypasses `enabled_real` per §8.3, not limited to the
   pre-scanned roster).
3. **DONE (2026-09-19), with one deliberate deviation from the original plan below.**
   `usecase.ManualTrader`'s core open/protect/close lifecycle now lives in `cmd/trader`
   (`internal/usecase/manualtrader.go`), reusing `RealTrader`'s proven mechanics
   (`waitForFill`/`sizeToContracts`/protect-or-close-immediately) rather than reinventing them.
   Market and limit orders both open correctly; §8.4's protection-sharing check works; the
   close-request sweep flattens filled positions and cancels resting ones from one shared flag.
   8 tests, all passing.

   **Deviation**: this does NOT register `ManualTrader` with the shared `ReconcileDriver`
   (§4/§4.6's original plan) — `ReconcileDriver.Engines` is a concrete `map[string]*RealTrader`
   with no interface, and widening it was judged too risky to do blind, mid-feature, against
   production-critical reconciliation code with no chance yet to verify the change against a real
   account. Instead, §8.4's "does RealTrader already protect this token" check reads
   `Repo.ListRealPositions` directly (a DB read, no live exchange call) from a closure wired in
   `cmd/trader/main.go`. **Concrete consequence, not yet closed**: a manually-opened position is
   NOT currently re-verified against exchange drift the way every `RealTrader` position is (no
   equivalent of `ensureProtection`/the untracked-position halt runs for it). This is an open gap,
   not a resolved design choice — before manual trading is used with meaningful size, either (a)
   extend `ReconcileDriver` with a small interface both `*RealTrader` and `*ManualTrader` can
   satisfy, or (b) give `ManualTrader` its own periodic protection-verification pass calling
   `GetAlgoOrder` per open manual order, mirroring `ensureProtection`'s own logic. Flagging this
   explicitly rather than letting "it's in cmd/trader now" read as "it has full parity with
   RealTrader's safety net" — it does not, yet.
4. **DONE (2026-09-19).** `cmd/api` manual-order HTTP surface: `POST /api/manual/leverage`,
   `POST /api/manual/orders` (writes the intent row per §2.3/§4, converting slPct/tpPct to a price
   via the existing `priceFromMarginPct` helper before the intent is written so ManualTrader's own
   open sequence never has to re-derive a percent with a possibly-different reference price),
   `GET /api/manual/orders` (list), `GET /api/manual/order-intents/{id}` (poll while an order is
   still being placed — named `order-intents` rather than `orders/intent/{id}` to avoid an
   ambiguous route registration against `GET /api/manual/orders/{id}/adjustments`: net/http's
   `ServeMux` rejects two patterns of the same segment depth where one has a literal and the other
   a wildcard at the same position with more segments following), `GET /api/manual/orders/{id}`,
   `POST /api/manual/orders/{id}/close`, `POST /api/manual/orders/{id}/cancel` (resting-limit-only,
   §8.1), `POST /api/manual/orders/{id}/adjust` (same unclamped-manual-edit posture as
   `handleAdjustPosition`, refuses when `ProtectedByStrategy` since there is no algo order this
   order itself owns to amend), `GET /api/manual/orders/{id}/adjustments`. 11 new tests, all
   passing; 984 Go tests total.

## 9a. Rename: "Real Trader" -> "Bot Trader" (2026-09-19)

Done alongside starting steps 4-6, per explicit operator request to make the three trading
services' names read clearly: **paper trader**, **bot trader** (was "real trader" —
`usecase.RealTrader` -> `usecase.BotTrader`, `real_orders`/`real_order_adjustments` tables ->
`bot_orders`/`bot_order_adjustments`, mode value `"real"` -> `"bot"` everywhere in the DB/API/panel),
and **Trade** (this manual/discretionary trader, `usecase.ManualTrader` — already correctly named).
Full rename across the Go backend, database schema/data (applied directly on the server rather than
via a versioned migration, per explicit operator decision since the project is still in active
development — migration `000033` mirrors the same change for a fresh database), and the panel.
`trading.allow_real_money`/`UseConductorLifecycle` were deliberately left untouched (they describe
real capital and a feature flag, not the trader's name), and `instruments.enabled_real` was also
left untouched (a distinct column for per-token real-trading enablement, out of the rename's stated
scope). Verified end to end against the live server: DB rename applied cleanly (zero open bot
positions at the time, bot trading state was `stopped`, `cmd/trader` container was not even
running — the safest possible window), `go build`/`vet`/`test` all pass (973 tests before step 4's
11 additions), `tsc -b`/`vite build` both pass, `paper-trader`/`api`/`panel` rebuilt and redeployed
one at a time per the established procedure, `GET /api/health` now reports "Bot Trader" instead of
"Real trader", `GET /api/positions?mode=bot` returns `bot_orders` data correctly, `?mode=real` now
correctly 400s. `cmd/trader` itself was rebuilt (confirmed to compile and contain the renamed code)
but deliberately NOT started — `okx-gateway` currently holds real, non-simulated credentials
(`GET /health` -> `{"simulated":false}`), and starting the live trader is a separate, explicit
go/no-go decision, never a side effect of a rename deploy.
5. Panel `/trade` page skeleton: 3-column layout, chart (reusing existing internals), order ticket
   wired to the new endpoints, Home-page/`TokenChartModal` "Trade" button deep link. Ship this
   BEFORE the orderbook WS if useful — a manual trade page with a chart and no live orderbook ladder
   is still a functioning (if incomplete) manual trading page, whereas the orderbook is the single
   largest and riskiest new piece.
6. Orderbook WebSocket (§7) — the dynamic-subscription design needs its own focused session given
   its lifecycle differs from every existing WS consumer in this codebase.

## 10. What this plan deliberately does not cover

- Spot trading — explicitly deferred by the operator. Nothing above should make spot materially
  harder later (the schema's `inst_id`/`exec_inst_id` shape is instrument-type-agnostic), but no
  spot-specific work is included here.
- Any change to strategy-driven trading's own behavior — `RealTrader` is touched only to add shared
  reconcile-driver participation for `ManualTrader`; its own open/close/adjust logic is untouched.
- Paper-mode manual trading — not requested; would be a second, identically-shaped table per §2.1's
  own reasoning if ever wanted.
- Authentication — `cmd/api` remains unauthenticated/VPN-only per the existing posture. Adding a
  "place a real order for arbitrary size/leverage" endpoint to an unauthenticated surface is a real
  increase in blast radius versus the existing read-mostly + adjust-only API, worth the operator's
  explicit awareness even though this plan does not propose adding auth as part of this work.
