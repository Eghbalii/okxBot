import { memo, useState } from 'react'
import type { Position } from '../api/types'
import { tokenSymbol, trimPrice } from '../utils/format'

// Manual SL/TP-edit dialog for a real position (CLAUDE.md §27's real-trading plan §3b).
//
// Rendered as a modal rather than a row expanded beneath the order (2026-09-08 request): an
// expanded row pushed the rest of the table down and, on a table that refetches every 5s, the
// controls could shift under the pointer mid-edit. A modal keeps the edit anchored regardless of
// what the table does behind it.
// Both inputs are a SIGNED percentage of margin, leverage-adjusted — negative moves the level to
// the loss side, positive to the profit side (e.g. -5 on a 10x position means "move to a 5% loss
// of margin," a 0.5% price move from entry). Unclamped on the backend: an explicit operator/admin
// action is trusted directly, unlike the model's own automated edits.
//
// The edit is applied to the resting SL/TP order on the EXCHANGE first and only then stored
// locally (CLAUDE.md §35), so what this dialog reports as saved is what OKX is actually enforcing.
// A position still opening has no resting order yet and the backend refuses the edit rather than
// recording a level the exchange never received.
function AdjustPositionForm({
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
    <div className="modal-backdrop" onClick={onCancel}>
      <div className="modal modal-narrow" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <h2>
            Update #{position.ID} — <span title={position.InstID}>{tokenSymbol(position.InstID)}</span>{' '}
            <span className="text-dim">
              {position.Side} · entry {trimPrice(position.EntryPx)} · {position.Leverage}x
            </span>
          </h2>
          <button className="modal-close" onClick={onCancel} aria-label="Close" disabled={submitting}>
            ✕
          </button>
        </div>

        <p className="text-dim adjust-help">
          Both values are a <strong>signed percentage of margin</strong>, leverage-adjusted:
          negative moves the level to the loss side, positive to the profit side. On a 10x position,
          −5 means "stop at a 5% loss of margin" — a 0.5% price move from entry. Leave a field empty
          to leave that level unchanged. Not clamped: an explicit operator edit is trusted directly,
          unlike the model's own automated moves.
        </p>

        <div className="adjust-fields">
          <label>
            SL %
            <input
              type="number"
              step="0.1"
              placeholder="e.g. -5"
              value={slPct}
              onChange={(e) => setSlPct(e.target.value)}
              autoFocus
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
            />
          </label>
        </div>

        <div className="adjust-actions">
          <button className="btn-primary" onClick={handleSubmit} disabled={submitting || (!slPct && !tpPct)}>
            {submitting ? 'Applying…' : 'Apply'}
          </button>
          <button onClick={onCancel} disabled={submitting}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  )
}

// Memoized on the fields this form reads, for the same reason OrderDetailModal is: the positions
// page re-renders on every live price tick (~45/second) and hands this a new position object each
// time. React preserves the typed SL/TP input across re-renders, so nothing was being lost — but
// re-rendering a form 45 times a second while someone types in it is wasteful and can make the
// inputs feel unresponsive.
//
// onCancel/onSubmit are excluded deliberately: both are inline arrows from the parent, so they are
// new on every render and comparing them would defeat the memo. Their behaviour is stable even
// though their identity is not.
export default memo(AdjustPositionForm, (prev, next) => {
  const a = prev.position
  const b = next.position
  return (
    a.ID === b.ID &&
    a.Side === b.Side &&
    a.EntryPx === b.EntryPx &&
    a.Leverage === b.Leverage &&
    a.InstID === b.InstID
  )
})
