import { useState } from 'react'
import ServiceHealthBox from '../components/ServiceHealthBox'
import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import type { CleanupResult } from '../api/types'

// Bytes as a human figure — this reports gigabytes routinely (7.28GB of build cache accumulated in
// a single day of rebuilds), so raw bytes would be unreadable.
function formatBytes(n: number): string {
  if (n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

// Reclaims the Docker build cache, which is what actually fills this box: rebuilding services
// accumulates gigabytes of it, and nothing reclaims it on its own.
//
// Build cache only, by design. Images are not pruned (a service with no running container between
// deploys is still needed at the next one) and neither are volumes (the database and the RL model's
// replay buffer live there — losing the buffer makes the model forget every experience it has
// collected). Those calls do not exist in the backend at all rather than sitting behind a
// confirmation, since a button that can destroy them is one that eventually does.
function DiskCleanup() {
  const [running, setRunning] = useState(false)
  const [result, setResult] = useState<CleanupResult | null>(null)

  async function run() {
    setRunning(true)
    setResult(null)
    try {
      setResult(await api.cleanupDisk())
    } catch (err) {
      setResult({ buildCacheBytes: 0, error: (err as Error).message })
    } finally {
      setRunning(false)
    }
  }

  return (
    <div className="card">
      <h2>Disk cleanup</h2>
      <p className="text-dim">
        Frees the Docker build cache, which grows by gigabytes as services are rebuilt and is never
        reclaimed automatically. Images, volumes and the RL model files are never touched.
      </p>
      <button className="btn-primary" onClick={run} disabled={running}>
        {running ? 'Cleaning…' : 'Free disk space'}
      </button>
      {result && !result.error && (
        <div className="stat-row" style={{ marginTop: '0.75rem' }}>
          <span className="text-dim">Reclaimed</span>
          <span className="mono">{formatBytes(result.buildCacheBytes)}</span>
        </div>
      )}
      {result?.error && <div className="error-banner" style={{ marginTop: '0.75rem' }}>{result.error}</div>}
    </div>
  )
}

// CLAUDE.md §11.1: server resources (CPU/RAM/GPU) are owned entirely by Prometheus + Grafana, not
// re-implemented here. This page just links/embeds the existing dashboard.
export default function ResourcesPage() {
  const { data, error } = usePolling(() => api.resources(), 60_000)

  return (
    <div>
      {/* Service health first: when something is wrong this is the thing being looked for, and
          burying it under the Grafana link would put the least urgent content on top. */}
      <ServiceHealthBox />

      <div className="card">
        <h2>Server Resources</h2>
        {error && <div className="error-banner">{error}</div>}
        {data && (
          <>
            <p className="text-dim">
              CPU/RAM/GPU and process metrics are tracked in Grafana (Prometheus-scraped, CLAUDE.md
              §11.6).
            </p>
            <a href={data.grafanaUrl} target="_blank" rel="noreferrer">
              <button>Open Grafana ↗</button>
            </a>
            {data.grafanaUrl && (
              <div style={{ marginTop: '1rem' }}>
                <iframe
                  src={data.grafanaUrl}
                  title="Grafana"
                  style={{
                    width: '100%',
                    height: '70vh',
                    border: '1px solid var(--border)',
                    borderRadius: 8,
                  }}
                />
              </div>
            )}
          </>
        )}
      </div>

      <DiskCleanup />
    </div>
  )
}
