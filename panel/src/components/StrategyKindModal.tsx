import { useEffect, useState } from 'react'
import { api } from '../api/client'

// The 12 scalp/ICT/price-action kinds added CLAUDE.md §30, tagged "new" in this list so they are
// easy to spot among the original 14 while deciding which to enable — pure display, no backend
// concept of "new" exists (a strategy row carries no added_at/version marker for this).
const NEW_KINDS = new Set([
  'vwap_reversion',
  'bb_squeeze_breakout',
  'range_breakout',
  'keltner_trend_scalp',
  'ict_fvg',
  'ict_order_block',
  'ict_liquidity_sweep',
  'engulfing_reversal',
  'inside_bar_breakout',
  'macd_momentum',
  'volume_breakout',
  'ema_ribbon_pullback',
])

// Global per-strategy-KIND on/off switch for paper trading: applied uniformly across every token,
// bulk-toggling strategy_assignments.enabled for every assignment of that kind — distinct from the
// existing per-token/per-timeframe assignment granularity on the Strategies page, which stays the
// underlying mechanism this toggle drives. Disabling a kind only stops it from opening NEW
// positions; any of its already-open positions keep running to their normal SL/TP/timeout close.
//
// Checkbox semantics: the saved activeKinds list is empty by default, meaning "no restriction —
// every kind currently enabled in strategy_assignments applies as-is." An empty list must render
// as every kind CHECKED (they're all effectively active), not as every box empty — the modal
// previously got this backwards, which read as "everything is disabled" when the opposite was
// true. Unchecking is what actually creates a restriction; checking everything back is equivalent
// to clearing it, handled below by treating "all checked" as saving an empty list.
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
        // No saved restriction (activeKinds empty) means every kind is effectively active —
        // pre-check all of them rather than leaving the list looking fully disabled.
        setSelected((prev) => (activeKinds.length === 0 ? new Set(kinds) : prev))
      })
      .catch((err) => setError((err as Error).message))
      .finally(() => setLoading(false))
    // eslint-disable-next-line react-hooks/exhaustive-deps
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
      // Every kind checked is equivalent to "no restriction" — save an empty list rather than an
      // explicit list of all 14, so a future 15th kind isn't silently excluded by a stale snapshot.
      const allChecked = allKinds.length > 0 && allKinds.every((k) => selected.has(k))
      await onSave(allChecked ? [] : [...selected].sort())
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
          kind stops it from opening new positions — any of its existing open positions keep
          running to their normal close, nothing is force-closed.
        </div>

        {error && <div className="error-banner">{error}</div>}
        {loading && <div className="text-dim">Loading…</div>}

        {!loading && (
          <div className="checkbox-grid">
            {allKinds.map((kind) => (
              <label key={kind} className="checkbox-row">
                <input type="checkbox" checked={selected.has(kind)} onChange={() => toggle(kind)} />
                <span className="mono">{kind}</span>
                {NEW_KINDS.has(kind) && (
                  <span className="badge badge-green" style={{ marginLeft: 6 }}>
                    new
                  </span>
                )}
              </label>
            ))}
          </div>
        )}

        <div className="toolbar" style={{ marginTop: '1rem' }}>
          <button className="btn-primary" onClick={save} disabled={saving || loading}>
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
