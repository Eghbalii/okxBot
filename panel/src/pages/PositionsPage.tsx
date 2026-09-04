import { useEffect, useRef, useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { usePositionAlerts } from '../hooks/usePositionAlerts'
import { usePositionEvents } from '../hooks/usePositionEvents'
import { usePriceStream } from '../hooks/usePriceStream'
import AdjustPositionForm from '../components/AdjustPositionForm'
import OrderDetailModal from '../components/OrderDetailModal'
import PaperTradingConfigBox from '../components/PaperTradingConfigBox'
import PaperTradingStatsBox from '../components/PaperTradingStatsBox'
import Pagination, { DEFAULT_PAGE_SIZE } from '../components/Pagination'
import SortableTh from '../components/SortableTh'
import { api } from '../api/client'
import { formatDateTimeLines, formatUsd, pnlClass, tokenSymbol, trimPrice } from '../utils/format'
import type { CloseReason, Position, PositionMode } from '../api/types'

// Sortable columns are limited to what Postgres can ORDER BY directly (internal/postgres's
// positionSortColumns) since sorting/paging moved server-side 2026-09-02 — closed positions grew
// into the hundreds and fetching every row to sort/paginate client-side had become a genuinely
// slow query and a multi-MB payload on every 5s poll. Columns like strategy/side/leverage that
// have no backing SQL column are no longer sortable (they'd need a full client-side fetch to sort,
// which is exactly the thing being removed).
type SortField = 'opened_at' | 'closed_at' | 'pnl' | 'inst_id'
type OpenFilter = 'all' | 'open' | 'closed'

function closeReasonBadge(reason: CloseReason | null) {
  if (!reason) return <span className="badge badge-dim">open</span>
  const cls = reason === 'tp' ? 'badge-green' : reason === 'sl' ? 'badge-red' : 'badge-dim'
  return <span className={'badge ' + cls}>{reason}</span>
}

// Date on one line, time on the other — a single "03/09/2026, 05:40:11" line was too wide for the
// Opened/Closed columns and wrapped unpredictably depending on the column's actual rendered width.
function DateTimeCell({ iso }: { iso: string | null | undefined }) {
  const { date, time } = formatDateTimeLines(iso)
  return (
    <td className="mono datetime-cell">
      <div>{date}</div>
      {time && <div>{time}</div>}
    </td>
  )
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

// Leverage-adjusted % move from entry to a target price (SL or TP), same formula unrealizedPnL
// uses against the live price above — for SL this is always <= 0 (a stop realizes a loss) and for
// TP always >= 0 (a target realizes a gain), given a coherent order; shown to the operator as a
// magnitude with its own fixed sign convention (2026-09-04 request: SL/TP columns show % instead
// of price, so the real risk/reward at the position's actual leverage is visible at a glance
// rather than requiring the operator to do the leverage math against a raw price themselves).
function slTpPct(p: Position, target: string | null): number | null {
  if (!target) return null
  const entry = Number(p.EntryPx)
  const t = Number(target)
  const leverage = Number(p.Leverage) || 1
  if (!entry || !Number.isFinite(t)) return null
  const direction = p.Side === 'buy' ? 1 : -1
  return (direction * (t - entry) * 100 * leverage) / entry
}

// Rounds a percent's MAGNITUDE up (2026-09-04 request: "round up so 8.82 shows as 9", clarified as
// ceiling on the absolute value — e.g. -8.82 -> -9, not -8) rather than plain integer rounding,
// which would round 8.82 down to 9 anyway but would round e.g. -8.2 UP toward zero to -8, the
// opposite of "bigger magnitude" for a negative number. No decimal point, no sign, no percent
// suffix — those are added at the call site (a "+"/"−" was explicitly dropped as unnecessary here,
// the color already conveys direction).
function roundPctUp(pct: number): number {
  return Math.sign(pct) * Math.ceil(Math.abs(pct))
}

export default function PositionsPage() {
  const [mode, setMode] = useState<PositionMode | 'all'>('all')
  const [instId, setInstId] = useState('')
  // Defaults to open-only: the panel's job is to show real open positions, not the full historical
  // log — closed trades are still one click away via the filter.
  const [openFilter, setOpenFilter] = useState<OpenFilter>('open')
  // Default sort depends on which view is active: an open-only view has no closed_at to sort by
  // (always NULL), so opened_at is the meaningful default there, while a closed/all view's most
  // useful default is "most recently closed first" — closed_at is also the axis the query itself
  // is now indexed/ordered by server-side (internal/postgres's default when SortBy is unset).
  const [sortBy, setSortByRaw] = useState<SortField>('opened_at')
  const [sortDesc, setSortDesc] = useState(true)
  const [alertsEnabled, setAlertsEnabled] = useState(true)
  const [wsRefreshCount, setWsRefreshCount] = useState(0)
  const [detailOrderId, setDetailOrderId] = useState<number | null>(null)
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE)

  const showClosedColumns = openFilter !== 'open'
  const showLiveColumns = openFilter !== 'closed'
  const [closingId, setClosingId] = useState<number | null>(null)
  // The order id whose Adjust form is currently expanded — at most one open at a time (CLAUDE.md
  // §27's real-trading plan §3b, 2026-09-03).
  const [adjustingId, setAdjustingId] = useState<number | null>(null)

  // Switching into a closed/all view re-defaults the sort to closed_at (only if the user hasn't
  // picked a sort explicitly since — tracked via a ref so this doesn't fight a manual column
  // click). Switching back to open-only re-defaults to opened_at, since closed_at is meaningless
  // (always NULL) there.
  const userPickedSort = useRef(false)
  useEffect(() => {
    if (userPickedSort.current) return
    setSortByRaw(openFilter === 'open' ? 'opened_at' : 'closed_at')
  }, [openFilter, userPickedSort])

  // Server-side sort/page (2026-09-02): closed positions grew into the hundreds, and fetching
  // every row on every 5s poll to sort/paginate client-side had become a genuinely slow query and
  // a multi-MB payload. Any change to filter/sort/page now triggers a fresh, bounded query instead.
  const { data, error } = usePolling(
    () =>
      api.listPositions({
        mode: mode === 'all' ? undefined : mode,
        instId: instId || undefined,
        open: openFilter === 'all' ? undefined : openFilter === 'open',
        sortBy,
        sortDesc,
        page,
        pageSize,
      }),
    5_000,
    [mode, instId, openFilter, sortBy, sortDesc, page, pageSize],
    wsRefreshCount,
  )
  const rows = data?.items ?? null
  const total = data?.total ?? 0

  // CLAUDE.md §11.4/§12: cmd/api pushes a message over WebSocket the moment a paper order
  // opens/closes; rather than that event carrying full position detail, it just triggers an
  // immediate refetch here — usePositionAlerts' existing diff-the-snapshot logic then fires the
  // sound/notification off of that fresher data. The 5s poll above still runs as a fallback/
  // consistency check independent of the socket's connection state.
  usePositionEvents(() => setWsRefreshCount((c) => c + 1), true)

  usePositionAlerts(rows, alertsEnabled)

  const livePrices = usePriceStream(showLiveColumns)

  // Resolved from the current poll's data rather than held in state, so an open modal keeps showing
  // fresh values (live PnL, a close that just landed) instead of a snapshot frozen at click time.
  const detailPosition = detailOrderId === null ? null : (rows?.find((p) => p.ID === detailOrderId) ?? null)

  // A baseline order counts as "updated" if some other row in this same PAGE is a fork of it
  // (CLAUDE.md §15.4/§15.12: the RL controller never edits SL/TP in place, it creates a linked
  // 'rl_adjusted' copy instead) — derived client-side from ParentOrderID. Only sees forks within
  // the current page now that fetching is paginated, which is an accepted trade-off of the same
  // change: this was never a global computation the panel could afford to keep unbounded either.
  const updatedOrderIds = new Set<number>()
  for (const p of rows ?? []) {
    if (p.ParentOrderID != null) updatedOrderIds.add(p.ParentOrderID)
  }

  // Any change to what is being listed starts again from the first page.
  useEffect(() => {
    setPage(0)
  }, [mode, instId, openFilter, pageSize, sortBy, sortDesc])

  function toggleSort(field: SortField) {
    userPickedSort.current = true
    if (sortBy === field) {
      setSortDesc((d) => !d)
    } else {
      setSortByRaw(field)
      setSortDesc(true)
    }
    setPage(0)
  }

  // Manual close (2026-08-31 request): flags the order for PaperTrader to close at the live price
  // on its next tick — cmd/api can't close it directly (a separate process owns the tick stream),
  // so this only requests it, then triggers the same immediate refetch the WebSocket bridge uses
  // rather than waiting for the 5s poll to notice the position disappeared.
  async function closePosition(p: Position) {
    if (!confirm(`Close ${tokenSymbol(p.InstID)} #${p.ID} now at the live price? Reason will be recorded as "manual".`)) return
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

  // Manual SL/TP edit for a real position (CLAUDE.md §27's real-trading plan §3b, 2026-09-03) —
  // purely local on the backend (no exchange call), so the new levels are visible on the very next
  // poll rather than needing the WebSocket refresh close() uses.
  async function submitAdjust(id: number, slPct: string, tpPct: string) {
    const body: { slPct?: number; tpPct?: number } = {}
    if (slPct.trim() !== '') body.slPct = Number(slPct)
    if (tpPct.trim() !== '') body.tpPct = Number(tpPct)
    if (body.slPct === undefined && body.tpPct === undefined) {
      alert('Enter at least one of SL% or TP%.')
      return
    }
    try {
      await api.adjustPosition(id, body)
      setAdjustingId(null)
    } catch (err) {
      alert(`Failed to adjust position: ${(err as Error).message}`)
    }
  }

  // 12 always-shown columns (ID, Inst, Side, Strategy, TF, Entry, SL/TP, Leverage, Vol, Opened,
  // PnL, Max) plus showLiveColumns' 3 (Last, Updated, the close-button column) and
  // showClosedColumns' 2 (Closed, Reason).
  const columnCount = 12 + (showLiveColumns ? 3 : 0) + (showClosedColumns ? 2 : 0)

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
              {/* ID/side/strategy/bar/entry/leverage/size have no backing SQL column to ORDER BY
                  server-side, so they're no longer sortable (CLAUDE.md §11.4's pagination-moved-
                  server-side change, 2026-09-02) — sorting them would require fetching every row
                  again, exactly what moving pagination server-side was meant to stop. */}
              <th className="th-static">ID</th>
              <SortableTh field="inst_id" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                Inst
              </SortableTh>
              <th className="th-static">Side</th>
              <th className="th-static">Strategy</th>
              <th className="th-static">TF</th>
              <th className="th-static">Entry</th>
              {/* Current price is meaningless for a finished trade — its outcome is already
                  settled — so the live columns only appear where an open position can exist. */}
              {showLiveColumns && <th className="th-static">Last</th>}
              <th className="th-static">SL / TP</th>
              <th className="th-static">Leverage</th>
              <th className="th-static">Vol</th>
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
              {showClosedColumns && <th className="th-static">Reason</th>}
              <SortableTh field="pnl" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                PnL
              </SortableTh>
              {/* Peak/trough unrealized PnL this position reached while open (CLAUDE.md §15.11) —
                  unlike Last/Updated/the close button below, this is meaningful for closed rows
                  too (it's exactly what surfaces "this ran to +25% and still closed negative"
                  without opening the order-detail modal), so it isn't gated on showLiveColumns. */}
              <th className="th-static">Max</th>
              {showLiveColumns && <th className="th-static">Updated</th>}
              {showLiveColumns && <th className="th-static"></th>}
            </tr>
          </thead>
          <tbody>
            {(rows ?? []).map((p: Position) => {
              const lastPrice = livePrices[p.InstID]
              const live = unrealizedPnL(p, lastPrice)
              const isFork = p.Variant === 'rl_adjusted'
              const wasUpdated = updatedOrderIds.has(p.ID)
              const realized = p.RealizedPnL !== null ? Number(p.RealizedPnL) : null
              const slPct = slTpPct(p, p.SLPx)
              const tpPct = slTpPct(p, p.TPPx)
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
                  <td title={p.InstID}>
                    {tokenSymbol(p.InstID)}
                    {isFork && (
                      <span className="badge badge-dim" style={{ marginLeft: '0.4rem' }} title="RL shadow-fork: tracking-only copy comparing an adjusted SL/TP against its baseline parent">
                        fork
                      </span>
                    )}
                  </td>
                  <td>
                    <span className={'badge ' + (p.Side === 'buy' ? 'badge-green' : 'badge-red')}>
                      {p.Side === 'buy' ? 'long' : 'short'}
                    </span>
                  </td>
                  <td>{p.StrategyName || '—'}</td>
                  <td className="mono text-dim">{p.Bar || '—'}</td>
                  <td className="mono">{trimPrice(p.EntryPx)}</td>
                  {showLiveColumns && <td className="mono">{p.ClosedAt ? '—' : trimPrice(lastPrice)}</td>}
                  <td className="mono sl-tp-cell">
                    <div>
                      {tpPct !== null && <span className="text-green">{roundPctUp(tpPct)}%</span>}
                      {tpPct !== null && ' | '}
                      <span className="text-dim">{trimPrice(p.TPPx)}</span>
                    </div>
                    <div>
                      {slPct !== null && <span className="text-red">{roundPctUp(slPct)}%</span>}
                      {slPct !== null && ' | '}
                      <span className="text-dim">{trimPrice(p.SLPx)}</span>
                    </div>
                  </td>
                  <td className="mono">{p.Leverage}x</td>
                  <td className="mono">${trimPrice(p.Size)}</td>
                  <DateTimeCell iso={p.OpenedAt} />
                  {showClosedColumns && <DateTimeCell iso={p.ClosedAt} />}
                  {showClosedColumns && <td>{closeReasonBadge(p.CloseReason)}</td>}
                  <td className={'mono pnl-cell ' + pnlClass(realized ?? live?.usd ?? null)}>
                    {realized !== null ? (
                      <div>{formatUsd(realized)}</div>
                    ) : live ? (
                      <>
                        <div>{`${live.pct >= 0 ? '+' : '−'}${Math.abs(live.pct).toFixed(2)}%`}</div>
                        <div>{formatUsd(live.usd)}</div>
                      </>
                    ) : (
                      '—'
                    )}
                  </td>
                  <td className="mono pnl-cell">
                    <div className={pnlClass(Number(p.PnLMaxPct))}>
                      {`${Number(p.PnLMaxPct) >= 0 ? '+' : ''}${(Number(p.PnLMaxPct) * 100).toFixed(2)}%`}
                    </div>
                    <div className={pnlClass(Number(p.PnLMinPct))}>
                      {`${Number(p.PnLMinPct) >= 0 ? '+' : ''}${(Number(p.PnLMinPct) * 100).toFixed(2)}%`}
                    </div>
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
                      {/* Real-trading-only, unlike Close above: paper positions have no exchange
                          leg to adjust and are edited through the model only (CLAUDE.md §27's
                          real-trading plan §3b). */}
                      {!p.ClosedAt && p.Mode === 'real' && (
                        <button
                          onClick={() => setAdjustingId(adjustingId === p.ID ? null : p.ID)}
                          title="Manually move this position's SL/TP (no exchange call, unclamped)"
                        >
                          Adjust
                        </button>
                      )}
                    </td>
                  )}
                </tr>
              )
            })}
            {adjustingId !== null &&
              (() => {
                const p = rows?.find((x) => x.ID === adjustingId)
                if (!p) return null
                return (
                  <tr key={`adjust-${p.ID}`}>
                    <td colSpan={columnCount}>
                      <AdjustPositionForm
                        position={p}
                        onCancel={() => setAdjustingId(null)}
                        onSubmit={(slPct, tpPct) => submitAdjust(p.ID, slPct, tpPct)}
                      />
                    </td>
                  </tr>
                )
              })()}
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
          page={page}
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
