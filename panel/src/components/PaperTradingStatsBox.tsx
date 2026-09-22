import { Link } from 'react-router-dom'
import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import { formatUsd, pnlClass } from '../utils/format'
import type { PaperProfile, PositionMode } from '../api/types'
import BalanceChart from './BalanceChart'

// A PnL figure: the percentage leads because it is the comparable number across windows, with the
// dollar amount beneath in a quieter weight rather than parenthesised on the same line — at three
// tiles per row the old single line had to shrink to fit and read as one run-on value.
function PnLTile({ label, usd, pct }: { label: string; usd: string; pct: string }) {
  const usdNum = Number(usd)
  const pctNum = Number(pct)
  return (
    <div className="stat-tile">
      <div className="stat-label">{label}</div>
      <div className={'stat-value mono ' + pnlClass(usdNum)}>
        {Number.isFinite(pctNum) ? `${pctNum >= 0 ? '+' : ''}${pctNum.toFixed(2)}%` : '—'}
      </div>
      <div className="stat-sub mono">{formatUsd(usdNum)}</div>
    </div>
  )
}

// A plain money figure. Same shape as PnLTile so the two rows align on a common baseline rather
// than each tile sizing itself to its own content.
function StatTile({
  label,
  value,
  title,
  dim,
}: {
  label: string
  value: string
  title?: string
  dim?: boolean
}) {
  return (
    <div className="stat-tile">
      <div className="stat-label" title={title}>
        {label}
      </div>
      <div className={'stat-value mono' + (dim ? ' text-dim' : '')}>{value}</div>
    </div>
  )
}

// Stats box above the Positions table: open order count, Account Balance, Total Equity, and
// 24h/1w/1month realized PnL. The Set Trading Cap control moved to its own Account page
// (2026-09-20 request: capital allocation is a whole-account decision, not something to bury in a
// per-mode positions stats box) — this box links there instead of embedding the control, and Bot
// Trader's own row gains a Reserve tile since bot's cap now interacts with manual's on the SAME
// real balance (see AccountPage). Polls at a slower interval than the position table's own 5s
// poll — this data doesn't need that freshness.
export default function PaperTradingStatsBox({
  mode,
  exchange,
}: {
  mode: PositionMode
  // Which paper-trading profile this box shows (2026-09-22, multi-exchange paper trading) —
  // undefined/'okx' behaves exactly as before this prop existed. Meaningless outside mode='paper'.
  exchange?: PaperProfile | string
}) {
  // Was previously bumped by the Set Trading Cap control's onSaved callback to force an immediate
  // re-poll after the operator changed the cap — that control moved to the Account page
  // (2026-09-20), so nothing in this component changes the cap anymore and refreshSignal now only
  // exists to satisfy BalanceChart's prop contract (its own doc comment: deliberately NOT part of
  // its cache key, see CLAUDE.md §14/§50.2b's history with this exact prop).
  const refreshSignal = 0
  const { data, error } = usePolling(
    () => api.paperTradingStats(mode, exchange),
    15_000,
    [mode, exchange],
    refreshSignal,
  )

  return (
    <div className="card">
      {error && <div className="error-banner">{error}</div>}
      {!data && !error && <div className="text-dim">Loading…</div>}
      {data && (
        <div className="stats-layout">
          {/* Stats on the left, chart on the right (2026-09-12 redesign). The chart was previously
              full-width beneath the tiles, which made it the largest thing on the page while
              carrying the least-consulted information. Three rows of stats, grouped by what they
              answer: how am I doing, what do I hold, what am I risking. */}
          <div className="stats-col">
            {/* Row 1 — performance. */}
            <div className="stats-row stats-row-3">
              <PnLTile label="24h" usd={data.pnl24hUsd} pct={data.pnl24hPct} />
              <PnLTile label="1W" usd={data.pnl7dUsd} pct={data.pnl7dPct} />
              <PnLTile label="1M" usd={data.pnl30dUsd} pct={data.pnl30dPct} />
            </div>

            {/* Row 2 — capital. Reserve is bot-mode only (paper has no exchange balance to hold
                back), so the row is 2-up there and 3-up on bot. */}
            <div className={'stats-row ' + (mode === 'bot' ? 'stats-row-3' : 'stats-row-2')}>
              <StatTile
                label="Balance"
                title="The real, continuous running total — never reset by a trading-cap change"
                value={formatUsd(Number(data.accountBalanceUsd)).replace('+', '')}
              />
              <StatTile
                label="Equity"
                title={
                  mode === 'bot'
                    ? 'The slice of the exchange balance this engine trades with — profit and loss accrue here'
                    : 'The balance since the last chosen trading cap — what new positions size against'
                }
                value={formatUsd(Number(data.totalEquityUsd)).replace('+', '')}
              />
              {mode === 'bot' && (
                <StatTile
                  label="Reserve"
                  dim
                  title="Account Balance minus Total Equity — capital held back from trading. Moves only when you change the cap."
                  value={formatUsd(
                    Number(data.accountBalanceUsd) - Number(data.totalEquityUsd),
                  ).replace('+', '')}
                />
              )}
            </div>

            {/* The Set Trading Cap control itself now lives on the Account page (2026-09-20) —
                capital allocation between bot and manual trading is a whole-account decision, and
                showing it here would mean showing half of that decision (this mode's own cap)
                with no visibility into the OTHER mode's claim on the same real balance. */}
            {(mode === 'bot' || mode === 'manual') && (
              <div className="stats-row">
                <Link to="/account" className="text-dim">
                  Manage trading cap on the Account page →
                </Link>
              </div>
            )}
          </div>

          <div className="stats-chart">
            <BalanceChart
              mode={mode}
              exchange={exchange}
              currentEquity={Number(data.totalEquityUsd)}
              currentBalance={Number(data.accountBalanceUsd)}
              refreshSignal={refreshSignal}
            />
          </div>
        </div>
      )}
    </div>
  )
}
