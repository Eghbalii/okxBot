// 24-hour time, no AM/PM. Fixed to en-GB rather than the browser locale so the format is
// stable regardless of where the panel is opened from.
const DATE_TIME = new Intl.DateTimeFormat('en-GB', {
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
})

export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return DATE_TIME.format(d)
}

// Money is rounded for display only (the exact NUMERIC value stays in the database, CLAUDE.md §7)
// — a full-precision decimal is unreadable in a table and its extra digits carry no meaning here.
export function formatUsd(value: number, digits = 2): string {
  const sign = value > 0 ? '+' : value < 0 ? '−' : ''
  return `${sign}$${Math.abs(value).toFixed(digits)}`
}

// Green for profit, red for loss, dim for exactly zero / unknown.
export function pnlClass(value: number | null | undefined): string {
  if (value == null || Number.isNaN(value) || value === 0) return 'text-dim'
  return value > 0 ? 'text-green' : 'text-red'
}
