import { useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import { ExchangeBadges } from '../components/TokenIcon'
import TradingCapControl from '../components/TradingCapControl'
import type { ExchangeBalance } from '../api/types'

// The Account page (2026-09-20 request): where real money lives across every configured exchange,
// and where capital is split between bot trading and manual trading — both of which draw on the
// SAME real exchange balance, not two independent pools. Previously "Set Trading Cap" lived buried
// inside the Bot Trader positions page's stats box with no visibility into what manual trading (or
// any future trading surface) had already claimed of the same balance; this page is the one place
// that shows the whole picture.

function fmtUsd0(n: number): string {
  if (!Number.isFinite(n)) return '—'
  return `$${n.toFixed(2)}`
}

// Per-exchange balance card, the same shape HomePage's own BalanceRow uses — kept as its own
// smaller copy here rather than a shared extraction, since this page's card additionally needs
// room for the page's own layout (a column of exchange cards beside the trading-cap section)
// rather than HomePage's full-width row.
function ExchangeCard({ b }: { b: ExchangeBalance }) {
  return (
    <div className={'balance-card' + (b.configured ? '' : ' unconfigured')}>
      <div className="balance-exchange">
        <ExchangeBadges exchanges={[b.exchange]} />
        <span>{b.exchange.toUpperCase()}</span>
      </div>
      {/* Three distinct states, never collapsed into one — the same reasoning as HomePage's own
          BalanceRow: a missing key and an empty account mean very different things, and showing
          "$0.00" for either would be a plausible-looking lie about how much capital exists. */}
      {!b.configured ? (
        <div className="balance-amount muted">not configured</div>
      ) : b.err ? (
        <div className="balance-amount error" title={b.err}>
          unavailable
        </div>
      ) : (
        <>
          <div className="balance-amount">
            {fmtUsd0(Number(b.equityUsd))} <span className="balance-ccy">{b.ccy}</span>
          </div>
          <div className="balance-avail">avail {fmtUsd0(Number(b.availUsd))}</div>
        </>
      )}
    </div>
  )
}

export default function AccountPage() {
  const [refreshSignal, setRefreshSignal] = useState(0)

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
  // nothing (not a $0 that would misleadingly imply an empty real account), matching the same
  // "configured vs. reachable vs. real balance" distinction ExchangeCard itself renders.
  const totalAssetsUsd = (balances ?? [])
    .filter((b) => b.configured && !b.err)
    .reduce((sum, b) => sum + Number(b.equityUsd), 0)

  // Bot and manual trading draw on the SAME real exchange balance (Repository.SetTradingCap's own
  // doc comment) — accountBalanceUsd is that shared number, reported independently by each mode's
  // own account_equity row but mirroring the identical exchange-reported total either way, so
  // either stats response is an equally valid source for it. Prefer bot's since it is the
  // longer-established of the two; fall back to manual's if bot hasn't loaded yet.
  const sharedBalanceUsd = Number(botStats?.accountBalanceUsd ?? manualStats?.accountBalanceUsd ?? 0)

  // Each control's own upper bound is what's left for THAT mode once the other's current claim
  // (its own totalEquityUsd, which IS its cap once one is set) is set aside — mirrors exactly what
  // the backend's SetTradingCap enforces server-side (internal/postgres/account_equity.go's
  // realMoneyModes bound), so the slider's own ceiling never promises a larger cap than the
  // backend would actually honor.
  const botMax = Math.max(0, sharedBalanceUsd - Number(manualStats?.totalEquityUsd ?? 0))
  const manualMax = Math.max(0, sharedBalanceUsd - Number(botStats?.totalEquityUsd ?? 0))

  return (
    <div>
      <div className="card">
        <h2>Total Assets</h2>
        {balancesError && <div className="error-banner">{balancesError}</div>}
        <div className="stat-tile" style={{ marginBottom: '1rem' }}>
          <div className="stat-label">Across every configured exchange</div>
          <div className="stat-value mono">{fmtUsd0(totalAssetsUsd)}</div>
        </div>
        <div className="home-balances">{(balances ?? []).map((b) => <ExchangeCard key={b.exchange} b={b} />)}</div>
      </div>

      <div className="card">
        <h2>Trading Cap</h2>
        <p className="text-dim">
          Bot Trader and manual (Trade page) orders both draw on the same real exchange balance
          above — this decides how much of it each is allowed to use. Raising one lowers how much
          room the other has left.
        </p>
        <div className="stats-row stats-row-2">
          <div className="stat-tile">
            <div className="stat-label" title="How much of the shared balance is committed to open positions right now, in either mode">
              Currently used
            </div>
            <div className="stat-value mono">
              {fmtUsd0(Number(botStats?.usedMarginUsd ?? 0) + Number(manualStats?.usedMarginUsd ?? 0))}
            </div>
          </div>
          <div className="stat-tile">
            <div className="stat-label" title="The shared real exchange balance both caps below are bounded against">
              Shared account balance
            </div>
            <div className="stat-value mono">{fmtUsd0(sharedBalanceUsd)}</div>
          </div>
        </div>

        <div className="stats-row stats-row-2" style={{ marginTop: '1rem' }}>
          <div>
            {botError && <div className="error-banner">{botError}</div>}
            <TradingCapControl
              mode="bot"
              label="Bot Trader cap"
              maxAvailable={botMax}
              current={Number(botStats?.totalEquityUsd ?? 0)}
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
              current={Number(manualStats?.totalEquityUsd ?? 0)}
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
    </div>
  )
}
