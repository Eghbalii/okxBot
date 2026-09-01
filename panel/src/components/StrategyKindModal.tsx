import { useEffect, useState } from 'react'
import { api } from '../api/client'

// Global per-strategy-KIND on/off switch for paper trading (2026-09-01 request): applied
// uniformly across every token, bulk-toggling strategy_assignments.enabled for every assignment
// of that kind — distinct from the existing per-token/per-timeframe assignment granularity on the
// Strategies page, which stays the underlying mechanism this toggle drives.
export default function StrategyKindModal({
  activeKinds,
  onClose,
  onSave,
}: {
  activeKinds: string[]
  onClose: () => void
  onSave: (kinds: string[]) => Promise<void>
}) {
  const [allKinds, setAllKinds] = useState<string[]>([])
  const [selected, setSelected] = useState<Set<string>>(new Set(activeKinds))
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    api
      .listStrategies()
      .then((rows) => {
        const kinds = [...new Set(rows.map((s) => s.Kind))].sort()
        setAllKinds(kinds)
      })
      .catch((err) => setError((err as Error).message))
      .finally(() => setLoading(false))
  }, [])

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
      await onSave([...selected].sort())
      onClose()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <h2>Manage active strategies</h2>
          <button className="modal-close" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </div>

        <div className="text-dim" style={{ marginBottom: '0.75rem' }}>
          Only checked strategy kinds open new paper positions, across every token. Unchecking a
          kind here disables every one of its assignments — leave everything unchecked for no
          restriction (every currently-enabled assignment applies as-is).
        </div>

        {error && <div className="error-banner">{error}</div>}
        {loading && <div className="text-dim">Loading…</div>}

        {!loading && (
          <div style={{ display: 'flex', flexDirection: 'column', gap: '0.4rem' }}>
            {allKinds.map((kind) => (
              <label key={kind} style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                <input type="checkbox" checked={selected.has(kind)} onChange={() => toggle(kind)} />
                <span className="mono">{kind}</span>
              </label>
            ))}
          </div>
        )}

        <div className="toolbar" style={{ marginTop: '1rem' }}>
          <button onClick={save} disabled={saving || loading}>
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
