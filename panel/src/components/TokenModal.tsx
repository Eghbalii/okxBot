import { useEffect, useMemo, useState } from 'react'
import FilterToggle from './FilterToggle'
import { api } from '../api/client'
import SortableTh from './SortableTh'
import { formatUsd, pnlClass, tokenSymbol } from '../utils/format'
import type { PositionMode, TokenAffordability, TokenStats } from '../api/types'

type SortField = 'token' | 'positions' | 'pnlUsd' | 'pnlPct'

function sortRows(instIds: string[], stats: Record<string, TokenStats>, sortBy: SortField, sortDesc: boolean): string[] {
  const sorted = [...instIds].sort((a, b) => {
    switch (sortBy) {
      case 'token':
        return a.localeCompare(b)
      case 'positions':
        return (stats[a]?.positionCount ?? 0) - (stats[b]?.positionCount ?? 0)
      case 'pnlUsd':
        return Number(stats[a]?.pnlUsd ?? 0) - Number(stats[b]?.pnlUsd ?? 0)
      case 'pnlPct':
        return Number(stats[a]?.pnlPct ?? 0) - Number(stats[b]?.pnlPct ?? 0)
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
// 2026-09-04: gained a sortable 24h stats table (position count + PnL$/PnL% for trades CLOSED in
// the last 24h, CLAUDE.md) above the checkboxes, mirroring the Strategies-kind modal's own table.
// The min-size column, plus the tag explaining WHY a token is off (2026-09-08 request).
//
// The distinction the tag draws is the useful one: a token that is disabled AND cannot afford one
// lot was taken offline by the affordability service, and will come back on its own once the
// account grows. A token disabled while perfectly affordable was turned off by a person, and will
// stay off until a person turns it back on. Without the tag those two look identical.
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
  allInstIds,
  disabledInstIds,
  onClose,
  onSave,
}: {
  mode: PositionMode
  allInstIds: string[]
  disabledInstIds: string[]
  onClose: () => void
  onSave: (disabledInstIds: string[]) => Promise<void>
}) {
  const [disabled, setDisabled] = useState<Set<string>>(new Set(disabledInstIds))
  const [stats, setStats] = useState<Record<string, TokenStats>>({})
  const [statsLoading, setStatsLoading] = useState(true)
  // Per-token affordability: the exchange's smallest acceptable position against the current
  // per-token budget (2026-09-08). Real mode only — paper has no exchange minimums, so the
  // endpoint returns an empty list there and the column simply stays blank.
  const [afford, setAfford] = useState<Record<string, TokenAffordability>>({})
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [sortBy, setSortBy] = useState<SortField>('token')
  const [sortDesc, setSortDesc] = useState(false)
  // Show only active tokens (2026-09-12 request). Note the inverted state here: this modal tracks
  // DISABLED tokens, so "active" is everything NOT in that set.
  const [activeOnly, setActiveOnly] = useState(false)

  useEffect(() => {
    setStatsLoading(true)
    api
      .tokenStats24h(mode)
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

  const sortedInstIds = useMemo(
    () => sortRows(allInstIds, stats, sortBy, sortDesc),
    [allInstIds, stats, sortBy, sortDesc],
  )
  // Filters the view only — a hidden row keeps its checked state and is still saved.
  const visibleInstIds = useMemo(
    () => (activeOnly ? sortedInstIds.filter((id) => !disabled.has(id)) : sortedInstIds),
    [sortedInstIds, activeOnly, disabled],
  )

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
          positions (if any) keep running to their normal close — nothing is force-closed.
        </div>

        {error && <div className="error-banner">{error}</div>}

        {allInstIds.length === 0 && <div className="text-dim">No tokens configured.</div>}

        {allInstIds.length > 0 && (
          <div className="filter-bar">
            <FilterToggle
              activeOnly={activeOnly}
              onChange={setActiveOnly}
              activeCount={allInstIds.length - disabled.size}
              totalCount={allInstIds.length}
              noun="tokens"
            />
          </div>
        )}

        {allInstIds.length > 0 && (
          <div className="table-scroll" style={{ marginBottom: '1rem' }}>
            <table>
              <thead>
                <tr>
                  <SortableTh field="token" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                    Token
                  </SortableTh>
                  <SortableTh field="positions" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                    Positions (24h)
                  </SortableTh>
                  <SortableTh field="pnlUsd" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                    PnL $ (24h)
                  </SortableTh>
                  <SortableTh field="pnlPct" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                    PnL % (24h)
                  </SortableTh>
                  <th className="th-static" title="The exchange's smallest acceptable position for this instrument (contract value x price x min size), against the current per-token budget">
                    Min size
                  </th>
                  <th className="th-static">Active</th>
                </tr>
              </thead>
              <tbody>
                {visibleInstIds.map((instId) => {
                  const s = stats[instId]
                  return (
                    <tr key={instId}>
                      <td className="mono" title={instId}>
                        {tokenSymbol(instId)}
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
                })}
              </tbody>
            </table>
            {visibleInstIds.length === 0 && (
              <div className="text-dim filter-empty">
                No active tokens. Switch to All to enable some.
              </div>
            )}
          </div>
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
