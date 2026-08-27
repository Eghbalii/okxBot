import { useEffect, useState } from 'react'
import { api } from '../api/client'
import ParamChangeChart from '../components/ParamChangeChart'
import type { StrategyAssignment, StrategyConfig, StrategyStats } from '../api/types'

function winRate(stats: StrategyStats | undefined): string {
  if (!stats) return '—'
  const decided = stats.Wins + stats.Losses
  if (decided === 0) return '—'
  return `${Math.round((stats.Wins / decided) * 100)}%`
}

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
  onChanged,
}: {
  strategy: StrategyConfig
  stats: StrategyStats | undefined
  onChanged: () => void
}) {
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
      <td className="mono">{strategy.Kind}</td>
      <td>{strategy.InstIDs.join(', ') || '—'}</td>
      <td>
        <span className={'badge ' + (strategy.Enabled ? 'badge-green' : 'badge-dim')}>
          {strategy.Enabled ? 'enabled' : 'disabled'}
        </span>
      </td>
      <td>{stats?.SignalCount ?? '—'}</td>
      <td>{winRate(stats)}</td>
      <td className={stats && Number(stats.RealizedPnL) < 0 ? 'text-dim' : ''}>
        {stats?.RealizedPnL ?? '—'}
      </td>
      <td className="param-edit-cell">
        {strategy.IsOrigin ? (
          <span className="text-dim">read-only</span>
        ) : (
          <>
            <span>hover to edit</span>
            <ParamEditor strategy={strategy} onSaved={onChanged} />
          </>
        )}
      </td>
      <td>
        {!strategy.IsOrigin && (
          <div style={{ display: 'flex', gap: '0.4rem' }}>
            <button onClick={toggleEnabled}>{strategy.Enabled ? 'Disable' : 'Enable'}</button>
            <button onClick={reset}>Reset to origin</button>
            <button onClick={del}>Delete</button>
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

function AssignmentsPanel({
  strategies,
  assignments,
  onChanged,
}: {
  strategies: StrategyConfig[]
  assignments: StrategyAssignment[]
  onChanged: () => void
}) {
  // strategies loads asynchronously (empty on first render) — see the identical note on
  // CloneForm's clonedFrom above. null = no explicit choice yet, default resolved at submit time.
  const [strategyId, setStrategyId] = useState<number | null>(null)
  const [instId, setInstId] = useState('')
  const [bar, setBar] = useState('1m')

  const effectiveStrategyId = strategyId ?? strategies[0]?.ID ?? 0

  const nameFor = (id: number) => strategies.find((s) => s.ID === id)?.Name ?? `#${id}`

  async function create(e: React.FormEvent) {
    e.preventDefault()
    if (!effectiveStrategyId || !instId) return
    await api.createAssignment({ strategyId: effectiveStrategyId, instId, bar })
    setInstId('')
    onChanged()
  }

  return (
    <div className="card">
      <h2>Assignments (token + timeframe)</h2>
      <table>
        <thead>
          <tr>
            <th>Strategy</th>
            <th>Instrument</th>
            <th>Bar</th>
            <th>Status</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {assignments.map((a) => (
            <tr key={a.ID}>
              <td>{nameFor(a.StrategyID)}</td>
              <td>{a.InstID}</td>
              <td>{a.Bar}</td>
              <td>
                <span className={'badge ' + (a.Enabled ? 'badge-green' : 'badge-dim')}>
                  {a.Enabled ? 'enabled' : 'disabled'}
                </span>
              </td>
              <td>
                <div style={{ display: 'flex', gap: '0.4rem' }}>
                  <button
                    onClick={async () => {
                      await api.setAssignmentEnabled(a.ID, !a.Enabled)
                      onChanged()
                    }}
                  >
                    {a.Enabled ? 'Disable' : 'Enable'}
                  </button>
                  <button
                    onClick={async () => {
                      await api.deleteAssignment(a.ID)
                      onChanged()
                    }}
                  >
                    Delete
                  </button>
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <form className="toolbar" onSubmit={create} style={{ marginTop: '0.75rem' }}>
        <select value={effectiveStrategyId} onChange={(e) => setStrategyId(Number(e.target.value))}>
          {strategies.map((s) => (
            <option key={s.ID} value={s.ID}>
              {s.Name}
            </option>
          ))}
        </select>
        <input
          type="text"
          placeholder="inst id, e.g. BTC-USDT-SWAP"
          value={instId}
          onChange={(e) => setInstId(e.target.value)}
        />
        <select value={bar} onChange={(e) => setBar(e.target.value)}>
          {['1m', '3m', '5m', '15m', '1H', '4H', '1D'].map((b) => (
            <option key={b} value={b}>
              {b}
            </option>
          ))}
        </select>
        <button type="submit">Assign</button>
      </form>
    </div>
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
              {nameFor(a.StrategyID)} — {a.InstID} / {a.Bar}
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

  return (
    <div>
      {error && <div className="error-banner">{error}</div>}

      <div className="card">
        <h2>Strategies</h2>
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Kind</th>
              <th>Instruments</th>
              <th>Status</th>
              <th>Signals</th>
              <th>Win rate</th>
              <th>Realized PnL</th>
              <th>Params</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {origins.map((s) => (
              <StrategyRow key={s.ID} strategy={s} stats={stats[s.ID]} onChanged={reload} />
            ))}
            {subStrategies.map((s) => (
              <StrategyRow key={s.ID} strategy={s} stats={stats[s.ID]} onChanged={reload} />
            ))}
          </tbody>
        </table>

        <div style={{ marginTop: '0.75rem' }}>
          <CloneForm origins={origins} onCreated={reload} />
        </div>
      </div>

      <AssignmentsPanel strategies={strategies} assignments={assignments} onChanged={reload} />

      <ChartPanel strategies={strategies} assignments={assignments} />
    </div>
  )
}
