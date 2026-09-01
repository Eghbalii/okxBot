import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import { formatUsd, pnlClass } from '../utils/format'

function PnLTile({ label, usd, pct }: { label: string; usd: string; pct: string }) {
  const usdNum = Number(usd)
  const pctNum = Number(pct)
  return (
    <div>
      <div className="text-dim" style={{ fontSize: '0.8rem' }}>
        {label}
      </div>
      <div className={'mono ' + pnlClass(usdNum)} style={{ fontSize: '1.1rem' }}>
        {Number.isFinite(pctNum) ? `${pctNum >= 0 ? '+' : ''}${pctNum.toFixed(2)}%` : '—'}{' '}
        ({formatUsd(usdNum)})
      </div>
    </div>
  )
}

// Stats box above the Positions table (2026-09-01 request): open order count, total account
// equity, and 24h/1w/1month realized PnL. Polls at a slower interval than the position table's own
// 5s poll — this data doesn't need that freshness.
export default function PaperTradingStatsBox() {
  const { data, error } = usePolling(() => api.paperTradingStats(), 15_000)

  return (
    <div className="card">
      {error && <div className="error-banner">{error}</div>}
      {!data && !error && <div className="text-dim">Loading…</div>}
      {data && (
        <div
          style={{
            display: 'grid',
            gridTemplateColumns: 'repeat(auto-fit, minmax(9rem, 1fr))',
            gap: '1rem',
          }}
        >
          <div>
            <div className="text-dim" style={{ fontSize: '0.8rem' }}>
              Open Orders
            </div>
            <div className="mono" style={{ fontSize: '1.1rem' }}>
              {data.openCount}
            </div>
          </div>
          <div>
            <div className="text-dim" style={{ fontSize: '0.8rem' }}>
              Total Equity
            </div>
            <div className="mono" style={{ fontSize: '1.1rem' }}>
              {formatUsd(Number(data.totalEquityUsd)).replace('+', '')}
            </div>
          </div>
          <PnLTile label="24h PnL" usd={data.pnl24hUsd} pct={data.pnl24hPct} />
          <PnLTile label="1W PnL" usd={data.pnl7dUsd} pct={data.pnl7dPct} />
          <PnLTile label="1M PnL" usd={data.pnl30dUsd} pct={data.pnl30dPct} />
        </div>
      )}
    </div>
  )
}
