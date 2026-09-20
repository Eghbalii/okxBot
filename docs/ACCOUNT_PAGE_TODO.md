# Account page — follow-up tasks (not built yet, visual placeholders only)

Written 2026-09-20 after a UI/UX redesign pass on `/account`. The per-exchange cards
(`panel/src/components/ExchangeAccountCard.tsx`) now show a row of key-management actions and a
"last synced" / "key added" meta line, but **none of it is wired to anything real**. This file is
where that real work is tracked so it doesn't get lost or, worse, quietly shipped as if it worked.

## 1. Per-exchange API key management (Add / Remove)

Today credentials are environment/config-file only (CLAUDE.md §27.1 — the gateway is the one
process holding real OKX credentials; MEXC's key/secret come from `config.MEXC`). There is no
backend endpoint to add, view, or remove an exchange's credentials from the panel at all.

To build this for real:
- A new table (something like `exchange_credentials`: exchange, api_key, api_secret [+ passphrase
  for OKX], added_at, added_by) — **never** store secrets in plaintext in a column a casual
  `SELECT *` can read; at minimum encrypt at rest, and treat this as a security-review-worthy change
  before shipping, not an ordinary CRUD endpoint.
- `cmd/okx-gateway` / MEXC's REST client would need to read credentials from this table (or be
  restarted after a change) instead of only `config.yaml`/env — a real design question: does
  changing a key require a service restart (matches this project's existing "config changes need a
  restart" posture, §18/§22/§53) or does the gateway need to support a live credential swap?
- `DELETE` should refuse while the exchange has open positions (bot or manual) — removing a key out
  from under a running position would leave it unmanageable.
- Panel: a real modal for entering key/secret/passphrase, never logged, never shown again after
  submission.

## 2. "Key added" date

Trivial once #1 exists — it's just the `added_at` column shown on the card. Not worth building
before #1 since there's nothing to date yet.

## 3. "Sync now" — on-demand balance refresh

Today `GET /api/exchange-balances` is polled every 30s by the panel; there is no way to force an
immediate refresh outside that interval. Two ways to build it:
- Cheapest: the panel just re-polls its own 30s hook early on click (no backend change) — this
  already works AS a "the panel will show fresh data soon" button, it just isn't literally forcing
  the backend to hit the exchange right now.
- More honest to the label "Sync now": a new endpoint that bypasses whatever caching sits between
  the panel and the live exchange call, and returns the fresh number directly rather than relying
  on the next poll tick.

Either way, show a real "Last synced: Xs ago" using the actual response timestamp — the current
card hardcodes "just now," which is exactly the kind of placeholder that must not survive past this
task.

## 4. Multi-exchange capital allocation

`account_equity` (bot/manual's shared trading-cap tables) has no `exchange` column — bot/manual
trading is tied to OKX alone today (`cmd/api/marketscan.go:buildBalanceSources` is the only place
exchanges are enumerated, and only `okx` has a live trader/gateway). The Account page's allocation
donut already reads its exchange list dynamically from `GET /api/exchange-balances` rather than
hardcoding "okx", so a second exchange with real trading wired up would show up in the picker with
zero panel changes — but it would need its OWN `account_equity` rows (a schema change: either an
`exchange` column added to `account_equity`/`SetTradingCap`'s bound query, or a separate table per
exchange) before there'd be real per-exchange bot/manual numbers to show. Don't build the schema
change speculatively; build it when a second exchange actually gets real trading.
