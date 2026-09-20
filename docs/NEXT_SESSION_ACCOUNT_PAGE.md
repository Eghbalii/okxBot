# Account page / shared trading-cap feature — status for next session

Committed as `0358f11`. This document is what's left to verify/finish.

## What's done and verified

- **Backend, all tests pass** (`go test ./...` clean, `go build ./...` clean):
  - `account_equity` supports `mode='manual'` (migration `000035`).
  - `SetTradingCap` bounds one mode's cap against the sibling real-money mode's current claim
    (bot/manual share one real balance) — tested against a real Postgres
    (`internal/postgres/account_equity_test.go`, skips cleanly with no local DB).
  - `ManualTrader.RecordEquity` records manual's own balance row, wired into `ReconcileDriver`.
  - `conductor.ClampSLToCapUSD` (new): tightens a stop under cross margin so a touch can never
    exceed the mode's trading cap. Wired into `BotTrader.openBot` and
    `ManualTrader.finishOpen`, both gated on `TdMode == "cross"`. Fully unit-tested +
    mutation-checked in both call sites.
  - `ManualTrader.openFromIntent` declines an order that would exceed available manual margin,
    checked before any exchange call. Unit-tested + mutation-checked.
  - `handlePaperTradingStats` / `statsMode` now serve `mode=manual` (counts from `manual_orders`,
    not `paper_orders`/`bot_orders`), with new `usedMarginUsd`/`availableMarginUsd` fields.
  - Disk cleanup redesign (`internal/api/diskcleanup.go` + new `diskfiles.go`): automatic Docker
    prune (build cache + stopped containers + unused images) and a confirm-per-file candidate
    list (training dumps, model backups, config backups) — **deployed and verified live**.
  - Kafka per-topic retention fix for `okx.orderbook` (`internal/kafkastream/topicconfig.go`) —
    **deployed and verified live**, self-healing on ingestor restart.
  - `zigzag_pa` staleness fix (order #1127's null TP) — **deployed and verified live**, with a
    real-data regression test.

- **Frontend, typechecks + builds clean**:
  - New `/account` page (`panel/src/pages/AccountPage.tsx`): total assets across exchanges, both
    trading-cap controls (bot + manual) via extracted `TradingCapControl.tsx`.
  - `PaperTradingStatsBox` no longer embeds the cap control — links to `/account` instead.
  - Trade page's order ticket shows available manual margin and OKX min-size/lot-size hints,
    with a pre-submit block on an order below the exchange minimum and a warning (non-blocking)
    when a request exceeds available margin.
  - `PositionsTable` extraction (from an earlier session) — unrelated to this feature, already
    deployed.

## Deployed to the server, verified

- `cmd/api` — rebuilt, migration `000035` applied (confirmed via `\d account_equity`), manual
  mode endpoints tested directly (`/api/account?mode=manual`, `/api/paper-trading/stats?mode=manual`
  both return correct data). `panel` restarted after (nginx IP-cache rule, §18.2). Grafana/
  Prometheus stopped during build, resumed after. All 13 services confirmed up and healthy.

## NOT yet done — start here next session

1. **`cmd/trader` has NOT been rebuilt with this session's changes.** It contains the actual
   `BotTrader`/`ManualTrader` cross-margin guard and cap-check logic — until it's rebuilt and
   redeployed, **none of the new safety guards are active in production**, and the currently
   running `trader` binary is the pre-this-session version. This is the most important remaining
   step. Rebuild procedure (same as `api` above): stop grafana/prometheus if free memory is low,
   `docker builder prune -f`, `docker compose build trader`, `docker compose up -d trader`, check
   logs for a clean start, resume monitoring.

2. **Verify the file-transfer was fully clean before rebuilding `trader`.** During this session's
   deploy, the bulk per-file `scp` loop (70 files) had **silent failures on at least 3 files**
   (two new migration files, and later `account.go` and `paper_trading.go` were found stale on
   the server despite an earlier loop reporting them as matching). All three were manually
   re-transferred and confirmed byte-identical via individual `md5sum` calls, and `cmd/api` was
   rebuilt twice more afterward to pick up the fixes — the currently-built `api` image is
   confirmed correct. **Before rebuilding `trader`, redo a full, careful one-file-at-a-time
   `md5sum` comparison** for every file listed in this session's diff (`git show --stat 0358f11`)
   rather than trusting any single past "all matched" result — this session's own experience is
   that the loop can silently lie. Do not use the same 70-files-in-one-SSH-loop approach that
   failed; either verify one file at a time with real per-call output inspection, or use `rsync
   --checksum` and actually read its per-file output (not a summarized "ok (synced)" line).

3. **No live end-to-end test of the cross-margin guard or the cap-sum bound against the real
   server database has been done.** Unit tests pass against fakes and a real local-Postgres
   integration test exists for the cap-sum bound, but nothing has exercised
   `POST /api/account/cap?mode=manual` against the real production `bot`/`manual` rows, or placed
   a real (or even paper-adjacent) order through `ManualTrader` with `TdMode=cross` and a small
   cap to see the guard actually tighten a real order's stop. Recommend testing this via the
   panel's Account page (set a small manual cap, e.g. $5) before ever placing a real order against
   it, and checking `docker logs okxbot-trader-1` for the `"cross-margin guard: tightened..."` log
   line if/when a real cross-margin manual order is placed.

4. **The "spot vs futures" distinction was deliberately NOT built** — confirmed with the
   operator that OKX has one unified trading account with no spot/futures split, so the Account
   page shows one balance per exchange rather than inventing a split OKX doesn't have. If a future
   exchange (or OKX itself) genuinely separates them, `domain.Balance`/`ExchangeBalanceSource`
   would need a new field — nothing in this design blocks that, but nothing built it either.

5. **Real trading (`use_conductor_lifecycle`) is still off on the server** — per CLAUDE.md's own
   long-standing status, real trading has never been turned on. Everything in this feature is
   built and ready for when it is, but none of it has been exercised against real order flow.
