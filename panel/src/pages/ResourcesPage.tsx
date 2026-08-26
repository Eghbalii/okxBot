import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'

// CLAUDE.md §11.1: server resources (CPU/RAM/GPU) are owned entirely by Prometheus + Grafana, not
// re-implemented here. This page just links/embeds the existing dashboard.
export default function ResourcesPage() {
  const { data, error } = usePolling(() => api.resources(), 60_000)

  return (
    <div>
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
    </div>
  )
}
