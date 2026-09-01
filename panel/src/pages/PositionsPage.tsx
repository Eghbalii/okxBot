import { useEffect, useMemo, useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { usePositionAlerts } from '../hooks/usePositionAlerts'
import { usePositionEvents } from '../hooks/usePositionEvents'
import { usePriceStream } from '../hooks/usePriceStream'
import OrderDetailModal from '../components/OrderDetailModal'
import PaperTradingConfigBox from '../components/PaperTradingConfigBox'
import PaperTradingStatsBox from '../components/PaperTradingStatsBox'
import Pagination, { DEFAULT_PAGE_SIZE } from '../components/Pagination'
import SortableTh from '../components/SortableTh'
import { api } from '../api/client'
import { formatDateTime, formatUsd, pnlClass } from '../utils/format'
import type { CloseReason, Position, PositionMode } from '../api/types'

// Sort fields the backend understands, plus the ones resolved client-side below.
type SortField =
  | 'opened_at'
  | 'closed_at'
  | 'pnl'
  | 'inst_id'
  | 'side'
  | 'strategy'
  | 'bar'
  | 'entry'
  | 'leverage'
  | 'size'
  | 'reason'
  | 'id'
type OpenFilter = 'all' | 'open' | 'closed'

function closeReasonBadge(reason: CloseReason | null) {
  if (!reason) return <span className="badge badge-dim">open</span>
  const cls = reason === 'tp' ? 'badge-green' : reason === 'sl' ? 'badge-red' : 'badge-dim'
  return <span className={'badge ' + cls}>{reason}</span>
}

// Unrealized PnL computed client-side from entry_px/size/leverage against the live streamed price
// (CLAUDE.md §11.4) — mirrors the same math usecase.unrealizedPnLPct uses Go-side, just so the
// panel doesn't need a REST round-trip for something it can derive locally from data it already has.
//
// pct is PnL as a fraction of margin (the price move scaled by leverage), matching OKX's own
// uplRatio convention and usd's own leverage multiplication below — before 2026-08-31 pct omitted
// leverage entirely, so a 1% price move at 20x leverage displayed as 1% instead of the correct 20%
// while usd (which did multiply by leverage) was already right.
function unrealizedPnL(p: Position, lastPrice: string | undefined): { pct: number; usd: number } | null {
  if (p.ClosedAt) return null // realized, not unrealized — RealizedPnL covers this case
  if (!lastPrice) return null
  const entry = Number(p.EntryPx)
  const last = Number(lastPrice)
  const size = Number(p.Size)
  const leverage = Number(p.Leverage) || 1
  if (!entry || !Number.isFinite(last)) return null
  const direction = p.Side === 'buy' ? 1 : -1
  const pct = (direction * (last - entry) * 100 * leverage) / entry
  const usd = ((direction * (last - entry)) / entry) * size * leverage
  return { pct, usd }
}

// The value a row sorts by for a given column. Returned as number|string so the comparator
// can stay generic; nulls sort last regardless of direction.
function sortValue(p: Position, field: SortField, live: number | null): number | string | null {
  switch (field) {
    case 'id':
      return p.ID
    case 'inst_id':
      return p.InstID
    case 'side':
      return p.Side
    case 'strategy':
      return p.StrategyName || ''
    case 'bar':
      return p.Bar || ''
    case 'entry':
      return Number(p.EntryPx)
    case 'leverage':
      return Number(p.Leverage)
    case 'size':
      return Number(p.Size)
    case 'reason':
      return p.CloseReason ?? ''
    case 'opened_at':
      return new Date(p.OpenedAt).getTime()
    case 'closed_at':
      return p.ClosedAt ? new Date(p.ClosedAt).getTime() : null
    case 'pnl':
      // Closed rows sort on realized PnL; open rows on their live unrealized value, so one
      // click orders the column the user is actually looking at rather than half of it.
      return p.RealizedPnL !== null ? Number(p.RealizedPnL) : live
    default:
      return null
  }
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
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE)

  const showClosedColumns = openFilter !== 'open'
  const showLiveColumns = openFilter !== 'closed'
  const [closingId, setClosingId] = useState<number | null>(null)

  const { data, error } = usePolling(
    () =>
      api.listPositions({
        mode: mode === 'all' ? undefined : mode,
        instId: instId || undefined,
        open: openFilter === 'all' ? undefined : openFilter === 'open',
      }),
    5_000,
    [mode, instId, openFilter],
    wsRefreshCount,
  )

  // CLAUDE.md §11.4/§12: cmd/api pushes a message over WebSocket the moment a paper order
  // opens/closes; rather than that event carrying full position detail, it just triggers an
  // immediate refetch here — usePositionAlerts' existing diff-the-snapshot logic then fires the
  // sound/notification off of that fresher data. The 5s poll above still runs as a fallback/
  // consistency check independent of the socket's connection state.
  usePositionEvents(() => setWsRefreshCount((c) => c + 1), true)

  usePositionAlerts(data, alertsEnabled)

  const livePrices = usePriceStream(showLiveColumns)

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

  // Sorting is done client-side so every column is sortable, including the ones the backend has
  // no ORDER BY for (live PnL, strategy name) and the ones it would need a join to order by.
  const sorted = useMemo(() => {
    const rows = [...(data ?? [])]
    const dir = sortDesc ? -1 : 1
    rows.sort((a, b) => {
      const av = sortValue(a, sortBy, unrealizedPnL(a, livePrices[a.InstID])?.usd ?? null)
      const bv = sortValue(b, sortBy, unrealizedPnL(b, livePrices[b.InstID])?.usd ?? null)
      // Nulls always sink to the bottom, so flipping direction never fills the first page
      // with rows that have no value for the sorted column.
      if (av == null && bv == null) return 0
      if (av == null) return 1
      if (bv == null) return -1
      if (typeof av === 'string' || typeof bv === 'string') {
        return String(av).localeCompare(String(bv)) * dir
      }
      return (av - bv) * dir
    })
    return rows
  }, [data, sortBy, sortDesc, livePrices])

  const total = sorted.length
  const pageCount = Math.max(1, Math.ceil(total / pageSize))
  // Clamp rather than reset: a row closing while the user is on the last page shouldn't bounce
  // them back to page 1, but the page must not point past the end of a shrunken list either.
  const safePage = Math.min(page, pageCount - 1)
  const visible = useMemo(
    () => sorted.slice(safePage * pageSize, safePage * pageSize + pageSize),
    [sorted, safePage, pageSize],
  )

  useEffect(() => {
    if (safePage !== page) setPage(safePage)
  }, [safePage, page])

  // Any change to what is being listed starts again from the first page.
  useEffect(() => {
    setPage(0)
  }, [mode, instId, openFilter, pageSize])

  function toggleSort(field: SortField) {
    if (sortBy === field) {
      setSortDesc((d) => !d)
    } else {
      setSortBy(field)
      setSortDesc(true)
    }
    setPage(0)
  }

  // Manual close (2026-08-31 request): flags the order for PaperTrader to close at the live price
  // on its next tick — cmd/api can't close it directly (a separate process owns the tick stream),
  // so this only requests it, then triggers the same immediate refetch the WebSocket bridge uses
  // rather than waiting for the 5s poll to notice the position disappeared.
  async function closePosition(p: Position) {
    if (!confirm(`Close ${p.InstID} #${p.ID} now at the live price? Reason will be recorded as "manual".`)) return
    setClosingId(p.ID)
    try {
      await api.closePosition(p.ID)
      setWsRefreshCount((c) => c + 1)
    } catch (err) {
      alert(`Failed to request close: ${(err as Error).message}`)
    } finally {
      setClosingId(null)
    }
  }

  const columnCount = 11 + (showLiveColumns ? 2 : 0) + (showClosedColumns ? 2 : 0) + (showLiveColumns ? 1 : 0)

  return (
    <div>
      <PaperTradingStatsBox />
      <PaperTradingConfigBox />

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
              <SortableTh field="id" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                ID
              </SortableTh>
              <SortableTh field="inst_id" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                Instrument
              </SortableTh>
              <th className="th-static">Mode</th>
              <SortableTh field="side" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                Side
              </SortableTh>
              <SortableTh field="strategy" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                Strategy
              </SortableTh>
              <SortableTh field="bar" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                Timeframe
              </SortableTh>
              <SortableTh field="entry" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                Entry
              </SortableTh>
              {/* Current price is meaningless for a finished trade — its outcome is already
                  settled — so the live columns only appear where an open position can exist. */}
              {showLiveColumns && <th className="th-static">Last</th>}
              <th className="th-static">SL / TP</th>
              <SortableTh field="leverage" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                Leverage
              </SortableTh>
              <SortableTh field="size" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                Entry Volume
              </SortableTh>
              <SortableTh field="opened_at" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                Opened
              </SortableTh>
              {/* Closed/Reason carry no information in the open-only view, where they are
                  always "—" and "open" by definition. */}
              {showClosedColumns && (
                <SortableTh field="closed_at" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                  Closed
                </SortableTh>
              )}
              {showClosedColumns && (
                <SortableTh field="reason" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                  Reason
                </SortableTh>
              )}
              <SortableTh field="pnl" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                PnL
              </SortableTh>
              {showLiveColumns && <th className="th-static">Updated</th>}
              {showLiveColumns && <th className="th-static"></th>}
            </tr>
          </thead>
          <tbody>
            {visible.map((p: Position) => {
              const lastPrice = livePrices[p.InstID]
              const live = unrealizedPnL(p, lastPrice)
              const isFork = p.Variant === 'rl_adjusted'
              const wasUpdated = updatedOrderIds.has(p.ID)
              const realized = p.RealizedPnL !== null ? Number(p.RealizedPnL) : null
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
                  {showLiveColumns && <td className="mono">{p.ClosedAt ? '—' : (lastPrice ?? '—')}</td>}
                  <td className="mono text-dim">
                    {p.SLPx ?? '—'} / {p.TPPx ?? '—'}
                  </td>
                  <td className="mono">{p.Leverage}x</td>
                  <td className="mono">${Number(p.Size).toLocaleString()}</td>
                  <td className="mono">{formatDateTime(p.OpenedAt)}</td>
                  {showClosedColumns && <td className="mono">{formatDateTime(p.ClosedAt)}</td>}
                  {showClosedColumns && <td>{closeReasonBadge(p.CloseReason)}</td>}
                  <td className={'mono ' + pnlClass(realized ?? live?.usd ?? null)}>
                    {realized !== null
                      ? formatUsd(realized)
                      : live
                        ? `${live.pct >= 0 ? '+' : '−'}${Math.abs(live.pct).toFixed(2)}% (${formatUsd(live.usd)})`
                        : '—'}
                  </td>
                  {showLiveColumns && (
                    <td>
                      {wasUpdated ? (
                        <span className="badge badge-green">yes</span>
                      ) : (
                        <span className="badge badge-dim">no</span>
                      )}
                    </td>
                  )}
                  {showLiveColumns && (
                    <td>
                      {!p.ClosedAt && p.Mode === 'paper' && (
                        <button
                          onClick={() => closePosition(p)}
                          disabled={closingId === p.ID}
                          title="Close this position now at the live price (close_reason='manual')"
                        >
                          {closingId === p.ID ? 'Closing…' : 'Close'}
                        </button>
                      )}
                    </td>
                  )}
                </tr>
              )
            })}
            {total === 0 && (
              <tr>
                <td colSpan={columnCount} className="text-dim">
                  No positions match this filter.
                </td>
              </tr>
            )}
          </tbody>
        </table>
        </div>
        <Pagination
          page={safePage}
          pageSize={pageSize}
          total={total}
          onPageChange={setPage}
          onPageSizeChange={setPageSize}
        />
      </div>

      {detailPosition && (
        <OrderDetailModal position={detailPosition} onClose={() => setDetailOrderId(null)} />
      )}
    </div>
  )
}
