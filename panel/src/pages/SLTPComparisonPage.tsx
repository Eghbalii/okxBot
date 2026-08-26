import { useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import type { SLTPAdjustmentPair, VariantStats } from '../api/types'

// Baseline-vs-rl_adjusted A/B comparison (CLAUDE.md §15.4): every time the RL agent proposes an
// in-trade SL/TP adjustment, the original order is left untouched and a linked fork carries the
// adjustment instead — this page is where those paired outcomes are actually compared, rather
// than left as a manual SQL query against paper_orders.
type SinceFilter = 'all' | '24h' | '7d' | '30d'

function sinceISO(filter: SinceFilter): string | undefined {
  const days = { all: undefined, '24h': 1, '7d': 7, '30d': 30 }[filter]
  if (!days) return undefined
  return new Date(Date.now() - days * 86_400_000).toISOString()
}

function winRatePct(v: VariantStats): string {
  if (v.ClosedCount === 0) return '—'
  return ((v.Wins / v.ClosedCount) * 100).toFixed(1) + '%'
}

function pnlClass(pnl: string): string {
  return Number(pnl) < 0 ? 'text-dim' : ''
}

function VariantCard({ label, stats }: { label: string; stats: VariantStats | undefined }) {
  const s: VariantStats = stats ?? { Variant: 'baseline', ClosedCount: 0, Wins: 0, Losses: 0, RealizedPnL: '0' }
  return (
    <div className="card" style={{ flex: 1 }}>
      <h3 style={{ marginTop: 0 }}>{label}</h3>
      <div className="stat-row">
        <span className="text-dim">Closed trades</span>
        <span className="mono">{s.ClosedCount}</span>
      </div>
      <div className="stat-row">
        <span className="text-dim">Win rate</span>
        <span className="mono">
          {winRatePct(s)} ({s.Wins}W / {s.Losses}L)
        </span>
      </div>
      <div className="stat-row">
        <span className="text-dim">Realized PnL</span>
        <span className={'mono ' + pnlClass(s.RealizedPnL)}>{s.RealizedPnL}</span>
      </div>
    </div>
  )
}

export default function SLTPComparisonPage() {
  const [instId, setInstId] = useState('')
  const [since, setSince] = useState<SinceFilter>('7d')

  const { data: stats, error: statsError } = usePolling(
    () => api.sltpAdjustmentStats({ instId: instId || undefined, since: sinceISO(since) }),
    10_000,
    [instId, since],
  )
  const { data: pairs, error: pairsError } = usePolling(
    () => api.sltpAdjustmentPairs({ instId: instId || undefined }),
    10_000,
    [instId],
  )

  const baseline = stats?.find((s) => s.Variant === 'baseline')
  const adjusted = stats?.find((s) => s.Variant === 'rl_adjusted')

  return (
    <div>
      <div className="toolbar">
        <input
          type="text"
          placeholder="filter by instrument"
          value={instId}
          onChange={(e) => setInstId(e.target.value)}
        />
        <select value={since} onChange={(e) => setSince(e.target.value as SinceFilter)}>
          <option value="all">All time</option>
          <option value="24h">Last 24h</option>
          <option value="7d">Last 7 days</option>
          <option value="30d">Last 30 days</option>
        </select>
      </div>

      {(statsError || pairsError) && <div className="error-banner">{statsError || pairsError}</div>}

      <p className="text-dim" style={{ marginTop: 0 }}>
        Only baseline orders the RL agent actually proposed an SL/TP adjustment for are counted
        here — a baseline order with no fork isn't evidence either way (CLAUDE.md §15.4).
      </p>

      <div style={{ display: 'flex', gap: '1rem', marginBottom: '1rem' }}>
        <VariantCard label="Baseline (untouched)" stats={baseline} />
        <VariantCard label="RL-adjusted" stats={adjusted} />
      </div>

      <div className="card">
        <h3 style={{ marginTop: 0 }}>Paired trades</h3>
        <table>
          <thead>
            <tr>
              <th>Instrument</th>
              <th>Side</th>
              <th>Entry</th>
              <th>Baseline SL/TP</th>
              <th>Adjusted SL/TP</th>
              <th>Baseline result</th>
              <th>Adjusted result</th>
            </tr>
          </thead>
          <tbody>
            {pairs?.map((p: SLTPAdjustmentPair) => (
              <tr key={p.RLAdjustedOrder.ID}>
                <td>{p.InstID}</td>
                <td>
                  <span className={'badge ' + (p.BaselineOrder.Side === 'buy' ? 'badge-green' : 'badge-red')}>
                    {p.BaselineOrder.Side === 'buy' ? 'long' : 'short'}
                  </span>
                </td>
                <td className="mono">{p.BaselineOrder.EntryPx}</td>
                <td className="mono text-dim">
                  {p.BaselineOrder.SLPx ?? '—'} / {p.BaselineOrder.TPPx ?? '—'}
                </td>
                <td className="mono text-dim">
                  {p.RLAdjustedOrder.SLPx ?? '—'} / {p.RLAdjustedOrder.TPPx ?? '—'}
                </td>
                <td className={p.BaselineOrder.RealizedPnL ? pnlClass(p.BaselineOrder.RealizedPnL) : ''}>
                  {p.BaselineOrder.CloseReason ?? 'open'} {p.BaselineOrder.RealizedPnL ? `(${p.BaselineOrder.RealizedPnL})` : ''}
                </td>
                <td className={p.RLAdjustedOrder.RealizedPnL ? pnlClass(p.RLAdjustedOrder.RealizedPnL) : ''}>
                  {p.RLAdjustedOrder.CloseReason ?? 'open'} {p.RLAdjustedOrder.RealizedPnL ? `(${p.RLAdjustedOrder.RealizedPnL})` : ''}
                </td>
              </tr>
            ))}
            {pairs?.length === 0 && (
              <tr>
                <td colSpan={7} className="text-dim">
                  No SL/TP-adjustment pairs yet.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
