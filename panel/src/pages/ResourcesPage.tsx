import { useEffect, useState } from 'react'
import ServiceHealthBox from '../components/ServiceHealthBox'
import { usePolling } from '../hooks/usePolling'
import { api, ApiError } from '../api/client'
import type { CleanupCandidate, CleanupResult } from '../api/types'

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

// A relative date is easier to act on than a bare timestamp for someone deciding "is this old
// enough to delete" — e.g. "12 days ago" rather than a raw ISO string.
function formatRelativeDate(iso: string): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return iso
  const days = Math.floor((Date.now() - then) / (1000 * 60 * 60 * 24))
  if (days <= 0) return 'today'
  if (days === 1) return 'yesterday'
  return `${days} days ago`
}

// Automatic cleanup: build cache, stopped containers, and images nothing runs anymore. Every one
// of these is safe by Docker's own construction (see the backend endpoint's own doc comment) —
// no confirmation is asked because none of these can ever be a service's live data.
function AutomaticCleanup() {
  const [running, setRunning] = useState(false)
  const [result, setResult] = useState<CleanupResult | null>(null)

  async function run() {
    setRunning(true)
    setResult(null)
    try {
      setResult(await api.cleanupDisk())
    } catch (err) {
      setResult({ buildCacheBytes: 0, containersBytes: 0, imagesBytes: 0, error: (err as Error).message })
    } finally {
      setRunning(false)
    }
  }

  const total = result ? result.buildCacheBytes + result.containersBytes + result.imagesBytes : 0

  return (
    <div className="card">
      <h2>Automatic cleanup</h2>
      <p className="text-dim">
        Removes leftover Docker build cache, old stopped containers, and unused images — things
        nothing on this server actually uses anymore. This never touches your database, the trading
        model, or anything a running service depends on, so it's always safe to run.
      </p>
      <button className="btn-primary" onClick={run} disabled={running}>
        {running ? 'Cleaning…' : 'Free disk space'}
      </button>
      {result && !result.error && (
        <div style={{ marginTop: '0.75rem' }}>
          <div className="stat-row">
            <span className="text-dim">Total reclaimed</span>
            <span className="mono">{formatBytes(total)}</span>
          </div>
          {total === 0 && (
            <p className="text-dim" style={{ marginTop: '0.5rem' }}>
              Nothing to clean up right now — your server is already tidy.
            </p>
          )}
        </div>
      )}
      {result?.error && <div className="error-banner" style={{ marginTop: '0.75rem' }}>{result.error}</div>}
    </div>
  )
}

// Confirm-per-item cleanup: real accumulated files (old training data, model backups, config
// backups) that are safe to delete but represent a real choice — an operator should see what a
// file is, how big it is, and how old it is before it's removed, per the explicit 2026-09-20
// request that this NOT be folded into the automatic button above.
function FileCleanup() {
  const [candidates, setCandidates] = useState<CleanupCandidate[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [confirming, setConfirming] = useState<CleanupCandidate | null>(null)
  const [deletingId, setDeletingId] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  async function load() {
    try {
      setCandidates(await api.cleanupCandidates())
      setLoadError(null)
    } catch (err) {
      // A 503 means this deployment hasn't enabled the feature — not an error to alarm over, just
      // nothing to show (same as an empty candidate list).
      if (err instanceof ApiError && err.status === 503) {
        setCandidates([])
        setLoadError(null)
        return
      }
      setLoadError((err as Error).message)
    }
  }

  useEffect(() => {
    load()
  }, [])

  async function confirmDelete() {
    if (!confirming) return
    setDeletingId(confirming.id)
    setActionError(null)
    try {
      const res = await api.deleteCleanupCandidate(confirming.id)
      setNotice(`Removed ${res.deleted} (${formatBytes(res.sizeBytes)} freed)`)
      setConfirming(null)
      await load()
    } catch (err) {
      setActionError((err as Error).message)
    } finally {
      setDeletingId(null)
    }
  }

  if (loadError) {
    return (
      <div className="card">
        <h2>Old files</h2>
        <div className="error-banner">{loadError}</div>
      </div>
    )
  }

  if (candidates && candidates.length === 0) {
    return null // nothing to show, and nothing to explain — an empty list isn't news
  }

  return (
    <div className="card">
      <h2>Old files</h2>
      <p className="text-dim">
        These are files that have piled up over time and are no longer needed by anything running.
        Nothing here is deleted automatically — review each one and confirm before it's removed.
      </p>
      {notice && <div className="stat-row" style={{ marginTop: '0.5rem' }}><span>{notice}</span></div>}
      {candidates?.map((c) => (
        <div
          key={c.id}
          style={{
            display: 'flex',
            justifyContent: 'space-between',
            alignItems: 'center',
            gap: '1rem',
            padding: '0.6rem 0',
            borderTop: '1px solid var(--border)',
          }}
        >
          <div style={{ minWidth: 0 }}>
            <div className="mono" style={{ fontSize: '0.85rem' }}>{c.path}</div>
            <div className="text-dim" style={{ fontSize: '0.85rem' }}>{c.description}</div>
            <div className="text-dim" style={{ fontSize: '0.8rem' }}>
              {formatBytes(c.sizeBytes)} · last changed {formatRelativeDate(c.modifiedAt)}
            </div>
          </div>
          <button onClick={() => setConfirming(c)} disabled={deletingId !== null}>
            Delete
          </button>
        </div>
      ))}

      {confirming && (
        <div
          role="dialog"
          style={{
            position: 'fixed',
            inset: 0,
            background: 'rgba(0,0,0,0.5)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            zIndex: 1000,
          }}
        >
          <div className="card" style={{ maxWidth: 480 }}>
            <h3>Delete this file?</h3>
            <p className="mono" style={{ fontSize: '0.85rem' }}>{confirming.path}</p>
            <p>{confirming.description}</p>
            <p className="text-dim">
              Size: {formatBytes(confirming.sizeBytes)} · Last changed: {formatRelativeDate(confirming.modifiedAt)}
            </p>
            {actionError && <div className="error-banner">{actionError}</div>}
            <div style={{ display: 'flex', gap: '0.5rem', marginTop: '1rem' }}>
              <button className="btn-primary" onClick={confirmDelete} disabled={deletingId !== null}>
                {deletingId ? 'Deleting…' : 'Yes, delete it'}
              </button>
              <button onClick={() => setConfirming(null)} disabled={deletingId !== null}>
                Cancel
              </button>
            </div>
          </div>
        </div>
      )}
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

      <AutomaticCleanup />
      <FileCleanup />
    </div>
  )
}
