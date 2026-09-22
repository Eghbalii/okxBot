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

## What's left before an actual MEXC paper-trading comparison can run

Ordered roughly by dependency:

1. **MEXC credentials.** Need real API key/secret for a MEXC account with futures access, placed
   as env vars on the server (`MEXC_API_KEY`/`MEXC_API_SECRET`) — never on the local machine, same
   rule as OKX credentials (CLAUDE.md's own standing instruction).

2. **A second gateway container.** `docker-compose.yml` needs a new service (e.g.
   `okx-gateway-mexc`) — same image as `okx-gateway`, different `GATEWAY_EXCHANGE=mexc` env, its
   own port (e.g. `8096`, since `8094` is OKX's), its own `MEXC_API_KEY`/`MEXC_API_SECRET`. Model
   this on the existing `okx-gateway` service block.

3. **Mode-scoped storage for MEXC.** The operator wants separate stats/PnL. `bot_orders` already
   has no `mode` column (per CLAUDE.md §33's history, "the table is the discriminator" —
   `paper_orders` vs `bot_orders` vs `real_orders`/`manual_orders` are separate tables, not one
   table with a mode flag). Two real options, needs a decision:
   - (a) Add a `mode` or `exchange` column to `bot_orders` and filter every existing query by it
     (touches `internal/postgres/bot_orders.go`'s every SELECT — the same shape of change as the
     SL/TP-split migration this session already did to that file, so the pattern is fresh).
   - (b) A new table (`mexc_bot_orders` or similar), mirroring `bot_orders`' schema exactly,
     with its own repository methods. More isolated, more duplication.
   Given the operator's explicit "maximize shared code" instruction, (a) is probably closer to
   the spirit of the ask — worth confirming with the operator directly rather than assuming.

4. **A second `cmd/trader` instance.** Needs its own config (`configs/config.mexc.yaml` or
   similar) pointing `Gateway.URL` at the new MEXC gateway's port, its own `trading.inst_ids`
   (MEXC symbols are plain `BTC_USDT` etc., no `symbol_map` needed per the identity resolver),
   `risk.max_leverage: 100` (the operator's explicit ask for this test), and whatever mode value
   item 3 settles on. `docker-compose.yml` needs a new `trader-mexc` service block, same image as
   `trader`, different config/env.

5. **Panel visibility (lower priority for a 24h throwaway test, but worth knowing it's missing)**:
   the panel's Positions page currently only has Paper/Real tabs (CLAUDE.md §34) — no MEXC view
   exists. For a quick comparison, direct DB queries or a temporary Grafana panel are probably
   faster than building panel UI for a test that may be thrown away.

6. **MEXC private WS (optional, not blocking).** No account-push client exists yet
   (`internal/mexc/ws` has only `public.go`). The gateway change in this session already scopes
   the private-stream wiring to OKX only, so a MEXC gateway instance runs correctly without it —
   just on reconciliation-poll-only visibility, matching how this whole system worked before the
   OKX private WS existed.

## Recommended next step

Item 2 (second gateway container) and item 3's decision are the actual unblocking work — item 4
is mostly copy-paste once those two are settled. Get the operator's confirmation on 3(a) vs 3(b)
before writing the migration, since it affects every existing query against `bot_orders`.
