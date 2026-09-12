import { useEffect, useMemo, useRef, useState } from 'react'
import type { Position } from '../api/types'
import { pctOnMargin } from './PositionZones'
import { tokenSymbol, trimPrice } from '../utils/format'

export type LevelMode = 'price' | 'pct'

/** Converts a signed margin percentage to the price it lands on. */
export function priceFromPct(entry: number, leverage: number, side: 'buy' | 'sell', pct: number): number {
  const lev = leverage > 0 ? leverage : 1
  const dist = entry * (Math.abs(pct) / 100 / lev)
  const long = side !== 'sell'
  const profit = pct >= 0
  return long === profit ? entry + dist : entry - dist
}

// Enough decimals to express the instrument without rounding a real level away — the roster spans
// BTC at 77295.7 to PEPE at 0.000003382, so this is derived from the entry price rather than fixed.
function decimalsFor(entry: number): number {
  if (!Number.isFinite(entry) || entry === 0) return 2
  if (Math.abs(entry) >= 1) return 2
  const s = entry.toFixed(12).replace(/0+$/, '')
  const lead = /^0\.(0*)/.exec(s)
  return Math.min((lead ? lead[1].length : 0) + 4, 10)
}

const fmtPrice = (v: number, entry: number) => v.toFixed(decimalsFor(entry))

// For an <input type="number">: NO leading "+". The HTML spec's valid-floating-point-number
// production allows an optional "-" and nothing else, so a browser treats "+40.0" as invalid and
// renders the field EMPTY. That is the reported bug — a take-profit (positive) vanished in % mode
// while a stop (negative) displayed fine, because only the positive one carried the plus.
const fmtPctInput = (v: number) => (Math.trunc(v * 10) / 10).toFixed(1)

// For display text, where a sign makes a stop and a target distinguishable at a glance.
export const fmtPctLabel = (v: number) => `${v > 0 ? '+' : ''}${fmtPctInput(v)}`

/**
 * Chart-side SL/TP editor (2026-09-12 request).
 *
 * Differs from AdjustPositionForm (the table's modal) in three ways the operator asked for:
 *  - the fields are PREFILLED with the level currently in force, not left blank, so an edit starts
 *    from where the position actually is rather than from nothing;
 *  - edits are live on the chart but NOT sent — the exchange only sees them on Update;
 *  - price/percent is switchable, and the conversion happens here so the backend still receives the
 *    signed margin percentage it expects (verified to round-trip exactly against its own formula).
 *
 * The parent owns the pending levels as PRICES. Price is the invariant the chart draws and the
 * exchange enforces; percentage is a view of it. Keeping percentage as the source of truth would
 * mean re-deriving the price on every leverage read and re-rounding on each keystroke.
 */
export default function ChartAdjustPanel({
  position,
  sl,
  tp,
  mode,
  onModeChange,
  onChange,
  onSubmit,
  onReset,
  dirty,
  onClosePosition,
  closing,
}: {
  position: Position
  sl: number | null
  tp: number | null
  mode: LevelMode
  onModeChange: (m: LevelMode) => void
  onChange: (which: 'sl' | 'tp', price: number | null) => void
  onSubmit: () => Promise<void>
  onReset: () => void
  dirty: boolean
  onClosePosition: () => void
  closing: boolean
}) {
  const entry = Number(position.EntryPx)
  const leverage = Number(position.Leverage) || 1
  const side = position.Side

  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [saved, setSaved] = useState(false)

  // Raw input text, kept separate from the numeric level so a half-typed value ("-", "0.") is not
  // parsed into a nonsense price and pushed onto the chart mid-keystroke.
  const [slText, setSlText] = useState('')
  const [tpText, setTpText] = useState('')
  // Which field the user is actively typing in — that one is left alone while a drag or a mode
  // switch rewrites the other, so the caret never jumps mid-edit.
  const editing = useRef<'sl' | 'tp' | null>(null)

  const display = useMemo(
    () => (price: number | null) => {
      if (price === null) return ''
      return mode === 'price'
        ? fmtPrice(price, entry)
        : fmtPctInput(pctOnMargin(entry, price, side, leverage))
    },
    [mode, entry, side, leverage],
  )

  // Re-sync the text from the authoritative price whenever it changes from outside (a drag, a mode
  // switch, a reset), except in the field currently being typed in.
  useEffect(() => {
    if (editing.current !== 'sl') setSlText(display(sl))
  }, [sl, display])
  useEffect(() => {
    if (editing.current !== 'tp') setTpText(display(tp))
  }, [tp, display])

  function commit(which: 'sl' | 'tp', text: string) {
    if (text.trim() === '') {
      onChange(which, null)
      return
    }
    const n = Number(text)
    if (!Number.isFinite(n)) return
    onChange(which, mode === 'price' ? n : priceFromPct(entry, leverage, side, n))
  }

  async function handleSubmit() {
    setSubmitting(true)
    setError(null)
    try {
      await onSubmit()
      setSaved(true)
      window.setTimeout(() => setSaved(false), 2500)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  const row = (which: 'sl' | 'tp', label: string, price: number | null, cls: string) => {
    const pct = price === null ? null : pctOnMargin(entry, price, side, leverage)
    return (
      <div className="cae-row">
        <div className="cae-row-head">
          <span className={'cae-dot ' + cls} />
          <span className="cae-label">{label}</span>
          {price !== null && (
            // The other representation always shown beside the input: whichever unit is being
            // typed, the one that is not is the one worth seeing, and switching modes to check a
            // number would lose your place.
            <span className={'cae-alt ' + (pct !== null && pct < 0 ? 'text-red' : 'text-green')}>
              {mode === 'price' ? `${fmtPctLabel(pct ?? 0)}%` : fmtPrice(price, entry)}
            </span>
          )}
        </div>
        <input
          type="number"
          className="cae-input"
          step="any"
          value={which === 'sl' ? slText : tpText}
          placeholder={mode === 'price' ? 'price' : '% of margin'}
          onFocus={() => (editing.current = which)}
          onBlur={() => {
            editing.current = null
            // Re-format on blur so a typed "45.9600" settles to the canonical rendering.
            const p = which === 'sl' ? sl : tp
            if (which === 'sl') setSlText(display(p))
            else setTpText(display(p))
          }}
          onChange={(e) => {
            const v = e.target.value
            if (which === 'sl') setSlText(v)
            else setTpText(v)
            commit(which, v)
          }}
        />
      </div>
    )
  }

  return (
    <aside className="chart-adjust-panel">
      <div className="cae-head">
        <div className="cae-title">
          Position <span className="cae-title-id">#{position.ID}</span>
        </div>
        <div className="cae-sub">
          <span className="cae-sym">{tokenSymbol(position.InstID)}</span>
          <span className={'cae-side ' + (side === 'buy' ? 'cae-side-long' : 'cae-side-short')}>
            {side === 'buy' ? 'LONG' : 'SHORT'}
          </span>
          {/* One decimal (2026-09-12 request): leverage is a decimal column server-side and a
              bare integer hid that — 12.5x and 12x are a real difference at this account size. */}
          <span className="cae-lev">{leverage.toFixed(1)}x</span>
        </div>
      </div>

      <div className="cae-entry">
        <span className="text-dim">Entry</span>
        <span className="mono">{trimPrice(position.EntryPx)}</span>
      </div>

      <div className="cae-mode" role="group" aria-label="Level unit">
        {(['price', 'pct'] as const).map((m) => (
          <button
            key={m}
            className={'cae-mode-btn' + (mode === m ? ' active' : '')}
            onClick={() => onModeChange(m)}
            type="button"
          >
            {m === 'price' ? 'Price' : '% of margin'}
          </button>
        ))}
      </div>

      {row('tp', 'Take profit', tp, 'cae-dot-green')}
      {row('sl', 'Stop loss', sl, 'cae-dot-red')}

      {/* One hint for both modes (2026-09-12 request). The % variant used to append an
          explanation of margin-vs-price, which made the panel tall enough to scroll — the margin
          point is already carried by the "% of margin" button label and the per-row conversion
          shown beside each input, so it was costing layout to repeat something visible twice. */}
      <p className="cae-hint">
        Drag a level on the chart, or type here. Nothing reaches the exchange until you press
        Update.
      </p>

      <div className="cae-spacer" />

      {error && <p className="error cae-error">{error}</p>}
      {saved && !dirty && <p className="cae-saved">Sent to the exchange.</p>}

      {/* Update/Reset share a row; Close sits beneath spanning both (2026-09-12 request). The two
          rows also separate the reversible actions from the irreversible one without needing a
          divider. */}
      <div className="cae-actions">
        <button className="btn-primary" onClick={handleSubmit} disabled={!dirty || submitting}>
          {submitting ? 'Updating…' : 'Update'}
        </button>
        <button onClick={onReset} disabled={!dirty || submitting} type="button">
          Reset
        </button>
      </div>
      <button
        className="btn-danger cae-close-wide"
        onClick={onClosePosition}
        disabled={closing || submitting}
        type="button"
        title="Close this position now at the live price (close_reason='manual')"
      >
        {closing ? 'Closing…' : 'Close position'}
      </button>
    </aside>
  )
}
