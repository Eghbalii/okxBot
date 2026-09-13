import { useEffect, useRef, useState } from 'react'
import { Navigate, useParams } from 'react-router-dom'
import { useCachedResource } from '../hooks/useCachedResource'
import { usePositionAlerts } from '../hooks/usePositionAlerts'
import { usePositionEvents } from '../hooks/usePositionEvents'
import { usePriceStream } from '../hooks/usePriceStream'
import { unrealizedPnL } from '../lib/pnl'
import AdjustPositionForm from '../components/AdjustPositionForm'
import OrderDetailModal from '../components/OrderDetailModal'
import TokenChartModal from '../components/TokenChartModal'
import PaperTradingConfigBox from '../components/PaperTradingConfigBox'
import PaperTradingStatsBox from '../components/PaperTradingStatsBox'
import Pagination, { DEFAULT_PAGE_SIZE } from '../components/Pagination'
import SortableTh from '../components/SortableTh'
import { api } from '../api/client'
import { formatDateTimeLines, formatUsd, pnlClass, tokenSymbol, trimPrice } from '../utils/format'
import type { CloseReason, OrderStatus, Position, PositionMode } from '../api/types'

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

// Real-order fill-lifecycle status badge (CLAUDE.md real-trading readiness plan, 2026-09-04) —
// shown only for real positions, where "pending"/"partial" carry real product meaning (an order
// still in flight or a fill smaller than requested); "filled" is the unremarkable default so it's
// dimmed rather than colored, and "canceled" reads distinctly from a normal closed/SL/TP row since
// it never became a position at all.
// 'opening'/'closing' are in-flight states: a request is with the exchange and its outcome is not
// yet known. They render as the attention-getting yellow rather than the neutral dim, because a row
// that stays in one is exactly what a trader needs to notice — a stuck close means the position may
// still be live on the exchange (2026-09-08).
function statusBadge(status: OrderStatus | null) {
  if (!status || status === 'filled') return <span className="badge badge-dim">filled</span>
  const cls = status === 'canceled' ? 'badge-red' : 'badge-yellow'
  return <span className={'badge ' + cls} title={statusHelp[status] ?? status}>{status}</span>
}

const statusHelp: Record<string, string> = {
  pending: 'Order accepted by the exchange; fill not yet confirmed.',
  opening: 'Open order is in flight with the exchange, waiting on confirmation.',
  partial: 'Partially filled — a real, smaller-than-intended position exists.',
  closing: 'Flattening order sent, waiting on the exchange to confirm. The position is still open until it does.',
  canceled: 'Never filled before the fill timeout — no position was opened.',
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

// feesDisplay renders the trading fee + funding cost/credit already subtracted into a closed
// position's PnL (2026-09-06). Kept as one combined total for the table's compact column — the
// order-detail modal is where a reader who wants the fee/funding split separately can see it — but
// both are null-safe independently since a row closed before funding tracking existed carries a
// fee with no funding value.
function feesDisplay(feesUSD: string | null, fundingUSD: string | null): string {
  if (feesUSD === null && fundingUSD === null) return '—'
  const total = (feesUSD !== null ? Number(feesUSD) : 0) + (fundingUSD !== null ? Number(fundingUSD) : 0)
  return formatUsd(-total) // stored as a cost (positive = charged); display as its effect on PnL
}

// SL/TP percentages are shown to one decimal place, unrounded (2026-09-12 request, replacing an
// earlier magnitude-ceiling that displayed 8.82 as 9). Truncating rather than rounding the last
// digit keeps a stop placed exactly on the 15% loss cap reading as 15.0 — Number.toFixed would
// render the float64 representation -15.000000000000037 as "-15.0" anyway, but truncation makes
// that independent of representation error rather than incidentally correct.
function formatPct1(pct: number): string {
  const truncated = Math.trunc(pct * 10) / 10
  return truncated.toFixed(1)
}

export default function PositionsPage() {
  const { mode: rawMode } = useParams<{ mode: string }>()
  // Only 'paper'/'real' are valid route segments — an unrecognized value (a stale bookmark, a typo)
  // redirects to paper rather than silently misinterpreting it.
  if (rawMode !== 'paper' && rawMode !== 'real') {
    return <Navigate to="/positions/paper" replace />
  }
  const mode: PositionMode = rawMode

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
  const [chartInstId, setChartInstId] = useState<string | null>(null)
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
  //
  // Cached per query (2026-09-13): switching between the Paper and Real tabs is a ROUTE change, so
  // this whole page unmounts and `rows` restarts at null — and until it refills, the chart has no
  // positions and cannot draw its green/red zones. That is the reported "the highlight takes a
  // while to appear". These are the panel's largest payloads (measured: 210KB paper, 133KB real),
  // so re-fetching and re-parsing them on every tab switch is the expensive part, not the ~50ms the
  // server spends.
  //
  // wsRefreshCount is part of the KEY rather than a dependency that forces a refetch, so an
  // order opening or closing moves to a fresh entry while the previous one stays available.
  const queryKey = `positions:${mode}:${instId}:${openFilter}:${sortBy}:${sortDesc}:${page}:${pageSize}:${wsRefreshCount}`
  const { data, error } = useCachedResource(
    queryKey,
    () =>
      api.listPositions({
        mode,
        instId: instId || undefined,
        open: openFilter === 'all' ? undefined : openFilter === 'open',
        sortBy,
        sortDesc,
        page,
        pageSize,
      }),
    // Revalidates on the same 5s cadence as before, so live PnL stays as fresh as it was; the
    // difference is that a cached page is shown immediately while that happens instead of a blank.
    { maxAgeMs: 4_000, refetchMs: 5_000 },
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

  // Also enabled while a chart is open: the chart's header strip shows live PnL for every open
  // position regardless of how the table itself is filtered, so a chart opened from the closed-only
  // view would otherwise show "—" for all of them.
  const livePrices = usePriceStream(showLiveColumns || chartInstId !== null)

  // Resolved from the current poll's data rather than held in state, so an open modal keeps showing
  // fresh values (live PnL, a close that just landed) instead of a snapshot frozen at click time.
  const detailPosition = detailOrderId === null ? null : (rows?.find((p) => p.ID === detailOrderId) ?? null)
  const adjustingPosition = adjustingId === null ? null : (rows?.find((p) => p.ID === adjustingId) ?? null)

  // A baseline order counts as "updated" if some other row in this same PAGE is a fork of it
  // (CLAUDE.md §15.4/§15.12: the RL controller never edits SL/TP in place, it creates a linked
  // 'rl_adjusted' copy instead) — derived client-side from ParentOrderID. Only sees forks within
  // the current page now that fetching is paginated, which is an accepted trade-off of the same
  // change: this was never a global computation the panel could afford to keep unbounded either.
  // Superseded 2026-09-06: this used to mark a row "updated" when some OTHER row was a shadow fork
  // of it (ParentOrderID). Forking was replaced by in-place SL/TP edits on 2026-09-02 (CLAUDE.md
  // §15.4 revision), so no fork has been created since and the column was permanently blank —
  // measured on live data: 0 forks against 338 real adjustments across 37 orders. AdjustmentCount
  // (served by ListPositions/ListRealPositions) is the live equivalent. The fork set is still built
  // so historical rows from before the change keep rendering as updated.
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
      await api.closePosition(p.ID, p.Mode)
      setWsRefreshCount((c) => c + 1)
    } catch (err) {
      alert(`Failed to request close: ${(err as Error).message}`)
    } finally {
      setClosingId(null)
    }
  }

  // Manual SL/TP edit for a real position (CLAUDE.md §27's real-trading plan §3b, §35 for the
  // exchange leg). The backend amends the position's resting SL/TP order on OKX BEFORE writing the
  // new levels locally (2026-09-09), so a rejection leaves both sides at the old level and this
  // surfaces as a failed edit rather than a level the panel shows and the exchange never received.
  // Errors keep the modal open so the operator can retry or correct the value.
  async function submitAdjust(id: number, slPct: string, tpPct: string) {
    const body: { slPct?: number; tpPct?: number } = {}
    if (slPct.trim() !== '') body.slPct = Number(slPct)
    if (tpPct.trim() !== '') body.tpPct = Number(tpPct)
    if (body.slPct === undefined && body.tpPct === undefined) {
      alert('Enter at least one of SL% or TP%.')
      return
    }
    try {
      await api.adjustPosition(id, 'real', body)
      setAdjustingId(null)
    } catch (err) {
      alert(`Failed to adjust position: ${(err as Error).message}`)
    }
  }

  // 12 always-shown columns (ID, Inst, Side, Strategy, TF, Entry, SL/TP, Leverage, Vol, Opened,
  // PnL, Max) plus showLiveColumns' 3 (Last, Updated, the close-button column), showClosedColumns'
  // 3 (Closed, Reason, Fees — the last added 2026-09-06), and the real-only Status column.
  const columnCount = 12 + (showLiveColumns ? 3 : 0) + (showClosedColumns ? 3 : 0) + (mode === 'real' ? 1 : 0)

  return (
    <div>
      {/* The Paper/Real selector moved into the header (2026-09-12 request) — it is a mode the
          whole panel operates in, not a control belonging to this table. */}
      <PaperTradingStatsBox mode={mode} />
      <PaperTradingConfigBox mode={mode} />

      <div className="toolbar">
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
              {/* Fill-lifecycle status only carries meaning for real orders (a paper order is
                  always instantly and fully filled). */}
              {mode === 'real' && <th className="th-static">Status</th>}
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
              {/* Trading fee + funding cost/credit already subtracted into PnL (2026-09-06) — only
                  meaningful once a position has actually closed and those costs are known. */}
              {showClosedColumns && <th className="th-static">Fees</th>}
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
              // Either a real in-place edit (current behaviour) or a historical shadow fork.
              const wasUpdated = p.AdjustmentCount > 0 || updatedOrderIds.has(p.ID)
              // The EXCHANGE's own realized PnL wins wherever reported (2026-09-08): a locally
              // computed figure cannot see fees, funding, or the true fill price, so the two can
              // silently disagree with what the account actually moved by. null (not zero) is what
              // "the exchange did not report it" looks like, and only then do we fall back.
              const realized =
                p.ExchangeRealizedPnL !== null
                  ? Number(p.ExchangeRealizedPnL)
                  : p.RealizedPnL !== null
                    ? Number(p.RealizedPnL)
                    : null
              const realizedFromExchange = p.ExchangeRealizedPnL !== null
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
                  <td title={`${p.InstID} — click for the chart`}>
                    <button className="order-id-button" onClick={() => setChartInstId(p.InstID)}>
                      {tokenSymbol(p.InstID)}
                    </button>
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
                  {mode === 'real' && <td>{statusBadge(p.Status)}</td>}
                  <td>{p.StrategyName || '—'}</td>
                  <td className="mono text-dim">{p.Bar || '—'}</td>
                  <td className="mono">{trimPrice(p.EntryPx)}</td>
                  {showLiveColumns && <td className="mono">{p.ClosedAt ? '—' : trimPrice(lastPrice)}</td>}
                  <td className="mono sl-tp-cell">
                    <div>
                      {tpPct !== null && <span className="text-green">{formatPct1(tpPct)}%</span>}
                      {tpPct !== null && ' | '}
                      <span className="text-dim">{trimPrice(p.TPPx)}</span>
                    </div>
                    <div>
                      {slPct !== null && <span className="text-red">{formatPct1(slPct)}%</span>}
                      {slPct !== null && ' | '}
                      <span className="text-dim">{trimPrice(p.SLPx)}</span>
                    </div>
                  </td>
                  <td className="mono">{Number(p.Leverage).toFixed(1)}x</td>
                  <td className="mono">${Number(p.Size).toFixed(1)}</td>
                  <DateTimeCell iso={p.OpenedAt} />
                  {showClosedColumns && <DateTimeCell iso={p.ClosedAt} />}
                  {showClosedColumns && <td>{closeReasonBadge(p.CloseReason)}</td>}
                  <td className={'mono pnl-cell ' + pnlClass(realized ?? live?.usd ?? null)}>
                    {realized !== null ? (
                      <div
                        title={
                          realizedFromExchange
                            ? "The exchange's own reported realized PnL"
                            : 'Computed locally — the exchange did not report a figure for this order'
                        }
                      >
                        {formatUsd(realized)}
                        {!realizedFromExchange && p.Mode === 'real' && <span className="pnl-local-marker">*</span>}
                      </div>
                    ) : live ? (
                      <>
                        <div>{`${live.pct >= 0 ? '+' : '−'}${Math.abs(live.pct).toFixed(2)}%`}</div>
                        <div>{formatUsd(live.usd)}</div>
                      </>
                    ) : (
                      '—'
                    )}
                  </td>
                  {showClosedColumns && (
                    <td
                      className="mono text-dim"
                      title={
                        p.ExchangeFee !== null
                          ? "The exchange's own reported fee for this order"
                          : 'Trading fee + funding cost/credit already subtracted into PnL'
                      }
                    >
                      {p.ExchangeFee !== null ? formatUsd(Number(p.ExchangeFee)) : feesDisplay(p.FeesUSD, p.FundingUSD)}
                    </td>
                  )}
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
                        <span
                          className="badge badge-green"
                          title={`${p.AdjustmentCount} in-place SL/TP edit${p.AdjustmentCount === 1 ? '' : 's'}`}
                        >
                          {p.AdjustmentCount}
                        </span>
                      ) : (
                        <span className="badge badge-dim">no</span>
                      )}
                    </td>
                  )}
                  {showLiveColumns && (
                    <td className="actions-cell">
                      {/* Close works in BOTH modes. It was gated to paper until 2026-09-08, which
                          left real positions with no way to exit from the panel at all — the
                          backend half (RequestRealManualClose, and RealTrader's own
                          ManualCloseRequested check on every tick) had been in place the whole
                          time, only the button was missing. */}
                      {!p.ClosedAt && (
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
                          onClick={() => setAdjustingId(p.ID)}
                          title="Manually move this position's SL/TP (no exchange call, unclamped)"
                        >
                          Update
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

      {/* Positions come from the rows already loaded rather than a second fetch: the chart shows
          exactly the orders the table shows, and cannot disagree with what was just clicked. */}
      {chartInstId && (
        <TokenChartModal
          instId={chartInstId}
          mode={mode}
          positions={rows ?? []}
          livePrices={livePrices}
          onClose={() => setChartInstId(null)}
        />
      )}

      {/* Mounted here, outside the table, rather than as an expanded row beneath the order
          (2026-09-08 request): the expanded row pushed the rest of the table down, and with a 5s
          refetch running the controls could shift under the pointer mid-edit. Resolved from `rows`
          by id so a refetch that reorders or re-pages the table cannot leave the dialog showing a
          different order than the one that was clicked — it closes instead. */}
      {adjustingPosition && (
        <AdjustPositionForm
          position={adjustingPosition}
          onCancel={() => setAdjustingId(null)}
          onSubmit={(slPct, tpPct) => submitAdjust(adjustingPosition.ID, slPct, tpPct)}
        />
      )}
    </div>
  )
}
