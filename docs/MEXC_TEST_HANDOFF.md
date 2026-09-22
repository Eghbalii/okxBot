# MEXC paper-trading comparison — handoff (2026-09-22)

## What the operator asked for

Run a small paper-trading test on MEXC at 100x max leverage, in parallel with the existing OKX
setup, so the two exchanges' behavior/PnL can be compared over ~24h. Explicit requirements:

1. **Separate running instance**, not a switch — OKX must keep running unmodified while MEXC
   runs alongside it.
2. **Maximize shared code.** This project's whole reason for the ports-and-adapters design (§10)
   is multi-exchange support from day one — the operator was explicit that the two exchanges
   should be "as much the same code as possible," not a forked/duplicated implementation.
3. **Separate stats/PnL** so the two are comparable — a fresh, zeroed record for MEXC, not mixed
   into OKX's existing `bot_orders` history.
4. Model backup taken first (done — see below), since this is explicitly a throwaway first
   attempt that may be discarded.

## What's already true before this session (don't re-verify from scratch)

- `internal/mexc/rest.Client` and `internal/mexc/ws.PublicClient` already exist and already
  satisfy `port.ExchangeClient` (compile-time-asserted in `internal/mexc/adapter.go`). Verified
  live against real public MEXC endpoints in an earlier session (CLAUDE.md §46).
- MEXC's symbol format needs no mapping table (`BTC_USDT`, stable, no expiry) — `NewSymbolResolver()`
  in `internal/mexc/adapter.go` returns `port.IdentitySymbolResolver{}`.
- MEXC has **no passphrase**, no cross/isolated demo-vs-real split OKX has — `cfg.MEXC.APIKey`/
  `APISecret`/`RESTBaseURL`/`PublicWSURL`/`PrivateWSURL` already exist in `internal/config/config.go`
  with env-only credential loading (`MEXC_API_KEY`/`MEXC_API_SECRET`), same secure pattern as OKX.
- `cmd/okx-gateway`'s routes/limiter/retry/metrics were already exchange-agnostic BEFORE this
  session (§46.4) — proven by `cmd/okx-gateway/multiexchange_test.go`, which runs the real
  service with a MEXC-shaped client and asserts it works end to end.
- **The authenticated half of MEXC (real orders, positions, balance) has never run against a real
  account.** Only public market data has been live-verified.

## What THIS session did (committed, tested, NOT deployed)

Commit: `feat(gateway): make cmd/okx-gateway exchange-parameterized for MEXC`

1. Added `cfg.Gateway.Exchange` (`GATEWAY_EXCHANGE` env, default `"okx"`) to
   `internal/config/config.go`.
2. Rewrote `cmd/okx-gateway/main.go`'s construction into `buildService(cfg, logger)`, which
   branches on `cfg.Gateway.Exchange` and constructs either `okx/rest.Client` (existing, default)
   or `mexc/rest.Client` (new branch, `simulated` hardcoded false — MEXC has no demo environment).
   An unknown exchange value is refused at startup rather than silently falling back to OKX.
3. Found and fixed a real compile-time gap: `mexc/rest.Client` didn't implement `GetOrderRaw`
   (the gateway's own richer `exchangeClient` interface needs it for the panel's exchange-report
   passthrough — OKX-only until now). Added `GetOrderRaw` to `internal/mexc/rest/trade.go`,
   mirroring OKX's implementation of the same method.
4. The OKX private account-WS stream (real-time position/order push, CLAUDE.md §35.4) stays
   OKX-only — gated on `cfg.Gateway.Exchange == "okx"` — since `internal/mexc/ws` has no private
   client yet. A MEXC gateway instance runs on the reconciliation poll alone, which §27.6 already
   treats as a legitimate baseline, not a blocking gap.

**Verified**: `go build ./...` clean. `cmd/okx-gateway`, `internal/mexc/rest`, `internal/mexc/ws`,
`internal/config` all pass their own test suites. The exact multi-exchange claim
(`TestGateway_ServesANonOKXClient`, `TestGateway_UsesTheConfiguredRetryPredicate`,
`TestGateway_DefaultsToOKXPredicate`) still passes with the real `buildService` wiring in place,
not just the hand-built fake the original test used.

**NOT done**: nothing was deployed to the server. No second gateway instance is running. No
`cmd/trader` (or paper-trader) instance points at a MEXC gateway. No MEXC credentials have been
placed on the server. No real MEXC order has ever been placed.

## Model backup (done, separately from the gateway work)

Before touching anything, `rl-service` was stopped cleanly and the model + replay buffer were
backed up in three places, all checksum-verified identical:
- Running service's own files: `/opt/okxBot/rl-service/models/{sac_global.zip,sac_global_buffer.pkl}`
- Server backup: `/opt/okxBot/models/backup/20260922-pre-mexc-test/`
- Local: `/Users/rez/go/src/okxBot/models/backup/20260922-pre-mexc-test/` (gitignored, ~76MB total)

md5: `sac_global.zip` = `8f9e3c6a875398b21bcf60c48763c290`,
`sac_global_buffer.pkl` = `9bc22605c6b2acbf9c142bbc0bf9dcc8`.

`rl-service` was restarted afterward and confirmed healthy — `completed_trades`/`updates` reset
to 0 (this is a **per-process session counter**, not data loss, per CLAUDE.md §53.7's own
documented finding) but the replay buffer's actual content survived (`buffer_size: 100000` in
`/health`, matching the buffer's full pre-restart capacity — an empty buffer would read `0`).

## Two unrelated pre-existing test hangs, found but NOT investigated

While running the full suite to verify the gateway change, two DIFFERENT tests timed out on
separate runs, both in packages nobody touched this session:
- `internal/strategy.TestPortedKinds_FireOnRealMarketData/open_close_cross` (timed out at 10 min)
- `internal/backtest.TestRun_EverySampleObservationIsValid` and, on a separate run,
  `TestRun_EachTradeHasItsOwnOrderID` (timed out at 60-180s)

Both stack traces point into `internal/strategy`'s indicator math (`ADX`, `smmaOfField`) via
`decimal.Decimal` arithmetic — not networking, not gateway code, not anything from this session's
diff. Every OTHER package (32 total) passes cleanly and quickly when these two are excluded. This
reads as a local-machine load/performance issue (possibly the machine being busy with other work
during this session) rather than a real regression, but **it was explicitly not investigated** —
operator's own instruction was to ignore it and let a future session look if it recurs. If it
recurs on a clean machine with nothing else running, that would be worth taking seriously as a
real bug in `internal/strategy`'s indicator computation (possibly an actual infinite loop, not
just slowness — worth checking `smmaOfField`/`ADX` for a loop bound that can fail to terminate on
certain input shapes).

## Update 2026-09-22: scope changed to PAPER TRADING ONLY, and items 2/3/4 are DONE

The operator redirected this mid-session: **only `cmd/paper-trader` runs against MEXC, not
`cmd/trader`/bot trading** — that stays OKX-only and untouched. The operator also asked for a more
general mechanism than a one-off MEXC hack: something that also supports running a *config-variant*
experiment later (e.g. `rl_early_close` on/off) as a repeatable comparison, not just a second
exchange.

Resolution on item 3's (a) vs (b) choice: **neither, exactly** — `mode` (already the key that gives
`paper_orders`/`account_equity`/`paper_trading_config`/`strategy_assignments` their own isolated
balance/config/assignments per instance) stays a closed whitelist (`paper`/`bot`/`demo`/`manual`,
hardcoded across `internal/api`) and is NOT widened. Instead a **new `exchange` column** (default
`'okx'`) was added to all four of those tables (migration `000037`), and `usecase.PaperTrader`
gained an `Exchange` field threaded through every repo call. `mode` stays `"paper"` for both
instances; `exchange` is the new isolation/comparison dimension, and it's a free string — so
`PAPER_EXCHANGE=mexc` for the exchange test, or e.g. `PAPER_EXCHANGE=no_early_close` for a future
config-variant experiment, work identically with no further schema change. See the commit
`feat(paper-trading): add exchange scoping for parallel paper-trading experiments` for the full
implementation (also fixed a real pre-existing-method-breakage bug found while widening
`account_equity`'s key — see the commit message).

**Deployed and verified live** (2026-09-22): migration applied cleanly against production
(2833 pre-existing `paper_orders` rows, 3 `account_equity` rows, all preserved), `paper-trader`/
`api`/`panel` rebuilt one at a time with docker build-cache pruned between builds, monitoring
stopped/restarted around the builds per the server's memory constraints. Existing OKX paper-trading
instance confirmed working identically with and without the new `?exchange=` query param — zero
regression. `docker-compose.yml` gained `okx-gateway-mexc` (port 8096/9106) and `paper-trader-mexc`
(port 8098/9107) service definitions plus `go-engine/configs/config.mexc.yaml` (2-token starter
roster BTC/ETH, 100x leverage per the operator's ask, its own $100 `account.initial_usd`) — **not
yet started**, since MEXC credentials aren't on the server.

Item 2 (second gateway container) and item 4 (second trader instance) are effectively superseded by
this — there is no second `cmd/trader`, only `paper-trader-mexc`, and the gateway container is
already defined in `docker-compose.yml`, just not running.

## Update 2026-09-22 (second pass): no API key needed, panel done, real blocker is MEXC market data

Operator feedback corrected two wrong assumptions from the first pass:

- **Paper trading needs no MEXC API credentials at all.** Positions are virtual; nothing here ever
  calls MEXC's authenticated endpoints (place/cancel order, positions, balance). Only public
  ticker/candle/funding-rate data is needed, and `internal/mexc/rest`'s `doPublic` path sends no
  auth headers. `MEXC_API_KEY`/`MEXC_API_SECRET` were removed from the compose file entirely — the
  gateway starts and serves real MEXC data fine with them unset. The item below about "credentials"
  in the first pass of this doc was simply wrong; don't go looking for a MEXC key.
- Renamed `okx-gateway-mexc` → **`mexc-gateway`** in `docker-compose.yml` (the binary/package is
  still `cmd/okx-gateway`/`Dockerfile.okx-gateway` — those names predate the exchange-parameterized
  design and weren't worth the larger, riskier rename; only the compose service name changed).
- `PAPER_EXCHANGE` for the MEXC instance is **`MEXC_100x_1`** (the operator's own chosen profile
  label), not a bare `mexc` — this label is what appears in the `exchange` column and what the
  panel's profile switcher shows. `account.initial_usd` in `config.mexc.yaml` is **$8** ($4/token ×
  2 starter tokens BTC/ETH, per the operator's explicit instruction), not $100.
- **Panel done**: `PositionsPage` → Paper tab now has a Profile selector (OKX / MEXC_100x_1) above
  the stats/config boxes, threading `?exchange=` through positions/stats/config/balance-history/
  restart. Non-OKX profiles hide the Manage Strategies/Manage Tokens buttons — those modals read the
  discovery-scan `instruments` roster (a different, OKX-only "exchange" concept), not this profile
  label, and wiring them up against the wrong roster would silently corrupt it.

### A deploy-time discovery, fixed same session

The **previous** session's `cmd/okx-gateway` exchange-parameterization commit had been committed to
git but genuinely **never deployed** — the running `okx-gateway`/`trader` binaries on the server
predated it (confirmed via `docker inspect`'s `Created` timestamp, and via `strings` on the running
binary finding zero occurrence of MEXC-specific error strings). This was caught only because the
first live MEXC ticker call through the freshly-built `mexc-gateway` returned an *OKX*-shaped error
— tracing it down found the whole gateway/trader stack was stale. Deployed the missing 3 files,
rebuilt `okx-gateway`+`trader` (both confirmed behavior-preserving for OKX before restarting — real
trading was `stopped` throughout, no capital at risk), and re-verified via `strings` on the rebuilt
binaries this time, not just a successful build exit code. **Lesson for next time: never assume a
prior session's commit reached the server just because a service `git log` says it should have —
check the running binary directly.**

### The real remaining blocker: MEXC has no live market data pipeline

Found while first starting `paper-trader-mexc` for real: it seeded its candle windows from
Postgres's `candles` table and consumed live ticks from Kafka's `okx.tickers`/`okx.candles.<bar>`
topics — **exactly like the OKX instance does**, because `candles` has no `exchange` column
(`PRIMARY KEY (inst_id, bar, ts)` only, migration `000001`) and `cmd/ingestor` has zero MEXC
awareness (`grep -i mexc go-engine/cmd/ingestor/*.go` → nothing). `paper-trader-mexc` was about to
silently trade "MEXC" positions against **OKX's own BTC/ETH price feed**, which would have
invalidated the whole comparison without any visible error. **Stopped `paper-trader-mexc`
immediately** (`docker compose stop paper-trader-mexc`) before it opened anything — confirmed zero
rows exist under `exchange='MEXC_100x_1'` in `paper_orders`. `mexc-gateway` itself stays up and is
verified working correctly against real MEXC data (`curl http://localhost:8096/ticker?instId=BTC_USDT`
returns real live MEXC prices) — the gap is purely that nothing publishes MEXC ticks/candles onto
the Kafka topics `PaperTrader` consumes.

## What's still left

1. **MEXC market data ingestion — the real blocking item, not credentials.** `paper-trader-mexc`
   cannot run correctly until something publishes real MEXC ticks/candles onto Kafka topics it
   reads. Two shapes worth considering, not decided here:
   - Extend `cmd/ingestor` to also subscribe to MEXC's public WS (`internal/mexc/ws.PublicClient`
     already exists and is live-verified per CLAUDE.md §46.6) and publish to exchange-qualified
     topics/DB rows (would need `candles`/Kafka topic naming to carry an exchange dimension, which
     `paper_orders`/`account_equity` etc. now have but `candles` does not).
   - A separate, smaller MEXC-only ingestor process, publishing to its own topics
     (`mexc.tickers`/`mexc.candles.<bar>`) that `paper-trader-mexc` is pointed at via its own Kafka
     consumer config — more isolated, less shared code, but avoids touching `candles`' schema or
     `cmd/ingestor`'s OKX-specific logic at all.
   Whichever is chosen, `paper-trader-mexc` must NOT be restarted/left running until this exists —
   restarting it today reproduces the exact silent-wrong-data bug this update just caught.

2. **MEXC private WS (optional, not blocking, unchanged from the first pass).** No account-push
   client exists yet — irrelevant for paper trading anyway, which has no exchange account to push
   from.

3. **The two pre-existing test hangs from the prior session** (`internal/strategy`,
   `internal/backtest`, decimal-arithmetic-related) — still open, still not investigated, per the
   operator's own instruction to leave them.

## Recommended next step

Decide and build the MEXC market-data ingestion path (item 1 above) before ever starting
`paper-trader-mexc` again. Once real MEXC ticks/candles are flowing, `docker compose up -d
paper-trader-mexc` and watch its logs for live tick/candle consumption (not just the one-time
Postgres seed) before trusting any of its numbers.
