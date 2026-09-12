import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import type { PositionMode } from '../api/types'

const MODES: { value: PositionMode; label: string; hint: string }[] = [
  { value: 'paper', label: 'Paper', hint: 'Simulated — no orders reach the exchange' },
  { value: 'real', label: 'Real', hint: 'Live money — orders execute on OKX' },
]

/**
 * Paper/Real selector for the header.
 *
 * A hand-rolled menu rather than a native <select> (2026-09-12): a select's OPEN list is drawn by
 * the operating system, so no amount of CSS on the closed control reaches it — it rendered as a
 * grey system list against this dark UI, which is what read as dated. The trigger is a button and
 * the list is ordinary markup, so both halves can be styled as one piece.
 *
 * The colour treatment is carried by a small dot rather than by tinting the whole control. Real
 * money does need a standing marker, but a red-on-red button reads as an error state, and it sat
 * in the header on every page — a persistent alarm for something that is simply a mode.
 */
export default function ModeSelect({ mode }: { mode: PositionMode }) {
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)
  const wrapRef = useRef<HTMLDivElement>(null)

  // Close on an outside click or Escape — a menu that can only be dismissed by choosing something
  // traps the pointer, and this one sits in the header on every page.
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (!wrapRef.current?.contains(e.target as Node)) setOpen(false)
    }
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDown)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const current = MODES.find((m) => m.value === mode) ?? MODES[0]

  return (
    <div className="mode-select" ref={wrapRef}>
      <button
        type="button"
        className={'mode-trigger' + (open ? ' open' : '')}
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="listbox"
        aria-expanded={open}
        title={current.hint}
      >
        <span className={'mode-dot mode-dot-' + current.value} />
        <span className="mode-trigger-label">{current.label}</span>
        <svg className="mode-caret" width="10" height="10" viewBox="0 0 12 12" aria-hidden="true">
          <path fill="currentColor" d="M2.5 4.5 6 8l3.5-3.5z" />
        </svg>
      </button>

      {open && (
        <div className="mode-menu" role="listbox">
          {MODES.map((m) => (
            <button
              key={m.value}
              type="button"
              role="option"
              aria-selected={m.value === mode}
              className={'mode-option' + (m.value === mode ? ' active' : '')}
              onClick={() => {
                setOpen(false)
                if (m.value !== mode) navigate(`/positions/${m.value}`)
              }}
            >
              <span className={'mode-dot mode-dot-' + m.value} />
              <span className="mode-option-text">
                <span className="mode-option-label">{m.label}</span>
                <span className="mode-option-hint">{m.hint}</span>
              </span>
              {m.value === mode && (
                <svg className="mode-check" width="12" height="12" viewBox="0 0 16 16" aria-hidden="true">
                  <path
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="2"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    d="M3 8.5 6.5 12 13 4.5"
                  />
                </svg>
              )}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
