import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import SortableTh from './SortableTh'
import { formatUsd, pnlClass, winRate } from '../utils/format'
import type { StrategyConfig, StrategyStats } from '../api/types'

// The 12 scalp/ICT/price-action kinds added CLAUDE.md §30, tagged "new" in this list so they are
// easy to spot among the original 14 while deciding which to enable — pure display, no backend
// concept of "new" exists (a strategy row carries no added_at/version marker for this).
const NEW_KINDS = new Set([
  'vwap_reversion',
  'bb_squeeze_breakout',
  'range_breakout',
  'keltner_trend_scalp',
  'ict_fvg',
  'ict_order_block',
  'ict_liquidity_sweep',
  'engulfing_reversal',
  'inside_bar_breakout',
  'macd_momentum',
  'volume_breakout',
  'ema_ribbon_pullback',
])

type SortField = 'name' | 'signals' | 'winRate' | 'pnl'

// One row per KIND (not per strategy id): the modal's checkboxes toggle a kind globally across
// every token, so its stats table aggregates every strategy row sharing that kind — origin plus
// any sub-strategies cloned from it — rather than showing one line per sub-strategy the way the
// Strategies page itself does.
interface KindRow {
  kind: string
  signalCount: number
  wins: number
  losses: number
  realizedPnL: number
}

function buildKindRows(strategies: StrategyConfig[], stats: Record<number, StrategyStats>): KindRow[] {
  const byKind = new Map<string, KindRow>()
  for (const s of strategies) {
    const row = byKind.get(s.Kind) ?? { kind: s.Kind, signalCount: 0, wins: 0, losses: 0, realizedPnL: 0 }
    const st = stats[s.ID]
    if (st) {
      row.signalCount += st.SignalCount
      row.wins += st.Wins
      row.losses += st.Losses
      row.realizedPnL += Number(st.RealizedPnL)
    }
    byKind.set(s.Kind, row)
  }
  return [...byKind.values()]
}

function sortRows(rows: KindRow[], sortBy: SortField, sortDesc: boolean): KindRow[] {
  const sorted = [...rows].sort((a, b) => {
    switch (sortBy) {
      case 'name':
        return a.kind.localeCompare(b.kind)
      case 'signals':
        return a.signalCount - b.signalCount
      case 'winRate': {
        const aDecided = a.wins + a.losses
        const bDecided = b.wins + b.losses
        const aRate = aDecided === 0 ? -1 : a.wins / aDecided
        const bRate = bDecided === 0 ? -1 : b.wins / bDecided
        return aRate - bRate
      }
      case 'pnl':
        return a.realizedPnL - b.realizedPnL
    }
  })
  return sortDesc ? sorted.reverse() : sorted
}

// Global per-strategy-KIND on/off switch for paper trading: applied uniformly across every token,
// bulk-toggling strategy_assignments.enabled for every assignment of that kind — distinct from the
// existing per-token/per-timeframe assignment granularity on the Strategies page, which stays the
// underlying mechanism this toggle drives. Disabling a kind only stops it from opening NEW
// positions; any of its already-open positions keep running to their normal SL/TP/timeout close.
//
// Checkbox semantics: the saved activeKinds list is empty by default, meaning "no restriction —
// every kind currently enabled in strategy_assignments applies as-is." An empty list must render
// as every kind CHECKED (they're all effectively active), not as every box empty — the modal
// previously got this backwards, which read as "everything is disabled" when the opposite was
// true. Unchecking is what actually creates a restriction; checking everything back is equivalent
// to clearing it, handled below by treating "all checked" as saving an empty list.
export default function StrategyKindModal({
  activeKinds,
  onClose,
  onSave,
}: {
  activeKinds: string[]
  onClose: () => void
  onSave: (kinds: string[]) => Promise<void>
}) {
  const [strategies, setStrategies] = useState<StrategyConfig[]>([])
  const [stats, setStats] = useState<Record<number, StrategyStats>>({})
  const [selected, setSelected] = useState<Set<string>>(new Set(activeKinds))
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [sortBy, setSortBy] = useState<SortField>('name')
  const [sortDesc, setSortDesc] = useState(false)

  useEffect(() => {
    api
      .listStrategies()
      .then(async (rows) => {
        setStrategies(rows)
        const kinds = [...new Set(rows.map((s) => s.Kind))].sort()
        // No saved restriction (activeKinds empty) means every kind is effectively active —
        // pre-check all of them rather than leaving the list looking fully disabled.
        setSelected((prev) => (activeKinds.length === 0 ? new Set(kinds) : prev))
        const entries = await Promise.all(
          rows.map(async (s) => [s.ID, await api.strategyStats(s.ID)] as const),
        )
        setStats(Object.fromEntries(entries))
      })
      .catch((err) => setError((err as Error).message))
      .finally(() => setLoading(false))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  function toggleSort(field: SortField) {
    if (sortBy === field) {
      setSortDesc((d) => !d)
    } else {
      setSortBy(field)
      setSortDesc(false)
    }
  }

  function toggle(kind: string) {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(kind)) next.delete(kind)
      else next.add(kind)
      return next
    })
  }

  async function save() {
    setSaving(true)
    try {
      // Every kind checked is equivalent to "no restriction" — save an empty list rather than an
      // explicit list of all 14, so a future 15th kind isn't silently excluded by a stale snapshot.
      const allKindsList = [...new Set(strategies.map((s) => s.Kind))]
      const allChecked = allKindsList.length > 0 && allKindsList.every((k) => selected.has(k))
      await onSave(allChecked ? [] : [...selected].sort())
      onClose()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const kindRows = useMemo(() => buildKindRows(strategies, stats), [strategies, stats])
  const sortedRows = useMemo(() => sortRows(kindRows, sortBy, sortDesc), [kindRows, sortBy, sortDesc])

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 900 }}>
        <div className="modal-header">
          <h2>Manage active strategies</h2>
          <button className="modal-close" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </div>

        <div className="text-dim" style={{ marginBottom: '0.75rem' }}>
          Only checked strategy kinds open new paper positions, across every token. Unchecking a
          kind stops it from opening new positions — any of its existing open positions keep
          running to their normal close, nothing is force-closed.
        </div>

        {error && <div className="error-banner">{error}</div>}
        {loading && <div className="text-dim">Loading…</div>}

        {!loading && (
          <>
            <div className="table-scroll" style={{ marginBottom: '1rem' }}>
              <table>
                <thead>
                  <tr>
                    <SortableTh field="name" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                      Kind
                    </SortableTh>
                    <SortableTh field="signals" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                      Signals
                    </SortableTh>
                    <SortableTh field="winRate" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                      Win rate
                    </SortableTh>
                    <SortableTh field="pnl" sortBy={sortBy} sortDesc={sortDesc} onSort={toggleSort}>
                      Realized PnL
                    </SortableTh>
                    <th className="th-static">Active</th>
                  </tr>
                </thead>
                <tbody>
                  {sortedRows.map((row) => (
                    <tr key={row.kind}>
                      <td>
                        <span className="mono">{row.kind}</span>
                        {NEW_KINDS.has(row.kind) && (
                          <span className="badge badge-green" style={{ marginLeft: 6 }}>
                            new
                          </span>
                        )}
                      </td>
                      <td>{row.signalCount}</td>
                      <td>{winRate(row.wins, row.losses)}</td>
                      <td className={'mono ' + pnlClass(row.realizedPnL)}>{formatUsd(row.realizedPnL)}</td>
                      <td>
                        <label className="checkbox-row" style={{ margin: 0 }}>
                          <input
                            type="checkbox"
                            checked={selected.has(row.kind)}
                            onChange={() => toggle(row.kind)}
                          />
                        </label>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </>
        )}

        <div className="toolbar" style={{ marginTop: '1rem' }}>
          <button className="btn-primary" onClick={save} disabled={saving || loading}>
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
