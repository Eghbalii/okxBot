import { useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import type { UnitStatus } from '../api/types'

function formatUptime(sec?: number): string {
  if (!sec || sec <= 0) return '—'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  return `${m}m`
}

function UnitCard({ unit }: { unit: UnitStatus }) {
  const [logs, setLogs] = useState<string | null>(null)
  const [errorsOnly, setErrorsOnly] = useState(false)
  const [loadingLogs, setLoadingLogs] = useState(false)

  async function loadLogs(onlyErrors: boolean) {
    setLoadingLogs(true)
    setErrorsOnly(onlyErrors)
    try {
      const res = await api.modelLogs(unit.unit, { lines: 200, errorsOnly: onlyErrors })
      setLogs(res.logs)
    } catch (err) {
      setLogs(`failed to load logs: ${(err as Error).message}`)
    } finally {
      setLoadingLogs(false)
    }
  }

  return (
    <div className="card">
      <h2>{unit.unit}</h2>
      {unit.error ? (
        <div className="error-banner">{unit.error}</div>
      ) : (
        <div className="grid" style={{ marginBottom: '0.75rem' }}>
          <div>
            <div className="text-dim">State</div>
            <span className={'badge ' + (unit.active ? 'badge-green' : 'badge-red')}>
              {unit.state} / {unit.subState}
            </span>
          </div>
          <div>
            <div className="text-dim">Uptime</div>
            <div>{formatUptime(unit.uptimeSec)}</div>
          </div>
          <div>
            <div className="text-dim">Restarts</div>
            <span className={'badge ' + (unit.restartCount > 0 ? 'badge-yellow' : 'badge-dim')}>
              {unit.restartCount}
            </span>
          </div>
        </div>
      )}

      <div className="toolbar">
        <button onClick={() => loadLogs(false)} disabled={loadingLogs}>
          Tail logs
        </button>
        <button onClick={() => loadLogs(true)} disabled={loadingLogs}>
          Errors only
        </button>
      </div>
      {logs !== null && (
        <pre className="logs">{logs || `(no ${errorsOnly ? 'error ' : ''}log lines)`}</pre>
      )}
    </div>
  )
}

export default function ModelStatusPage() {
  const { data, error } = usePolling(() => api.modelStatus(), 10_000)

  return (
    <div>
      <div className="card">
        <h2>RL Inference Service</h2>
        {error && <div className="error-banner">{error}</div>}
        {data && (
          <div className="grid">
            <div>
              <div className="text-dim">Reachable</div>
              <span className={'badge ' + (data.health.reachable ? 'badge-green' : 'badge-red')}>
                {data.health.reachable ? 'up' : 'down'}
              </span>
            </div>
            <div>
              <div className="text-dim">Model loaded</div>
              <span
                className={
                  'badge ' + (data.health.modelLoaded ? 'badge-green' : 'badge-yellow')
                }
              >
                {data.health.modelLoaded ? 'loaded' : 'not loaded (flat actions only)'}
              </span>
            </div>
            {data.health.error && <div className="error-banner">{data.health.error}</div>}
          </div>
        )}
      </div>

      {data?.units.map((u) => <UnitCard key={u.unit} unit={u} />)}
    </div>
  )
}
