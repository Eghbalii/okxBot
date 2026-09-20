import { useEffect, useMemo, useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import ExchangeAccountCard from '../components/ExchangeAccountCard'
import CapitalDonut, { type DonutSlice } from '../components/CapitalDonut'
import { ExchangeIcon } from '../components/TokenIcon'
import TradingCapControl from '../components/TradingCapControl'

// The Account page (2026-09-20 request, redesigned same day after direct UI/UX feedback): where
// real money lives across every configured exchange, and where capital is split between bot
// trading and manual trading — both of which draw on the SAME real exchange balance today, not
// two independent pools (see the note on CAPITAL ALLOCATION IS PER-EXCHANGE below).
//
// Redesign notes, so the reasoning survives past this session:
//   - The first version put a big number in a big box for each exchange, with nothing else —
//     correctly called out as "sizing a whole card just to show one number." Exchange identity
//     (logo + name) now leads; balance is a secondary line; and there is a real row of
//     key-management actions underneath (Sync / Remove / Add) rather than nothing. Those actions
//     are visual-only placeholders for now — see ExchangeAccountCard's own comment and
//     docs/ACCOUNT_PAGE_TODO.md for the follow-up task.
//   - Capital allocation moved from two bare sliders to a donut chart (Bot / Manual / Reserve),
//     which is the thing the operator actually wants a felt sense of at a glance: not two numbers
//     to compare, but one balance and how it's currently carved up.
//
// CAPITAL ALLOCATION IS PER-EXCHANGE, NOT ACROSS EXCHANGES: bot/manual trading's shared balance
// (Repository.SetTradingCap's own "realMoneyModes" bound) is tied to ONE exchange today — there is
// no `exchange` column on account_equity, and only OKX has a live trader/gateway wired to it
// (cmd/api/marketscan.go's buildBalanceSources). A second exchange with real trading would need
// its own bot/manual split; nothing here pretends otherwise by pooling balances across exchanges.
// The exchange picker below is built from the SAME dynamic `balances` list the top section reads
// (never a hardcoded exchange name), filtered to configured ones — so a newly-configured exchange
// appears here with zero code changes, and one with no credentials is correctly absent rather than
// shown with invented numbers.
function fmtUsd0(n: number): string {
  if (!Number.isFinite(n)) return '—'
  return `$${n.toFixed(2)}`
}

export default function AccountPage() {
  const [refreshSignal, setRefreshSignal] = useState(0)
  const [allocExchange, setAllocExchange] = useState<string | null>(null)

  const { data: balances, error: balancesError } = usePolling(
    () => api.exchangeBalances(),
    30_000,
    [],
    refreshSignal,
  )
  const { data: botStats, error: botError } = usePolling(
    () => api.paperTradingStats('bot'),
    15_000,
    [],
    refreshSignal,
  )
  const { data: manualStats, error: manualError } = usePolling(
    () => api.paperTradingStats('manual'),
    15_000,
    [],
    refreshSignal,
  )

  // Total assets across every CONFIGURED exchange — an exchange with no credentials contributes
  // nothing (not a $0 that would misleadingly imply an empty real account).
  const totalAssetsUsd = (balances ?? [])
    .filter((b) => b.configured && !b.err)
    .reduce((sum, b) => sum + Number(b.equityUsd), 0)

  // Exchanges with a real, reachable balance — the only ones that could possibly host bot/manual
  // trading capital, so the only ones offered in the allocation picker below. Built fresh from the
  // same list the cards above render, never a hardcoded name.
  const tradableExchanges = useMemo(
    () => (balances ?? []).filter((b) => b.configured && !b.err).map((b) => b.exchange),
    [balances],
  )

  useEffect(() => {
    if (allocExchange && tradableExchanges.includes(allocExchange)) return
    setAllocExchange(tradableExchanges[0] ?? null)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tradableExchanges.join(',')])

  // Bot and manual trading draw on the SAME real exchange balance (Repository.SetTradingCap's own
  // doc comment) — accountBalanceUsd is that shared number, reported independently by each mode's
  // own account_equity row but mirroring the identical exchange-reported total either way.
  const sharedBalanceUsd = Number(botStats?.accountBalanceUsd ?? manualStats?.accountBalanceUsd ?? 0)
  const botEquity = Number(botStats?.totalEquityUsd ?? 0)
  const manualEquity = Number(manualStats?.totalEquityUsd ?? 0)
  const reserveUsd = Math.max(0, sharedBalanceUsd - botEquity - manualEquity)

  const donutSlices: DonutSlice[] = [
    { label: 'Bot Trader', usd: botEquity, color: 'var(--accent)' },
    { label: 'Manual trading', usd: manualEquity, color: 'var(--yellow)' },
    { label: 'Reserve (unallocated)', usd: reserveUsd, color: 'var(--border)' },
  ]

  // Each control's own upper bound is what's left for THAT mode once the other's current claim
  // (its own totalEquityUsd, which IS its cap once one is set) is set aside — mirrors exactly what
  // the backend's SetTradingCap enforces server-side.
  const botMax = Math.max(0, sharedBalanceUsd - manualEquity)
  const manualMax = Math.max(0, sharedBalanceUsd - botEquity)

  const noCapSet = botStats && manualStats && sharedBalanceUsd > 0 &&
    Number(botStats.totalEquityUsd) === sharedBalanceUsd &&
    Number(manualStats.totalEquityUsd) === sharedBalanceUsd

  return (
    <div className="account-page">
      <div className="card">
        <h2>Total Assets</h2>
        {balancesError && <div className="error-banner">{balancesError}</div>}
        <div className="stat-tile" style={{ marginBottom: '1rem' }}>
          <div className="stat-label">Across every configured exchange</div>
          <div className="stat-value mono">{fmtUsd0(totalAssetsUsd)}</div>
        </div>
        <div className="xcard-grid">
          {(balances ?? []).map((b) => (
            <ExchangeAccountCard key={b.exchange} b={b} />
          ))}
        </div>
      </div>

      <div className="card">
        <div className="alloc-head">
          <h2>Capital Allocation</h2>
          {tradableExchanges.length > 1 && (
            <div className="alloc-picker">
              {tradableExchanges.map((ex) => (
                <button
                  key={ex}
                  className={'alloc-picker-btn' + (allocExchange === ex ? ' active' : '')}
                  onClick={() => setAllocExchange(ex)}
                >
                  <ExchangeIcon exchange={ex} size={16} />
                  {ex.toUpperCase()}
                </button>
              ))}
            </div>
          )}
        </div>
        <p className="text-dim">
          Bot Trader and manual (Trade page) orders both draw on the same real{' '}
          {allocExchange ? allocExchange.toUpperCase() : 'exchange'} balance — this decides how much
          of it each is allowed to use. Raising one lowers how much room the other has left.
        </p>

        {!allocExchange ? (
          <p className="text-dim">No exchange with a reachable balance is available to allocate yet.</p>
        ) : (
          <>
            {noCapSet && (
              <div className="alloc-warning">
                No cap has been set for either mode yet — both are currently reading the FULL shared
                balance as "available," which means they can jointly over-commit it if both trade at
                once. Set a cap below for at least one mode to split the balance for real.
              </div>
            )}

            <div className="alloc-body">
              <CapitalDonut slices={donutSlices} totalUsd={sharedBalanceUsd} centerLabel="shared balance" />

              <div className="alloc-controls">
                <div>
                  {botError && <div className="error-banner">{botError}</div>}
                  <TradingCapControl
                    mode="bot"
                    label="Bot Trader cap"
                    maxAvailable={botMax}
                    current={botEquity}
                    onSaved={() => setRefreshSignal((n) => n + 1)}
                  />
                  {botStats && (
                    <p className="text-dim" style={{ marginTop: '0.4rem' }}>
                      {fmtUsd0(Number(botStats.usedMarginUsd))} in open positions ·{' '}
                      {fmtUsd0(Number(botStats.availableMarginUsd))} free to trade with
                    </p>
                  )}
                </div>
                <div>
                  {manualError && <div className="error-banner">{manualError}</div>}
                  <TradingCapControl
                    mode="manual"
                    label="Manual trading cap"
                    maxAvailable={manualMax}
                    current={manualEquity}
                    onSaved={() => setRefreshSignal((n) => n + 1)}
                  />
                  {manualStats && (
                    <p className="text-dim" style={{ marginTop: '0.4rem' }}>
                      {fmtUsd0(Number(manualStats.usedMarginUsd))} in open positions ·{' '}
                      {fmtUsd0(Number(manualStats.availableMarginUsd))} free to trade with
                    </p>
                  )}
                </div>
              </div>
            </div>
          </>
        )}
      </div>
    </div>
  )
}
