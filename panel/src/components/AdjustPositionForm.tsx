import { useState } from 'react'
import type { Position } from '../api/types'
import { tokenSymbol } from '../utils/format'

// Manual SL/TP-edit form for a real position (CLAUDE.md §27's real-trading plan §3b, 2026-09-03).
// Both inputs are a SIGNED percentage of margin, leverage-adjusted — negative moves the level to
// the loss side, positive to the profit side (e.g. -5 on a 10x position means "move to a 5% loss
// of margin," a 0.5% price move from entry). Unclamped on the backend: an explicit operator/admin
// action is trusted directly, unlike the model's own automated edits.
export default function AdjustPositionForm({
  position,
  onCancel,
  onSubmit,
}: {
  position: Position
  onCancel: () => void
  onSubmit: (slPct: string, tpPct: string) => void | Promise<void>
}) {
  const [slPct, setSlPct] = useState('')
  const [tpPct, setTpPct] = useState('')
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit() {
    setSubmitting(true)
    try {
      await onSubmit(slPct, tpPct)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="adjust-position-form">
      <span className="text-dim">
        Adjust {tokenSymbol(position.InstID)} #{position.ID} ({position.Side}, entry {position.EntryPx}, {position.Leverage}x)
      </span>
      <label>
        SL %
        <input
          type="number"
          step="0.1"
          placeholder="e.g. -5"
          value={slPct}
          onChange={(e) => setSlPct(e.target.value)}
          title="Signed, leverage-adjusted margin %. Negative = loss side, positive = profit side."
        />
      </label>
      <label>
        TP %
        <input
          type="number"
          step="0.1"
          placeholder="e.g. 10"
          value={tpPct}
          onChange={(e) => setTpPct(e.target.value)}
          title="Signed, leverage-adjusted margin %. Negative = loss side, positive = profit side."
        />
      </label>
      <button onClick={handleSubmit} disabled={submitting}>
        {submitting ? 'Applying…' : 'Apply'}
      </button>
      <button onClick={onCancel} disabled={submitting}>
        Cancel
      </button>
    </div>
  )
}
