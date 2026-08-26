import { useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { usePositionAlerts } from '../hooks/usePositionAlerts'
import { api } from '../api/client'
import type { CloseReason, Position, PositionMode } from '../api/types'

type SortField = 'opened_at' | 'closed_at' | 'pnl' | 'inst_id'
type OpenFilter = 'all' | 'open' | 'closed'

function closeReasonBadge(reason: CloseReason | null) {
  if (!reason) return <span className="badge badge-dim">open</span>
  const cls = reason === 'tp' ? 'badge-green' : reason === 'sl' ? 'badge-red' : 'badge-dim'
  return <span className={'badge ' + cls}>{reason}</span>
}

export default function PositionsPage() {
  const [mode, setMode] = useState<PositionMode | 'all'>('all')
  const [instId, setInstId] = useState('')
  const [openFilter, setOpenFilter] = useState<OpenFilter>('all')
  const [sortBy, setSortBy] = useState<SortField>('opened_at')
  const [sortDesc, setSortDesc] = useState(true)
  const [alertsEnabled, setAlertsEnabled] = useState(true)

  const { data, error } = usePolling(
    () =>
      api.listPositions({
        mode: mode === 'all' ? undefined : mode,
        instId: instId || undefined,
        open: openFilter === 'all' ? undefined : openFilter === 'open',
        sortBy,
        sortDesc,
      }),
    5_000,
    [mode, instId, openFilter, sortBy, sortDesc],
  )

  usePositionAlerts(data, alertsEnabled)

  function toggleSort(field: SortField) {
    if (sortBy === field) {
      setSortDesc((d) => !d)
    } else {
      setSortBy(field)
      setSortDesc(true)
    }
  }

  return (
    <div>
      <div className="toolbar">
        <select value={mode} onChange={(e) => setMode(e.target.value as PositionMode | 'all')}>
          <option value="all">All modes</option>
          <option value="paper">Paper</option>
          <option value="demo">Demo</option>
          <option value="real">Real</option>
        </select>
        <select value={openFilter} onChange={(e) => setOpenFilter(e.target.value as OpenFilter)}>
          <option value="all">Open + Closed</option>
          <option value="open">Open only</option>
          <option value="closed">Closed only</option>
        </select>
        <input
          type="text"
          placeholder="filter by instrument"
          value={instId}
          onChange={(e) => setInstId(e.target.value)}
        />
        <label style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
          <input
            type="checkbox"
            checked={alertsEnabled}
            onChange={(e) => setAlertsEnabled(e.target.checked)}
          />
          <span className="text-dim">Sound + notifications</span>
        </label>
      </div>

      {error && <div className="error-banner">{error}</div>}

      <div className="card">
        <table>
          <thead>
            <tr>
              <th onClick={() => toggleSort('inst_id')}>Instrument {sortBy === 'inst_id' && (sortDesc ? '▼' : '▲')}</th>
              <th>Mode</th>
              <th>Side</th>
              <th>Entry</th>
              <th>SL / TP</th>
              <th onClick={() => toggleSort('opened_at')}>
                Opened {sortBy === 'opened_at' && (sortDesc ? '▼' : '▲')}
              </th>
              <th onClick={() => toggleSort('closed_at')}>
                Closed {sortBy === 'closed_at' && (sortDesc ? '▼' : '▲')}
              </th>
              <th>Reason</th>
              <th onClick={() => toggleSort('pnl')}>PnL {sortBy === 'pnl' && (sortDesc ? '▼' : '▲')}</th>
            </tr>
          </thead>
          <tbody>
            {data?.map((p: Position) => (
              <tr key={p.ID}>
                <td>{p.InstID}</td>
                <td>
                  <span className="badge badge-dim">{p.Mode}</span>
                </td>
                <td>
                  <span className={'badge ' + (p.Side === 'buy' ? 'badge-green' : 'badge-red')}>
                    {p.Side === 'buy' ? 'long' : 'short'}
                  </span>
                </td>
                <td className="mono">{p.EntryPx}</td>
                <td className="mono text-dim">
                  {p.SLPx ?? '—'} / {p.TPPx ?? '—'}
                </td>
                <td>{new Date(p.OpenedAt).toLocaleString()}</td>
                <td>{p.ClosedAt ? new Date(p.ClosedAt).toLocaleString() : '—'}</td>
                <td>{closeReasonBadge(p.CloseReason)}</td>
                <td className={p.RealizedPnL && Number(p.RealizedPnL) < 0 ? 'text-dim' : ''}>
                  {p.RealizedPnL ?? '—'}
                </td>
              </tr>
            ))}
            {data?.length === 0 && (
              <tr>
                <td colSpan={9} className="text-dim">
                  No positions match this filter.
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
