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

## Update 2026-09-20 (later session): items 1-3 resolved

Before touching anything, a full one-file-at-a-time `md5` comparison (local `md5 -q` vs. remote
`md5sum`, 71 files from `git show --name-only` on both `0358f11` and `f12cdf9`, no bulk-loop
shortcuts) found **exactly one mismatch**: `docs/NEXT_SESSION_ACCOUNT_PAGE.md` itself (a docs file,
never `scp`'d in the original deploy — the server didn't even have a `docs/` directory). All 70
code/config/migration files were already byte-identical on the server, so items #1/#2's transfer
concern turned out to be moot for everything except this one doc file, which is now synced
(`mkdir -p docs` + `scp`, md5-verified identical).

1. **DONE — `cmd/trader` rebuilt and redeployed.** Followed the one-service-at-a-time procedure:
   checked free memory (209Mi free, 1.7Gi available with buff/cache — stopped `grafana`/
   `prometheus` first per the standing rule), `docker builder prune -f` (reclaimed 2.3GB), built
   `trader` alone (`docker compose build trader`, ~2 minutes), started it
   (`docker compose up -d trader`), watched logs for a clean start, then resumed `grafana`/
   `prometheus`. Verified the NEW binary is actually running (not just that the build exited 0) by
   `strings`-grepping the container for `ClampSLToCapUSD` and the cross-margin guard log lines —
   both present. Zero errors/panics in `trader`'s logs since startup. All 13 services confirmed up
   afterward via `GET /api/health`.
   - Before starting it: confirmed zero open real positions (`bot_orders`: 150 rows, all closed;
     `manual_orders`: 0 rows) and `paper_trading_config` for `mode='bot'` reads
     `trading_state='stopped'` — so even though `config.yaml` has `use_conductor_lifecycle: true`
     / `allow_real_money: true` (a later session than this doc enabled them, see item #5 below),
     the application-level gate kept `BotTrader` from opening anything. Trader's own startup log
     confirms this: `"real trading is not in the running state" tradingState=stopped`.

2. **DONE — file-transfer re-verified clean**, see the note above this list. Nothing needed
   re-transferring except the one docs file.

3. **PARTIALLY DONE — the cap-sum bound was exercised live against real production data; the
   cross-margin SL guard itself was NOT, since that requires placing a real order and doing so
   without explicit operator authorization in this session was judged out of scope.**
   - `POST /api/account/cap` with `{"mode":"manual","newCapUsd":30}` against real production rows
     (`bot` mode already held `TradingCapUSD=39.7` against a real balance of ~$40.12, leaving only
     ~$0.42 of headroom): the stored cap was written as the requested `30`, but `EquityUSD` (the
     enforced/effective slice) came back correctly bounded to `0.0718...` —
     `GREATEST(balance - siblingClaimed, 0)` computed and applied exactly as designed, verified
     against the real numbers rather than a fake. Confirmed via `account_equity_history`'s new
     `reason='cap'` row, then **reverted** (`trading_cap_usd` back to `NULL`, `equity_usd` back to
     `40`, the stray history row deleted) since manual mode's row had only been seeded that same
     day with no real orders against it yet, and leaving test state on a production row felt wrong
     to leave behind silently.
   - `GET /api/paper-trading/stats?mode=manual` re-confirmed correct after the revert
     (`availableMarginUsd: 40`, no cap).
   - **Still open**: nobody has placed a real cross-margin order through `ManualTrader` to watch
     `ClampSLToCapUSD` actually tighten a stop end-to-end. That's a real-capital action and stays a
     deliberate operator call — do it via the panel's Account page (small manual cap, e.g. $5)
     immediately before the first such order, and grep `docker logs okxbot-trader-1` for
     `"cross-margin guard: tightened..."` when it happens.

4. **The "spot vs futures" distinction was deliberately NOT built** — confirmed with the
   operator that OKX has one unified trading account with no spot/futures split, so the Account
   page shows one balance per exchange rather than inventing a split OKX doesn't have. If a future
   exchange (or OKX itself) genuinely separates them, `domain.Balance`/`ExchangeBalanceSource`
   would need a new field — nothing in this design blocks that, but nothing built it either.

5. **Real trading's config flags (`use_conductor_lifecycle`, `allow_real_money`) are now `true` on
   the server** — this changed in a session after this doc was originally written (evidenced by
   migrations `000032`-`000035` and the `bot_orders`/`manual_orders` rename/tables, none of which
   this doc's own "done" section mentions). The actual trading loop stays gated by
   `paper_trading_config.trading_state`, which reads `'stopped'` for `mode='bot'` as of this
   update — so config alone does not mean real orders are flowing; check that table (or
   `GET /api/health`'s halt block) before assuming otherwise.
