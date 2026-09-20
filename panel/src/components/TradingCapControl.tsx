import { useState } from 'react'
import type { PositionMode } from '../api/types'

// Trading-cap control: "I've decided to trade with $X from now on." What that means depends on
// the mode, because bot/manual disagree with paper about what Account Balance IS:
//
//   paper         — no exchange exists, so Account Balance is bookkeeping this system owns and a
//                   cap moves BOTH numbers to the chosen value together (CLAUDE.md §31.2/§31.3).
//   bot / manual  — Account Balance mirrors the exchange's own reported total and must never be
//                   overwritten with a chosen number (it is what realized PnL is reconciled
//                   against). The cap names the tradable SLICE of it instead; the remainder
//                   becomes the Reserve figure, and profit/loss accrues to the slice while the
//                   reserve stays put. Bot and manual share the SAME real exchange balance
//                   (2026-09-20 Account page) — the backend bounds each cap against what the
//                   OTHER mode has already claimed.
//
// CONTROLLED, not self-contained (2026-09-20 redesign, direct request): the draft value now lives
// in AccountPage, not here, so bot's slider, manual's slider, and the donut chart can all move
// together the instant either slider is dragged — before either is actually saved. This control is
// now "dumb": it renders whatever draft/onDraftChange it's given and calls onSave on click. Every
// other line of reasoning below (why the slider is bounded and the input is too) is unchanged.
//
// The number input is now BOUNDED to [0, maxAvailable] and clamped as the user types (2026-09-20
// request: "handle the error so it can't enter more than what's allowed") — this reverses the
// original design's "the input is the escape hatch, the backend is the authority" stance. That
// stance assumed a slider ceiling was just a convenience; the two-mode sync this control now
// participates in means an out-of-range draft would show the OTHER mode's slider/max and the donut
// a number the backend could never actually honor, which is a worse experience than just not
// allowing it client-side. The backend's own bound (SetTradingCap's sibling-aware LEAST/GREATEST)
// is still the actual authority and is re-validated on save regardless.
export default function TradingCapControl({
  mode,
  label,
  maxAvailable,
  draft,
  onDraftChange,
  onSave,
}: {
  mode: PositionMode
  label: string
  maxAvailable: number
  draft: number
  onDraftChange: (v: number) => void
  onSave: (v: number) => Promise<void>
}) {
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const sliderMax = maxAvailable > 0 ? maxAvailable : 0
  const clamped = Math.min(Math.max(draft, 0), sliderMax)
  const step = sliderMax >= 100 ? 1 : sliderMax >= 10 ? 0.1 : 0.01

  function setClamped(raw: number) {
    if (!Number.isFinite(raw)) {
      onDraftChange(0)
      return
    }
    onDraftChange(Math.min(Math.max(raw, 0), sliderMax))
  }

  async function save() {
    if (!Number.isFinite(clamped) || clamped <= 0) {
      setError('enter a positive number')
      return
    }
    setSaving(true)
    setError(null)
    try {
      await onSave(clamped)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="stat-tile cap-tile">
      <div className="stat-label">{label}</div>
      <div className="cap-row">
        <input
          type="range"
          min={0}
          max={sliderMax}
          step={step}
          value={clamped}
          onChange={(e) => setClamped(Number(e.target.value))}
          className="cap-slider"
          title={`Max available: $${sliderMax.toFixed(2)}`}
        />
        <input
          type="number"
          placeholder="e.g. 40"
          min={0}
          max={sliderMax}
          value={Number.isFinite(draft) ? draft : ''}
          onChange={(e) => {
            const raw = e.target.value === '' ? NaN : Number(e.target.value)
            setClamped(raw)
          }}
          className="cap-input"
        />
        <button className="btn-primary" onClick={save} disabled={saving}>
          {saving ? '…' : 'Set'}
        </button>
      </div>
      <div className="text-dim cap-hint">
        {mode === 'paper' ? `max $${sliderMax.toFixed(2)}` : `of $${sliderMax.toFixed(2)} available on the exchange`}
      </div>
      {error && <div className="cap-error">{error}</div>}
    </div>
  )
}
