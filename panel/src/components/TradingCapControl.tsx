import { useEffect, useState } from 'react'
import { api } from '../api/client'
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
//                   OTHER mode has already claimed, so this control's own maxAvailable prop is
//                   the caller's job to compute correctly (see AccountPage's own reasoning).
//
// Moved here from PaperTradingStatsBox (2026-09-20 request): capital allocation is a decision
// about the whole account, not something that belongs buried in a per-mode positions-page stats
// box — it now lives on its own Account page, used once for bot and once for manual.
//
// The slider's upper bound is the account's REAL Account Balance (or less, once a sibling mode's
// own claim is accounted for), not an arbitrary ceiling: a cap above what's actually available
// would be claiming a deposit the panel cannot make. The direct number input is deliberately kept
// and left UNBOUNDED — an operator who really has deposited more on the exchange needs to be able
// to say so, and the backend is the authority on what is valid, not this control. The slider is
// the convenience, the input is the escape hatch.
export default function TradingCapControl({
  mode,
  label,
  maxAvailable,
  current,
  onSaved,
}: {
  mode: PositionMode
  label: string
  maxAvailable: number
  current: number
  onSaved: () => void
}) {
  const [value, setValue] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Seed the control from the account's own current balance so the slider starts somewhere
  // meaningful rather than at zero, and re-seed if the account moves while the field is untouched.
  useEffect(() => {
    if (value === '' && current > 0) setValue(current.toFixed(2))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [current])

  const num = Number(value)
  // The slider can only express [0, maxAvailable]; a typed value above that is still valid (see
  // the note above), it just pins the thumb to the far end rather than rescaling the track.
  const sliderMax = maxAvailable > 0 ? maxAvailable : 100
  const sliderValue = Number.isFinite(num) ? Math.min(Math.max(num, 0), sliderMax) : 0
  const step = sliderMax >= 100 ? 1 : sliderMax >= 10 ? 0.1 : 0.01

  async function save() {
    if (!Number.isFinite(num) || num <= 0) {
      setError('enter a positive number')
      return
    }
    setSaving(true)
    setError(null)
    try {
      await api.setAccountCap(String(num), mode)
      onSaved()
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
          value={sliderValue}
          onChange={(e) => setValue(e.target.value)}
          className="cap-slider"
          title={`Max available: $${sliderMax.toFixed(2)}`}
        />
        <input
          type="number"
          placeholder="e.g. 40"
          value={value}
          onChange={(e) => setValue(e.target.value)}
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
