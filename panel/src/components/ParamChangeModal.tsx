import { useEffect, useState } from 'react'
import { api } from '../api/client'
import { formatDateTime } from '../utils/format'
import type { ParamChange } from '../api/types'

// Shows a strategy's parameter-change timeline — old tuned values vs. new tuned values, one row
// per change — reusing the existing param-changes table/endpoint built for CLAUDE.md §16.7's
// manual-edit tracking rather than a new mechanism: the optimize pipeline's own promotions now
// write to the same table (a promotion IS a parameter change, just sourced from Optuna instead of
// an operator's hand edit), so this view works for both without new backend plumbing.
export default function ParamChangeModal({
  strategyId,
  instId,
  displayName,
  onClose,
}: {
  strategyId: number
  instId: string
  displayName: string
  onClose: () => void
}) {
  const [changes, setChanges] = useState<ParamChange[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    api
      .paramChanges(strategyId, { instId })
      .then(setChanges)
      .catch((err) => setError((err as Error).message))
  }, [strategyId, instId])

  function paramRows(oldConfig: Record<string, number> | null, newConfig: Record<string, number>) {
    const keys = new Set([...(oldConfig ? Object.keys(oldConfig) : []), ...Object.keys(newConfig)])
    return [...keys].sort()
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()} style={{ maxWidth: 640 }}>
        <div className="modal-header">
          <h2>Parameter history — {displayName}</h2>
          <button className="modal-close" onClick={onClose} aria-label="Close">
            ×
          </button>
        </div>

        <div className="text-dim" style={{ fontSize: '0.8rem', marginBottom: '0.75rem' }}>
          Every change to this strategy's tuned parameters for {instId}, newest first. A row with no
          "previous" column is the version's first-ever set of values — there was nothing before it
          to compare against. Changes sourced from the optimizer pipeline happen automatically each
          time a better-performing candidate replaces this one; "manual" rows come from a direct
          panel edit.
        </div>

        {error && <div className="error-banner">{error}</div>}
        {!error && !changes && <div className="text-dim">Loading…</div>}
        {changes && changes.length === 0 && <div className="text-dim">No recorded parameter changes for this strategy yet.</div>}

        {changes && changes.length > 0 && (
          <table>
            <thead>
              <tr>
                <th className="th-static">When</th>
                <th className="th-static">Source</th>
                <th className="th-static">Parameter</th>
                <th className="th-static">Previous</th>
                <th className="th-static">Updated</th>
              </tr>
            </thead>
            <tbody>
              {changes.map((c) =>
                paramRows(c.OldConfig, c.NewConfig).map((key, i) => (
                  <tr key={`${c.ID}-${key}`}>
                    {i === 0 && (
                      <>
                        <td rowSpan={paramRows(c.OldConfig, c.NewConfig).length}>{formatDateTime(c.CreatedAt)}</td>
                        <td rowSpan={paramRows(c.OldConfig, c.NewConfig).length}>
                          <span className={'badge ' + (c.Source === 'optimizer' ? 'badge-green' : 'badge-dim')}>{c.Source}</span>
                        </td>
                      </>
                    )}
                    <td className="mono">{key}</td>
                    <td className="mono text-dim">{c.OldConfig?.[key] ?? '—'}</td>
                    <td className="mono">{c.NewConfig[key] ?? '—'}</td>
                  </tr>
                )),
              )}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}
