import { useState } from 'react'

// Per-token active/inactive toggle for paper trading (2026-09-01 request): a disabled token stops
// opening NEW positions (checked in evaluateStrategies via PaperTrader.OpensDisabled) but any of
// its already-open positions keep running to their normal SL/TP/timeout close — ingestion is
// untouched, so re-enabling has no data gap. The checkbox here is "enabled" (checked = trades),
// inverted against the saved disabledInstIds list.
export default function TokenModal({
  allInstIds,
  disabledInstIds,
  onClose,
  onSave,
}: {
  allInstIds: string[]
  disabledInstIds: string[]
  onClose: () => void
  onSave: (disabledInstIds: string[]) => Promise<void>
}) {
  const [disabled, setDisabled] = useState<Set<string>>(new Set(disabledInstIds))
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  function toggle(instId: string) {
    setDisabled((prev) => {
      const next = new Set(prev)
      if (next.has(instId)) next.delete(instId)
      else next.add(instId)
      return next
    })
  }

  async function save() {
    setSaving(true)
    try {
      await onSave([...disabled].sort())
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
          <h2>Manage active tokens</h2>
          <button className="modal-close" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </div>

        <div className="text-dim" style={{ marginBottom: '0.75rem' }}>
          Unchecking a token stops it from opening new paper positions. Its existing open
          positions (if any) keep closing normally — close one manually from the Positions table
          if needed.
        </div>

        {error && <div className="error-banner">{error}</div>}

        <div style={{ display: 'flex', flexDirection: 'column', gap: '0.4rem' }}>
          {allInstIds.length === 0 && <div className="text-dim">No tokens configured.</div>}
          {allInstIds.map((instId) => (
            <label key={instId} style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
              <input type="checkbox" checked={!disabled.has(instId)} onChange={() => toggle(instId)} />
              <span className="mono">{instId}</span>
            </label>
          ))}
        </div>

        <div className="toolbar" style={{ marginTop: '1rem' }}>
          <button onClick={save} disabled={saving}>
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
