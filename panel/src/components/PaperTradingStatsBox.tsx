import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import { formatUsd, pnlClass } from '../utils/format'

function PnLTile({ label, usd, pct }: { label: string; usd: string; pct: string }) {
  const usdNum = Number(usd)
  const pctNum = Number(pct)
  return (
    <div className="config-tile">
      <div className="config-tile-label">{label}</div>
      <div className={'config-tile-value mono ' + pnlClass(usdNum)}>
        {Number.isFinite(pctNum) ? `${pctNum >= 0 ? '+' : ''}${pctNum.toFixed(2)}%` : '—'}{' '}
        <span className="text-dim" style={{ fontWeight: 400, fontSize: '0.8rem' }}>
          ({formatUsd(usdNum)})
        </span>
      </div>
    </div>
  )
}

// Stats box above the Positions table: open order count, total account equity, and 24h/1w/1month
// realized PnL. Polls at a slower interval than the position table's own 5s poll — this data
// doesn't need that freshness.
export default function PaperTradingStatsBox() {
  const { data, error } = usePolling(() => api.paperTradingStats(), 15_000)

  return (
    <div className="card">
      {error && <div className="error-banner">{error}</div>}
      {!data && !error && <div className="text-dim">Loading…</div>}
      {data && (
        <div className="config-grid">
          <div className="config-tile">
            <div className="config-tile-label">Open Orders</div>
            <div className="config-tile-value mono">{data.openCount}</div>
          </div>
          <div className="config-tile">
            <div className="config-tile-label">Total Equity</div>
            <div className="config-tile-value mono">
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
