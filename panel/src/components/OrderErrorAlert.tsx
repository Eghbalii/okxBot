import { useEffect, useState } from 'react'
import type { Position } from '../api/types'
import { formatDateTime, tokenSymbol } from '../utils/format'

// Raises a real order's most recent exchange failure to whoever is watching the panel, as a modal
// they must dismiss (2026-09-08 request). A failed open or a failed close is not something to
// leave in a log: a close that did not go through means a real position may still be live on the
// exchange while the engine has stopped trying, and only a person can decide what to do about it.
//
// Dismissal is per (order, error timestamp) and kept in component state rather than persisted:
// re-showing on a page reload is the safer default for an unresolved problem, and a NEW failure on
// an already-dismissed order raises the dialog again because its timestamp differs.
export default function OrderErrorAlert({ positions }: { positions: Position[] }) {
  const [dismissed, setDismissed] = useState<Set<string>>(new Set())

  const failing = positions.filter((p) => p.LastError)
  const key = (p: Position) => `${p.ID}:${p.LastErrorAt ?? ''}`
  const pending = failing.filter((p) => !dismissed.has(key(p)))

  // A browser notification too, so a failure is noticed even when the panel is not the focused tab
  // — the same reasoning as the existing open/SL/TP alerts.
  useEffect(() => {
    if (pending.length === 0) return
    if (typeof Notification === 'undefined' || Notification.permission !== 'granted') return
    const p = pending[0]
    new Notification(`Order #${p.ID} ${tokenSymbol(p.InstID)} failed`, {
      body: p.LastError ?? '',
      tag: key(p),
    })
    // Only re-fires when the set of undismissed failures changes, not on every poll.
  }, [pending.map(key).join(',')]) // eslint-disable-line react-hooks/exhaustive-deps

  if (pending.length === 0) return null
  const p = pending[0]

  return (
    <div className="modal-backdrop">
      <div className="modal modal-narrow order-error-modal">
        <div className="modal-header">
          <h2>
            <span className="badge badge-red">exchange error</span>{' '}
            Order #{p.ID} — <span title={p.InstID}>{tokenSymbol(p.InstID)}</span>
          </h2>
        </div>

        <p className="text-dim adjust-help">
          {p.Status === 'closing' ? (
            <>
              This position's <strong>close did not complete</strong>. It may still be open on the
              exchange — check OKX directly and close it by hand if so.
            </>
          ) : (
            <>
              This order failed at the exchange. The position may not exist as recorded — check OKX
              directly before acting on it.
            </>
          )}
        </p>

        <div className="order-error-detail mono">{p.LastError}</div>

        <div className="text-dim order-error-meta">
          <div>status: {p.Status ?? '—'}</div>
          <div>side: {p.Side} · entry: {p.EntryPx}</div>
          {p.ExchangeOrderID && <div>open order: {p.ExchangeOrderID}</div>}
          {p.ExchangeCloseOrderID && <div>close order: {p.ExchangeCloseOrderID}</div>}
          <div>at: {formatDateTime(p.LastErrorAt)}</div>
        </div>

        <div className="adjust-actions">
          <button
            className="btn-primary"
            onClick={() => setDismissed((d) => new Set(d).add(key(p)))}
          >
            Dismiss{pending.length > 1 ? ` (${pending.length - 1} more)` : ''}
          </button>
        </div>
      </div>
    </div>
  )
}
