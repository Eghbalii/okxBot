import { useEffect, useState } from 'react'
import FilterToggle from './FilterToggle'
import Pagination, { DEFAULT_PAGE_SIZE } from './Pagination'
import { api } from '../api/client'
import SortableTh from './SortableTh'
import { formatUsd, pnlClass, tokenSymbol } from '../utils/format'
import type { Instrument, PositionMode, TokenAffordability, TokenStats } from '../api/types'

type SortField = 'token' | 'positions' | 'pnlUsd' | 'pnlPct'

function sortRows(rows: Instrument[], stats: Record<string, TokenStats>, sortBy: SortField, sortDesc: boolean): Instrument[] {
  const sorted = [...rows].sort((a, b) => {
    switch (sortBy) {
      case 'token':
        return a.symbol.localeCompare(b.symbol)
      case 'positions':
        return (stats[a.symbol]?.positionCount ?? 0) - (stats[b.symbol]?.positionCount ?? 0)
      case 'pnlUsd':
        return Number(stats[a.symbol]?.pnlUsd ?? 0) - Number(stats[b.symbol]?.pnlUsd ?? 0)
      case 'pnlPct':
        return Number(stats[a.symbol]?.pnlPct ?? 0) - Number(stats[b.symbol]?.pnlPct ?? 0)
    }
  })
  return sortDesc ? sorted.reverse() : sorted
}

// Per-token active/inactive toggle for paper trading (2026-09-01 request): a disabled token stops
// opening NEW positions (checked in evaluateStrategies via PaperTrader.OpensDisabled) but any of
// its already-open positions keep running to their normal SL/TP/timeout close — ingestion is
// untouched, so re-enabling has no data gap. The checkbox here is "enabled" (checked = trades),
// inverted against the saved disabledInstIds list.
//
// 2026-09-04: gained a sortable stats table (position count + PnL$/PnL% for trades CLOSED, CLAUDE.md)
// above the checkboxes, mirroring the Strategies-kind modal's own table. Originally scoped to the
// last 24h; widened to all-time 2026-09-22 per operator instruction (matching StrategyKindModal's
// table, which never had a time window). The min-size column, plus the tag explaining WHY a token
// is off (2026-09-08 request).
//
// 2026-09-17: reads the REAL roster (GET /api/instruments, database-backed) instead of the
// config-file allInstIds prop this modal used to take, AND scoped to exchange=okx only — the
// discovery scan also admits MEXC tokens (enabled_paper=true on admission, §53.1), but paper-trader
// only ever loads the "okx" roster (no MEXC execution wiring yet, §46.6), so a MEXC row's checkbox
// would control nothing. Hiding them here is the honest answer, not a disabled/greyed-out control
// for a decision this panel cannot actually make.
//
// "Active" (2026-09-22 correction) means the per-token enable/disable checkbox state alone
// (instrument.active from the API) — NOT whether a strategy happens to be assigned. A same-day
// first version required BOTH active AND a live assignment, which broke bot/real mode entirely
// (zero assignments there today, so every bot-mode token read active=false regardless of the
// checkbox). The "Trading" column (instrument.hasAssignment) is the separate fact for whether a
// strategy is actually assigned — a token can be active (checkbox on) and idle (no assignment
// yet) at the same time, which is exactly bot mode's current shape. Since the OKX roster is small
// (tens, not hundreds) it is fetched whole and paginated client-side, which is also what lets the
// active/all toggle and sort compose correctly without a second server round trip.
function MinSizeCell({ a }: { a: TokenAffordability | undefined }) {
  if (!a) return <span className="text-dim">—</span>
  if (a.unknown) {
    return (
      <span className="text-dim" title="Instrument or price could not be read, so no judgement was made. Such a token is never auto-disabled.">
        unknown
      </span>
    )
  }
  const min = Number(a.minNotionalUsd)
  const budget = Number(a.budgetUsd)
  return (
    <span title={`Minimum $${min.toFixed(4)} vs. a per-token budget of $${budget.toFixed(2)}`}>
      <span className={a.affordable ? '' : 'text-red'}>${min.toFixed(min < 1 ? 4 : 2)}</span>
      {a.autoDisabled && (
        <span
          className="badge badge-yellow token-auto-tag"
          title="Disabled automatically: the account cannot fund one minimum lot of this instrument once it counts toward the per-token split. It comes back on its own as the account grows."
        >
          auto
        </span>
      )}
    </span>
  )
}

export default function TokenModal({
  mode,
  disabledInstIds,
  onClose,
  onSave,
}: {
  mode: PositionMode
  disabledInstIds: string[]
  onClose: () => void
  onSave: (disabledInstIds: string[]) => Promise<void>
}) {
  const [disabled, setDisabled] = useState<Set<string>>(new Set(disabledInstIds))
  const [rows, setRows] = useState<Instrument[]>([])
  const [rosterLoading, setRosterLoading] = useState(true)
  const [stats, setStats] = useState<Record<string, TokenStats>>({})
  const [statsLoading, setStatsLoading] = useState(true)
  // Per-token affordability: the exchange's smallest acceptable position against the current
  // per-token budget (2026-09-08). Bot mode only — paper has no exchange minimums, so the
  // endpoint returns an empty list there and the column simply stays blank.
  const [afford, setAfford] = useState<Record<string, TokenAffordability>>({})
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [sortBy, setSortBy] = useState<SortField>('token')
  const [sortDesc, setSortDesc] = useState(false)
  // Show only ACTIVE tokens (real assignment, not just enabledPaper — see the module doc comment).
  const [activeOnly, setActiveOnly] = useState(false)
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE)

  useEffect(() => {
    setRosterLoading(true)
    // exchange: 'okx' — the only tokens this modal's checkbox can actually control (see module doc
    // comment). Unpaginated: the OKX roster is small, and fetching it whole is what lets sort and
    // the active/all filter work correctly client-side.
    api
      .instruments({ exchange: 'okx', mode })
      .then(({ items }) => setRows(items))
      .catch((err) => setError((err as Error).message))
      .finally(() => setRosterLoading(false))
  }, [mode])

  useEffect(() => {
    setStatsLoading(true)
    api
      .tokenStatsAllTime(mode)
      .then((rows) => setStats(Object.fromEntries(rows.map((r) => [r.instId, r]))))
      .catch((err) => setError((err as Error).message))
      .finally(() => setStatsLoading(false))
    // Affordability is fetched separately and failures are ignored: it is supporting information,
    // and losing it must not block the token list itself from rendering.
    api
      .tokenAffordability(mode)
      .then((rows) => setAfford(Object.fromEntries(rows.map((r) => [r.instId, r]))))
      .catch(() => setAfford({}))
  }, [mode])

  // Reset to page 0 whenever the filter changes — a page number valid under "all" can be past the
  // end once "active only" shrinks the total.
  useEffect(() => {
    setPage(0)
  }, [activeOnly])

  function toggleSort(field: SortField) {
    if (sortBy === field) {
      setSortDesc((d) => !d)
    } else {
      setSortBy(field)
      setSortDesc(false)
    }
  }

  function toggle(instId: string) {
    setDisabled((prev) => {
      const next = new Set(prev)
      if (next.has(instId)) next.delete(instId)
      else next.add(instId)
      return next
    })
  }

  // Enable all / disable all (2026-09-22 request) — bulk-set every OKX token's checkbox at once.
  // Applies to the FULL roster, not just the currently filtered/paginated view: the "Active only"
  // filter and pagination are display controls (see FilterToggle's own doc comment), and a bulk
  // action that only touched what's on screen would silently leave off-screen rows untouched,
  // which is not what "all" means to someone clicking this button.
  function enableAll() {
    setDisabled(new Set())
  }
  function disableAll() {
    setDisabled(new Set(rows.map((r) => r.symbol)))
  }

  async function save() {
    setSaving(true)
    try {
      await onSave([...disabled].sort())
      onClose()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const filteredRows = activeOnly ? rows.filter((r) => r.active) : rows
  const sortedRows = sortRows(filteredRows, stats, sortBy, sortDesc)
  const total = sortedRows.length
  const pageRows = sortedRows.slice(page * pageSize, (page + 1) * pageSize)
  const activeCount = rows.filter((r) => r.active).length

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 760 }}>
        <div className="modal-header">
          <h2>Manage active tokens</h2>
          <button className="modal-close" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </div>

        <div className="text-dim" style={{ marginBottom: '0.75rem' }}>
          Unchecking a token stops it from opening new {mode} positions. Its existing open
          positions (if any) keep running to their normal close — nothing is force-closed. Only
          OKX tokens are shown: these are the ones this control can actually affect.
        </div>

        {error && <div className="error-banner">{error}</div>}

        {!rosterLoading && rows.length === 0 && <div className="text-dim">No tokens in the roster.</div>}

        {rows.length > 0 && (
          <div className="filter-bar">
            <FilterToggle
              activeOnly={activeOnly}
              onChange={setActiveOnly}
              activeCount={activeCount}
              totalCount={rows.length}
              noun="tokens"
            />
            <div className="bulk-actions">
              <button type="button" onClick={enableAll} disabled={saving}>
                Enable all
              </button>
              <button type="button" onClick={disableAll} disabled={saving}>
                Disable all
              </button>
            </div>
          </div>
        )}

        {(rosterLoading || rows.length > 0) && (
          <div className="table-scroll" style={{ marginBottom: '1rem' }}>
            <table>
              <thead>
                <tr>
                  <SortableTh field="token" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                    Token
                  </SortableTh>
                  <th
                    className="th-static"
                    title="Has a live, enabled strategy assignment right now — distinct from being merely enabled for paper trading, which a token can be without ever actually trading (e.g. a MEXC-only token before MEXC execution is wired in)"
                  >
                    Trading
                  </th>
                  <SortableTh field="positions" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                    Positions
                  </SortableTh>
                  <SortableTh field="pnlUsd" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                    PnL $
                  </SortableTh>
                  <SortableTh field="pnlPct" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                    PnL %
                  </SortableTh>
                  <th className="th-static" title="The exchange's smallest acceptable position for this instrument (contract value x price x min size), against the current per-token budget">
                    Min size
                  </th>
                  <th className="th-static">Active</th>
                </tr>
              </thead>
              <tbody>
                {rosterLoading ? (
                  <tr>
                    <td colSpan={7} className="text-dim">
                      Loading…
                    </td>
                  </tr>
                ) : (
                  pageRows.map((in_) => {
                    const instId = in_.symbol
                    const s = stats[instId]
                    return (
                      <tr key={instId}>
                        <td className="mono" title={instId}>
                          {tokenSymbol(instId)}
                        </td>
                        <td>
                          {in_.hasAssignment ? (
                            <span className="badge badge-green" title="Has a live strategy assignment">
                              trading
                            </span>
                          ) : (
                            <span className="text-dim" title="No strategy is currently assigned to this token, so it cannot open a position regardless of the checkbox below">
                              idle
                            </span>
                          )}
                        </td>
                        <td>{statsLoading ? '…' : (s?.positionCount ?? 0)}</td>
                        <td className={'mono ' + pnlClass(s ? Number(s.pnlUsd) : null)}>
                          {statsLoading ? '…' : formatUsd(Number(s?.pnlUsd ?? 0))}
                        </td>
                        <td className={'mono ' + pnlClass(s ? Number(s.pnlPct) : null)}>
                          {statsLoading ? '…' : `${Number(s?.pnlPct ?? 0).toFixed(2)}%`}
                        </td>
                        <td className="mono">
                          <MinSizeCell a={afford[instId]} />
                        </td>
                        <td>
                          <label className="checkbox-row" style={{ margin: 0 }}>
                            <input
                              type="checkbox"
                              checked={!disabled.has(instId)}
                              onChange={() => toggle(instId)}
                            />
                          </label>
                        </td>
                      </tr>
                    )
                  })
                )}
              </tbody>
            </table>
            {!rosterLoading && pageRows.length === 0 && (
              <div className="text-dim filter-empty">
                No active tokens. Switch to All to enable some.
              </div>
            )}
          </div>
        )}

        {total > 0 && (
          <Pagination
            page={page}
            pageSize={pageSize}
            total={total}
            onPageChange={setPage}
            onPageSizeChange={(size) => {
              setPageSize(size)
              setPage(0)
            }}
          />
        )}

        <div className="toolbar" style={{ marginTop: '1rem' }}>
          <button className="btn-primary" onClick={save} disabled={saving}>
            {saving ? 'Saving…' : 'Save'}
          </button>
          <button onClick={onClose} disabled={saving}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  )
}
