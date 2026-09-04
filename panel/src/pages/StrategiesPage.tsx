import { useEffect, useState } from 'react'
import { api } from '../api/client'
import ParamChangeChart from '../components/ParamChangeChart'
import { formatUsd, pnlClass, tokenSymbol, winRate } from '../utils/format'
import type { StrategyAssignment, StrategyConfig, StrategyStats } from '../api/types'

function ParamEditor({
  strategy,
  onSaved,
}: {
  strategy: StrategyConfig
  onSaved: () => void
}) {
  const [values, setValues] = useState<Record<string, string>>(
    Object.fromEntries(Object.entries(strategy.Config ?? {}).map(([k, v]) => [k, String(v)])),
  )
  const [saving, setSaving] = useState(false)

  async function save() {
    setSaving(true)
    try {
      const config = Object.fromEntries(
        Object.entries(values)
          .filter(([, v]) => v !== '')
          .map(([k, v]) => [k, Number(v)]),
      )
      await api.updateStrategy(strategy.ID, { config, enabled: strategy.Enabled })
      onSaved()
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="param-hover-box">
      {Object.entries(values).length === 0 && (
        <div className="text-dim" style={{ marginBottom: '0.5rem' }}>
          No overrides yet — this sub-strategy still runs origin defaults.
        </div>
      )}
      {Object.entries(values).map(([k, v]) => (
        <div key={k} style={{ marginBottom: '0.4rem' }}>
          <label className="text-dim" style={{ display: 'block', fontSize: '0.75rem' }}>
            {k}
          </label>
          <input
            type="text"
            value={v}
            onChange={(e) => setValues((prev) => ({ ...prev, [k]: e.target.value }))}
            style={{ width: '100%' }}
          />
        </div>
      ))}
      <button onClick={save} disabled={saving} style={{ marginTop: '0.3rem' }}>
        {saving ? 'Saving…' : 'Save'}
      </button>
    </div>
  )
}

function StrategyRow({
  strategy,
  stats,
  effectivelyActive,
  onChanged,
}: {
  strategy: StrategyConfig
  stats: StrategyStats | undefined
  // Whether this strategy row actually opens new positions right now: its own Enabled flag AND
  // at least one enabled strategy_assignments row. A strategy can read Enabled=true here and still
  // be fully idle — every assignment disabled by the Positions page's "active strategies" kind
  // filter (paper_trading_config.active_kinds) — since that filter bulk-toggles
  // strategy_assignments.enabled per kind, never the strategies table itself. Rendering Enabled
  // alone previously showed every strategy as "enabled" regardless of that filter.
  effectivelyActive: boolean
  onChanged: () => void
}) {
  const [editing, setEditing] = useState(false)

  async function toggleEnabled() {
    await api.updateStrategy(strategy.ID, {
      config: strategy.Config ?? {},
      enabled: !strategy.Enabled,
    })
    onChanged()
  }

  async function reset() {
    await api.resetStrategy(strategy.ID)
    onChanged()
  }

  async function del() {
    if (!confirm(`Delete sub-strategy "${strategy.Name}"?`)) return
    await api.deleteStrategy(strategy.ID)
    onChanged()
  }

  return (
    <tr>
      <td>
        {strategy.Name}
        {strategy.IsOrigin && <span className="badge badge-dim" style={{ marginLeft: 6 }}>origin</span>}
      </td>
      <td>
        <span className={'badge ' + (effectivelyActive ? 'badge-green' : 'badge-dim')}>
          {effectivelyActive ? 'active' : 'inactive'}
        </span>
      </td>
      <td>{stats?.SignalCount ?? '—'}</td>
      <td>{winRate(stats?.Wins, stats?.Losses)}</td>
      {/* Rounded for readability — the exact NUMERIC value stays in the database (CLAUDE.md §7).
          This is the sum of every closed trade's realized PnL, gains and losses together. */}
      <td className={'mono ' + pnlClass(stats ? Number(stats.RealizedPnL) : null)}>
        {stats ? formatUsd(Number(stats.RealizedPnL)) : '—'}
      </td>
      {/* The standalone "Params" column is gone, but parameter editing is not: it moves onto
          this actions cell, so a sub-strategy's overrides are still reachable (origins stay
          read-only, CLAUDE.md §11.3). */}
      <td className="param-edit-cell">
        {!strategy.IsOrigin && (
          <div style={{ display: 'flex', gap: '0.4rem' }}>
            <button onClick={toggleEnabled}>{strategy.Enabled ? 'Disable' : 'Enable'}</button>
            <button onClick={() => setEditing((v) => !v)}>{editing ? 'Close' : 'Edit params'}</button>
            <button onClick={reset}>Reset to origin</button>
            <button onClick={del}>Delete</button>
          </div>
        )}
        {editing && !strategy.IsOrigin && (
          <div style={{ marginTop: '0.5rem', maxWidth: 260 }}>
            <ParamEditor
              strategy={strategy}
              onSaved={() => {
                setEditing(false)
                onChanged()
              }}
            />
          </div>
        )}
      </td>
    </tr>
  )
}

function CloneForm({ origins, onCreated }: { origins: StrategyConfig[]; onCreated: () => void }) {
  // origins loads asynchronously (empty on first render), so a useState initializer would freeze
  // at 0 and never pick up the real default once origins arrives — every submit would then
  // silently no-op on the `!clonedFrom` guard below despite the <select> visually showing a
  // selection. null means "no explicit choice yet"; fall back to the first origin at submit time.
  const [clonedFrom, setClonedFrom] = useState<number | null>(null)
  const [name, setName] = useState('')
  const [instIds, setInstIds] = useState('')

  const effectiveClonedFrom = clonedFrom ?? origins[0]?.ID ?? 0

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (!name || !effectiveClonedFrom) return
    await api.createStrategy({
      name,
      clonedFrom: effectiveClonedFrom,
      instIds: instIds
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean),
    })
    setName('')
    setInstIds('')
    onCreated()
  }

  return (
    <form className="toolbar" onSubmit={submit}>
      <select value={effectiveClonedFrom} onChange={(e) => setClonedFrom(Number(e.target.value))}>
        {origins.map((o) => (
          <option key={o.ID} value={o.ID}>
            {o.Kind}
          </option>
        ))}
      </select>
      <input
        type="text"
        placeholder="new sub-strategy name"
        value={name}
        onChange={(e) => setName(e.target.value)}
      />
      <input
        type="text"
        placeholder="inst ids (comma-separated)"
        value={instIds}
        onChange={(e) => setInstIds(e.target.value)}
      />
      <button type="submit">Clone</button>
    </form>
  )
}

function ChartPanel({
  strategies,
  assignments,
}: {
  strategies: StrategyConfig[]
  assignments: StrategyAssignment[]
}) {
  // Picking a chart target = picking one assignment (strategy + inst + bar together), since the
  // chart needs all three and a strategy row can be assigned to several tokens/timeframes at once.
  const [assignmentId, setAssignmentId] = useState<number | null>(null)
  const effectiveAssignment = assignments.find((a) => a.ID === assignmentId) ?? assignments[0]

  const nameFor = (id: number) => strategies.find((s) => s.ID === id)?.Name ?? `#${id}`

  if (assignments.length === 0) {
    return (
      <div className="card">
        <h2>Parameter change history</h2>
        <div className="text-dim">No assignments yet — assign a strategy to a token/timeframe above to chart it.</div>
      </div>
    )
  }

  return (
    <div className="card">
      <h2>Parameter change history</h2>
      <div className="toolbar" style={{ marginBottom: '0.75rem' }}>
        <select
          value={effectiveAssignment.ID}
          onChange={(e) => setAssignmentId(Number(e.target.value))}
        >
          {assignments.map((a) => (
            <option key={a.ID} value={a.ID}>
              {nameFor(a.StrategyID)} — {tokenSymbol(a.InstID)} / {a.Bar}
            </option>
          ))}
        </select>
      </div>
      <ParamChangeChart
        strategyId={effectiveAssignment.StrategyID}
        instId={effectiveAssignment.InstID}
        bar={effectiveAssignment.Bar}
      />
    </div>
  )
}

export default function StrategiesPage() {
  const [strategies, setStrategies] = useState<StrategyConfig[]>([])
  const [assignments, setAssignments] = useState<StrategyAssignment[]>([])
  const [stats, setStats] = useState<Record<number, StrategyStats>>({})
  const [error, setError] = useState<string | null>(null)

  async function reload() {
    try {
      const [list, assigns] = await Promise.all([
        api.listStrategies(),
        api.listAssignments(),
      ])
      setStrategies(list)
      setAssignments(assigns)
      const entries = await Promise.all(
        list.map(async (s) => [s.ID, await api.strategyStats(s.ID)] as const),
      )
      setStats(Object.fromEntries(entries))
      setError(null)
    } catch (err) {
      setError((err as Error).message)
    }
  }

  useEffect(() => {
    reload()
  }, [])

  const origins = strategies.filter((s) => s.IsOrigin)
  const subStrategies = strategies.filter((s) => !s.IsOrigin)

  // A strategy row is effectively active only when its own Enabled flag is true AND at least one
  // of its strategy_assignments rows is enabled — see StrategyRow's effectivelyActive doc for why
  // Enabled alone is not the answer this column needs to give.
  const hasEnabledAssignment = new Set(
    assignments.filter((a) => a.Enabled).map((a) => a.StrategyID),
  )
  function isEffectivelyActive(s: StrategyConfig): boolean {
    return s.Enabled && hasEnabledAssignment.has(s.ID)
  }

  return (
    <div>
      {error && <div className="error-banner">{error}</div>}

      <div className="card">
        <h2>Strategies</h2>
        <table>
          <thead>
            <tr>
              <th className="th-static">Name</th>
              <th className="th-static">Status</th>
              <th className="th-static">Signals</th>
              <th className="th-static">Win rate</th>
              <th className="th-static">Realized PnL</th>
              <th className="th-static"></th>
            </tr>
          </thead>
          <tbody>
            {origins.map((s) => (
              <StrategyRow
                key={s.ID}
                strategy={s}
                stats={stats[s.ID]}
                effectivelyActive={isEffectivelyActive(s)}
                onChanged={reload}
              />
            ))}
            {subStrategies.map((s) => (
              <StrategyRow
                key={s.ID}
                strategy={s}
                stats={stats[s.ID]}
                effectivelyActive={isEffectivelyActive(s)}
                onChanged={reload}
              />
            ))}
          </tbody>
        </table>

        <div style={{ marginTop: '0.75rem' }}>
          <CloneForm origins={origins} onCreated={reload} />
        </div>
      </div>

      {/* The Assignments (token + timeframe) panel was removed as unused. The assignment data
          itself is still fetched, because ChartPanel needs it to know which strategy/inst/bar
          combinations exist to chart. */}
      <ChartPanel strategies={strategies} assignments={assignments} />
    </div>
  )
}
