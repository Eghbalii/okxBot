import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import { pnlClass } from '../utils/format'
import type { TesterConfig, TesterVersion, TesterVersionDetail } from '../api/types'

// Diffs two versions' config objects down to only the keys that actually changed, for the
// compare-to-parent modal (operator's explicit request: "click a new version's name to see the
// parent's params vs. this version's params").
function diffConfig(
  parent: Record<string, number> | null | undefined,
  current: Record<string, number> | null | undefined,
) {
  const keys = new Set([...Object.keys(parent ?? {}), ...Object.keys(current ?? {})])
  const rows: { key: string; from: number | undefined; to: number | undefined; changed: boolean }[] = []
  for (const key of keys) {
    const from = parent?.[key]
    const to = current?.[key]
    rows.push({ key, from, to, changed: from !== to })
  }
  return rows.sort((a, b) => a.key.localeCompare(b.key))
}

function VersionCompareModal({ versionId, onClose }: { versionId: number; onClose: () => void }) {
  const [detail, setDetail] = useState<TesterVersionDetail | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    api
      .testerVersion(versionId)
      .then(setDetail)
      .catch((err) => setError((err as Error).message))
  }, [versionId])

  const diffRows = useMemo(
    () => (detail ? diffConfig(detail.parent?.effectiveConfig, detail.version.effectiveConfig) : []),
    [detail],
  )

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <h2>{detail?.version.displayName ?? `version #${versionId}`}</h2>
          <button className="modal-close" onClick={onClose} aria-label="Close">
            ×
          </button>
        </div>
        {error && <div className="error-banner">{error}</div>}
        {!detail && !error && <div className="text-dim">Loading…</div>}
        {detail && (
          <>
            <div className="text-dim" style={{ marginBottom: '0.6rem' }}>
              {detail.parent
                ? `Compared to its parent, ${detail.parent.displayName}`
                : 'This is the origin version — no parent to compare against.'}
            </div>
            <table>
              <thead>
                <tr>
                  <th>Param</th>
                  <th>{detail.parent?.displayName ?? 'origin default'}</th>
                  <th>{detail.version.displayName}</th>
                </tr>
              </thead>
              <tbody>
                {/* Diffed on effectiveConfig (every param's ACTUAL value, defaults included), not
                    the raw override-only config — a version that overrides nothing has an empty
                    config, which made every row show "— -> new value" and read as if nothing had
                    a prior value at all. */}
                {diffRows.map((row) => (
                  <tr key={row.key}>
                    <td className="mono">{row.key}</td>
                    <td className={'mono ' + (row.changed ? 'text-dim' : '')}>{row.from ?? '—'}</td>
                    <td className={'mono ' + (row.changed ? 'text-green' : '')}>{row.to ?? '—'}</td>
                  </tr>
                ))}
                {diffRows.length === 0 && (
                  <tr>
                    <td colSpan={3} className="text-dim">
                      This kind has no tunable parameters.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </>
        )}
      </div>
    </div>
  )
}

// Go duration strings (e.g. "6h", "90m") parsed down to a whole number of hours for a plain
// numeric input — this panel only ever needs hour-granularity for a position-staleness limit, and
// round-tripping through hours avoids asking the operator to type Go duration syntax by hand.
function parseHours(goDuration: string): string {
  const match = /^(\d+(?:\.\d+)?)h$/.exec(goDuration.trim())
  if (match) return match[1]
  const minutesMatch = /^(\d+)m$/.exec(goDuration.trim())
  if (minutesMatch) return (Number(minutesMatch[1]) / 60).toString()
  return ''
}

function ConfigPanel() {
  const [cfg, setCfg] = useState<TesterConfig | null>(null)
  const [bar, setBar] = useState('')
  const [notional, setNotional] = useState('')
  const [leverage, setLeverage] = useState('')
  const [maxOpenHours, setMaxOpenHours] = useState('')
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)
  const [restarting, setRestarting] = useState(false)
  const [message, setMessage] = useState<string | null>(null)

  function load() {
    api.testerConfig().then((c) => {
      setCfg(c)
      setBar(c.bar)
      setNotional(c.notionalUsd)
      setLeverage(c.leverage)
      setMaxOpenHours(parseHours(c.maxOpenDuration))
      setDirty(false)
    })
  }

  useEffect(load, [])

  async function save() {
    setSaving(true)
    try {
      const hours = Number(maxOpenHours)
      await api.saveTesterConfig({
        bar,
        notionalUsd: notional,
        leverage,
        maxOpenDuration: Number.isFinite(hours) && hours > 0 ? `${hours}h` : undefined,
      })
      setDirty(false)
      setMessage('Saved. Restart the service for the changes to take effect.')
    } finally {
      setSaving(false)
    }
  }

  async function restart() {
    if (!confirm('Restart the strategy-tester service now? Only this service is affected — no other service is touched.')) return
    setRestarting(true)
    try {
      await api.restartTester()
      setMessage('Restart requested — the service will be back within a few seconds.')
    } finally {
      setRestarting(false)
    }
  }

  return (
    <div className="card">
      <h2>Config</h2>
      {!cfg && <div className="text-dim">Loading…</div>}
      {cfg && (
        <div className="toolbar">
          <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
            Timeframe
            <select
              value={bar}
              onChange={(e) => {
                setBar(e.target.value)
                setDirty(true)
              }}
            >
              {['1m', '3m', '5m', '15m', '1H', '4H', '1D'].map((b) => (
                <option key={b} value={b}>
                  {b}
                </option>
              ))}
            </select>
          </label>
          <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
            Notional USD
            <input
              type="text"
              style={{ width: '5rem' }}
              value={notional}
              onChange={(e) => {
                setNotional(e.target.value)
                setDirty(true)
              }}
            />
          </label>
          <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
            Leverage
            <input
              type="text"
              style={{ width: '4rem' }}
              value={leverage}
              onChange={(e) => {
                setLeverage(e.target.value)
                setDirty(true)
              }}
            />
            x
          </label>
          <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
            Max open (hours)
            <input
              type="text"
              style={{ width: '4rem' }}
              value={maxOpenHours}
              onChange={(e) => {
                setMaxOpenHours(e.target.value)
                setDirty(true)
              }}
              title="Force-close a position that has been open this long, close_reason='timeout'"
            />
          </label>
          <button onClick={save} disabled={!dirty || saving}>
            {saving ? 'Saving…' : 'Save'}
          </button>
          <button onClick={restart} disabled={restarting}>
            {restarting ? 'Restarting…' : 'Restart service'}
          </button>
        </div>
      )}
      {cfg && (
        <div className="text-dim" style={{ marginTop: '0.5rem' }}>
          Instruments: {cfg.instIds.join(', ') || '—'}
        </div>
      )}
      {message && <div className="text-dim" style={{ marginTop: '0.5rem' }}>{message}</div>}
    </div>
  )
}

export default function StrategyTesterPage() {
  const [versions, setVersions] = useState<TesterVersion[]>([])
  const [error, setError] = useState<string | null>(null)
  const [compareId, setCompareId] = useState<number | null>(null)

  function reload() {
    api
      .testerStats()
      .then(setVersions)
      .catch((err) => setError((err as Error).message))
  }

  useEffect(() => {
    reload()
    const id = setInterval(reload, 10_000)
    return () => clearInterval(id)
  }, [])

  // Grouped by kind, newest version first within each — mirrors how tester_strategy_versions is
  // queried server-side, so the enabled (live-trading) version is always visually first.
  const byKind = useMemo(() => {
    const groups = new Map<string, TesterVersion[]>()
    for (const v of versions) {
      const list = groups.get(v.kind) ?? []
      list.push(v)
      groups.set(v.kind, list)
    }
    for (const list of groups.values()) list.sort((a, b) => b.version - a.version)
    return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b))
  }, [versions])

  async function enable(id: number) {
    await api.enableTesterVersion(id)
    reload()
  }

  return (
    <div>
      <div className="text-dim" style={{ marginBottom: '0.75rem' }}>
        Independent strategy-signal validator — trades every strategy kind on the same live
        instruments as production, entirely separate from paper-trading and the RL agent. Stats
        only; no positions list.
      </div>

      {error && <div className="error-banner">{error}</div>}

      <div className="card">
        <h2>Strategy versions</h2>
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Version</th>
                <th>Status</th>
                <th>Signals</th>
                <th>Open</th>
                <th>TP closes</th>
                <th>SL closes</th>
                <th>Win rate</th>
                <th>Realized PnL</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {byKind.map(([kind, list]) => (
                <>
                  <tr key={`${kind}-header`}>
                    <td colSpan={9} className="text-dim" style={{ fontWeight: 600, paddingTop: '0.8rem' }}>
                      {kind}
                    </td>
                  </tr>
                  {list.map((v) => (
                    <tr key={v.id}>
                      <td>
                        <button
                          className="order-id-button"
                          onClick={() => setCompareId(v.id)}
                          title="Compare this version's params to its parent"
                        >
                          {v.displayName}
                        </button>
                      </td>
                      <td>
                        <span className={'badge ' + (v.enabled ? 'badge-green' : 'badge-dim')}>
                          {v.enabled ? 'live' : 'inactive'}
                        </span>
                      </td>
                      <td>{v.stats.signalCount}</td>
                      <td>{v.stats.openCount}</td>
                      <td>{v.stats.tpCloses}</td>
                      <td>{v.stats.slCloses}</td>
                      <td>{v.stats.winRatePct}</td>
                      <td className={'mono ' + pnlClass(Number(v.stats.realizedPnl))}>
                        ${Number(v.stats.realizedPnl).toFixed(0)}
                      </td>
                      <td>
                        {!v.enabled && <button onClick={() => enable(v.id)}>Make live</button>}
                      </td>
                    </tr>
                  ))}
                </>
              ))}
              {versions.length === 0 && !error && (
                <tr>
                  <td colSpan={9} className="text-dim">
                    No strategy versions yet.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      </div>

      <ConfigPanel />

      {compareId !== null && <VersionCompareModal versionId={compareId} onClose={() => setCompareId(null)} />}
    </div>
  )
}
