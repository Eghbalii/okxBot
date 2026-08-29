import { useMemo, useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { usePositionAlerts } from '../hooks/usePositionAlerts'
import { usePositionEvents } from '../hooks/usePositionEvents'
import { usePriceStream } from '../hooks/usePriceStream'
import OrderDetailModal from '../components/OrderDetailModal'
import { api } from '../api/client'
import type { CloseReason, Position, PositionMode } from '../api/types'

type SortField = 'opened_at' | 'closed_at' | 'pnl' | 'inst_id'
type OpenFilter = 'all' | 'open' | 'closed'

function closeReasonBadge(reason: CloseReason | null) {
  if (!reason) return <span className="badge badge-dim">open</span>
  const cls = reason === 'tp' ? 'badge-green' : reason === 'sl' ? 'badge-red' : 'badge-dim'
  return <span className={'badge ' + cls}>{reason}</span>
}

// Unrealized PnL computed client-side from entry_px/size/leverage against the live streamed price
// (CLAUDE.md §11.4) — mirrors the same math usecase.unrealizedPnLPct uses Go-side, just so the
// panel doesn't need a REST round-trip for something it can derive locally from data it already has.
function unrealizedPnL(p: Position, lastPrice: string | undefined): { pct: number; usd: number } | null {
  if (p.ClosedAt) return null // realized, not unrealized — RealizedPnL covers this case
  if (!lastPrice) return null
  const entry = Number(p.EntryPx)
  const last = Number(lastPrice)
  const size = Number(p.Size)
  const leverage = Number(p.Leverage)
  if (!entry || !Number.isFinite(last)) return null
  const direction = p.Side === 'buy' ? 1 : -1
  const pct = (direction * (last - entry) * 100) / entry
  const usd = ((direction * (last - entry)) / entry) * size * (leverage || 1)
  return { pct, usd }
}

export default function PositionsPage() {
  const [mode, setMode] = useState<PositionMode | 'all'>('all')
  const [instId, setInstId] = useState('')
  // Defaults to open-only: the panel's job is to show real open positions, not the full historical
  // log — closed trades are still one click away via the filter.
  const [openFilter, setOpenFilter] = useState<OpenFilter>('open')
  const [sortBy, setSortBy] = useState<SortField>('opened_at')
  const [sortDesc, setSortDesc] = useState(true)
  const [alertsEnabled, setAlertsEnabled] = useState(true)
  const [wsRefreshCount, setWsRefreshCount] = useState(0)
  const [detailOrderId, setDetailOrderId] = useState<number | null>(null)

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
    wsRefreshCount,
  )

  // CLAUDE.md §11.4/§12: cmd/api pushes a message over WebSocket the moment a paper order
  // opens/closes; rather than that event carrying full position detail, it just triggers an
  // immediate refetch here — usePositionAlerts' existing diff-the-snapshot logic then fires the
  // sound/notification off of that fresher data. The 5s poll above still runs as a fallback/
  // consistency check independent of the socket's connection state.
  usePositionEvents(() => setWsRefreshCount((c) => c + 1), true)

  usePositionAlerts(data, alertsEnabled)

  const livePrices = usePriceStream(true)

  // Resolved from the current poll's data rather than held in state, so an open modal keeps showing
  // fresh values (live PnL, a close that just landed) instead of a snapshot frozen at click time.
  const detailPosition = detailOrderId === null ? null : (data?.find((p) => p.ID === detailOrderId) ?? null)

  // A baseline order counts as "updated" if some other row in this same response is a fork of it
  // (CLAUDE.md §15.4/§15.12: the RL controller never edits SL/TP in place, it creates a linked
  // 'rl_adjusted' copy instead) — derived client-side from ParentOrderID rather than a new backend
  // field, since the panel already receives every row needed to compute this.
  const updatedOrderIds = useMemo(() => {
    const ids = new Set<number>()
    for (const p of data ?? []) {
      if (p.ParentOrderID != null) ids.add(p.ParentOrderID)
    }
    return ids
  }, [data])

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
        <div className="table-scroll">
        <table>
          <thead>
            <tr>
              <th>ID</th>
              <th onClick={() => toggleSort('inst_id')}>Instrument {sortBy === 'inst_id' && (sortDesc ? '▼' : '▲')}</th>
              <th>Mode</th>
              <th>Side</th>
              <th>Strategy</th>
              <th>Timeframe</th>
              <th>Entry</th>
              <th>Last</th>
              <th>SL / TP</th>
              <th>Leverage</th>
              <th>Entry Volume</th>
              <th onClick={() => toggleSort('opened_at')}>
                Opened {sortBy === 'opened_at' && (sortDesc ? '▼' : '▲')}
              </th>
              <th onClick={() => toggleSort('closed_at')}>
                Closed {sortBy === 'closed_at' && (sortDesc ? '▼' : '▲')}
              </th>
              <th>Reason</th>
              <th onClick={() => toggleSort('pnl')}>PnL {sortBy === 'pnl' && (sortDesc ? '▼' : '▲')}</th>
              <th>Updated</th>
            </tr>
          </thead>
          <tbody>
            {data?.map((p: Position) => {
              const lastPrice = livePrices[p.InstID]
              const live = unrealizedPnL(p, lastPrice)
              const isFork = p.Variant === 'rl_adjusted'
              const wasUpdated = updatedOrderIds.has(p.ID)
              return (
                <tr key={p.ID}>
                  <td>
                    <button
                      className="order-id-button"
                      onClick={() => setDetailOrderId(p.ID)}
                      title="Show the strategy signal vs. the model's decision for this order"
                    >
                      #{p.ID}
                    </button>
                  </td>
                  <td>
                    {p.InstID}
                    {isFork && (
                      <span className="badge badge-dim" style={{ marginLeft: '0.4rem' }} title="RL shadow-fork: tracking-only copy comparing an adjusted SL/TP against its baseline parent">
                        fork
                      </span>
                    )}
                  </td>
                  <td>
                    <span className="badge badge-dim">{p.Mode}</span>
                  </td>
                  <td>
                    <span className={'badge ' + (p.Side === 'buy' ? 'badge-green' : 'badge-red')}>
                      {p.Side === 'buy' ? 'long' : 'short'}
                    </span>
                  </td>
                  <td>{p.StrategyName || '—'}</td>
                  <td className="mono text-dim">{p.Bar || '—'}</td>
                  <td className="mono">{p.EntryPx}</td>
                  <td className="mono">{lastPrice ?? '—'}</td>
                  <td className="mono text-dim">
                    {p.SLPx ?? '—'} / {p.TPPx ?? '—'}
                  </td>
                  <td className="mono">{p.Leverage}x</td>
                  <td className="mono">${Number(p.Size).toLocaleString()}</td>
                  <td>{new Date(p.OpenedAt).toLocaleString()}</td>
                  <td>{p.ClosedAt ? new Date(p.ClosedAt).toLocaleString() : '—'}</td>
                  <td>{closeReasonBadge(p.CloseReason)}</td>
                  <td className={(p.RealizedPnL ? Number(p.RealizedPnL) : live?.usd ?? 0) < 0 ? 'text-dim' : ''}>
                    {p.RealizedPnL !== null
                      ? p.RealizedPnL
                      : live
                        ? `${live.pct >= 0 ? '+' : ''}${live.pct.toFixed(2)}% (${live.usd >= 0 ? '+' : ''}$${live.usd.toFixed(2)})`
                        : '—'}
                  </td>
                  <td>
                    {wasUpdated ? (
                      <span className="badge badge-green">yes</span>
                    ) : (
                      <span className="badge badge-dim">no</span>
                    )}
                  </td>
                </tr>
              )
            })}
            {data?.length === 0 && (
              <tr>
                <td colSpan={15} className="text-dim">
                  No positions match this filter.
                </td>
              </tr>
            )}
          </tbody>
        </table>
        </div>
      </div>

      {detailPosition && (
        <OrderDetailModal position={detailPosition} onClose={() => setDetailOrderId(null)} />
      )}
    </div>
  )
}
