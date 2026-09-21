import { useEffect, useRef, useState } from 'react'
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
// now "dumb": it renders whatever draft/onDraftChange it's given and calls onSave on click.
//
// The number input is BOUNDED to [0, maxAvailable] and clamped as the user types (2026-09-20
// request: "handle the error so it can't enter more than what's allowed"). The backend's own bound
// (SetTradingCap's sibling-aware LEAST/GREATEST) is still the actual authority and is re-validated
// on save regardless; this is purely about not showing a number the backend could never honor.
//
// Three bugs fixed here (2026-09-20 follow-up, all frontend-only):
//   1. Typing a value above the ceiling (e.g. 50 when the max is 40.11963720756992) silently
//      became that exact raw ceiling, unrounded — clamping is correct, displaying 13 decimal
//      places of a float is not. `round()` (1dp) is applied everywhere a number reaches the draft.
//   2. The number input was a plain controlled `<input value={draft}>` with a `useEffect` that
//      re-synced its own local text from `draft` on EVERY change — including changes this same
//      input had just caused itself. Clearing the field set text="" and draft=0; the next render's
//      effect saw draft change to 0 and immediately reset text back to a number, so a field cleared
//      and then typed into (e.g. "0" then "2" then "3") had its own first keystroke erased before
//      the second could land. Fixed by tracking the LAST VALUE THIS TEXT FIELD ITSELF PUSHED
//      (`lastPushedFromText`) and only SKIPPING the resync when the incoming draft matches that —
//      i.e. only when the change is an echo of this field's own last edit. A slider drag is a
//      DIFFERENT origin (its handler never touches `lastPushedFromText`), so it always resyncs the
//      text field — otherwise dragging the slider would silently leave the number field stale,
//      which is exactly the regression a first version of this fix introduced by having the
//      slider's own handler also set the "last pushed" marker.
//   3. save() rejected a value of exactly 0 as "not a positive number" — but 0 is a legitimate,
//      explicit choice ("give this mode nothing"), not an error. Only NaN/negative is now rejected,
//      matching the same relaxation made server-side in handleSetAccountCap.
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

  const ROUND_DP = 1
  function round(n: number): number {
    return Math.round(n * 10 ** ROUND_DP) / 10 ** ROUND_DP
  }

  const sliderMax = round(maxAvailable > 0 ? maxAvailable : 0)
  const clamped = round(Math.min(Math.max(draft, 0), sliderMax))
  const step = sliderMax >= 100 ? 1 : sliderMax >= 10 ? 0.1 : 0.01

  function clampToRange(raw: number): number {
    if (!Number.isFinite(raw)) return 0
    return round(Math.min(Math.max(raw, 0), sliderMax))
  }

  // Slider drags push straight through — no "last pushed" bookkeeping, so the resync effect below
  // always treats a slider move as an external change and updates the text field to match.
  function onSlider(raw: number) {
    onDraftChange(clampToRange(raw))
  }

  // The input's own literal text, separate from the numeric `draft` it feeds — lets the field hold
  // "" or a leading-zero-in-progress edit ("0", then "02", then "023") without a controlled value
  // fighting back on every keystroke.
  const [text, setText] = useState(() => (draft > 0 ? String(clamped) : ''))
  // The value the TEXT FIELD itself last pushed upward — used to tell "draft changed because I just
  // typed something" apart from every other kind of draft change (a slider drag, the sibling
  // control's save reshaping this one's ceiling), which must still resync `text`.
  const lastPushedFromText = useRef<number | null>(null)
  useEffect(() => {
    if (lastPushedFromText.current !== null && round(draft) === lastPushedFromText.current) return
    setText(draft > 0 ? String(round(draft)) : draft === 0 ? '' : String(draft))
    lastPushedFromText.current = null
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft])

  function onTextChange(raw: string) {
    setText(raw)
    if (raw === '') {
      lastPushedFromText.current = 0
      onDraftChange(0)
      return
    }
    const num = Number(raw)
    if (Number.isFinite(num)) {
      const v = clampToRange(num)
      lastPushedFromText.current = v
      onDraftChange(v)
    }
  }

  async function save() {
    // 0 is a legitimate, explicit cap ("give this mode nothing") — only NaN/negative is rejected.
    if (!Number.isFinite(clamped) || clamped < 0) {
      setError('enter a number, 0 or more')
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
          onChange={(e) => onSlider(Number(e.target.value))}
          className="cap-slider"
          title={`Max available: $${sliderMax.toFixed(1)}`}
        />
        <input
          type="number"
          placeholder="e.g. 40"
          min={0}
          max={sliderMax}
          value={text}
          onChange={(e) => onTextChange(e.target.value)}
          className="cap-input"
        />
        <button className="btn-primary" onClick={save} disabled={saving}>
          {saving ? '…' : 'Set'}
        </button>
      </div>
      <div className="text-dim cap-hint">
        {mode === 'paper' ? `max $${sliderMax.toFixed(1)}` : `of $${sliderMax.toFixed(1)} available on the exchange`}
      </div>
      {error && <div className="cap-error">{error}</div>}
    </div>
  )
}
