// 24-hour time, no AM/PM. Fixed to en-GB rather than the browser locale so the format is
// stable regardless of where the panel is opened from.
const DATE_ONLY = new Intl.DateTimeFormat('en-GB', {
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
})
const TIME_ONLY = new Intl.DateTimeFormat('en-GB', {
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
})

export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return `${DATE_ONLY.format(d)}, ${TIME_ONLY.format(d)}`
}

// Same value as formatDateTime, split into its date/time halves for a table cell that renders
// them on two lines (a single "03/09/2026, 05:40:11" line was too wide for the Opened/Closed
// columns and got line-wrapped unpredictably depending on the column's actual rendered width).
export function formatDateTimeLines(iso: string | null | undefined): { date: string; time: string } {
  if (!iso) return { date: '—', time: '' }
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return { date: '—', time: '' }
  return { date: DATE_ONLY.format(d), time: TIME_ONLY.format(d) }
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

// Wins/Losses come from the backend as "closed with positive realized PnL" vs. non-positive
// (CLAUDE.md §11.3) — not close_reason='tp'/'sl', which under the RL ratchet reported a
// profitable strategy as 0%. Open trades are excluded: their outcome isn't decided yet. Shared by
// StrategiesPage and StrategyKindModal (2026-09-04) so both read the same definition.
export function winRate(wins: number | undefined, losses: number | undefined): string {
  const decided = (wins ?? 0) + (losses ?? 0)
  if (decided === 0) return '—'
  return `${Math.round(((wins ?? 0) / decided) * 100)}% (${wins}/${decided})`
}

// Shortens an OKX instId (e.g. "BTC-USDT-SWAP") to just its base token ("BTC") for display —
// every instrument this project trades is a USDT-settled perpetual swap (CLAUDE.md's own trading
// config), so the "-USDT-SWAP" suffix is constant noise, never information. Display-only: the
// full instId is still what's sent to/received from the backend everywhere else. Anything that
// doesn't match the expected shape (a future non-USDT/non-SWAP instrument, or a malformed value)
// is returned unchanged rather than mangled, so an unexpected instId is still readable.
export function tokenSymbol(instId: string | null | undefined): string {
  if (!instId) return '—'
  return instId.endsWith('-USDT-SWAP') ? instId.slice(0, -'-USDT-SWAP'.length) : instId
}

// Trims a price/size value for display (2026-09-04 request) — the exact NUMERIC value from
// Postgres (CLAUDE.md §7) is untouched, this never reaches the backend, purely cosmetic rounding
// of numbers that were otherwise showing far more precision than is readable.
//
// Two regimes, split at magnitude 1:
//   >= 1: at most 4 digits after the decimal point (e.g. 81234.56789 -> "81234.5679").
//   < 1:  at most 4 SIGNIFICANT digits counted from the first non-zero digit, not 4 decimal
//         places flat — many tokens here trade at sub-cent prices (e.g. 0.000003624), where a
//         flat 4dp round would collapse the entire value to "0.0000" and destroy the only digits
//         that actually distinguish one price from another. 0.000003624 -> "0.000003624" trimmed
//         to its first 4 significant digits -> "0.000003624" -> "0.0000036".
export function trimPrice(value: string | number | null | undefined): string {
  if (value == null) return '—'
  const n = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(n)) return String(value)
  if (n === 0) return '0'
  if (Math.abs(n) >= 1) {
    // toFixed(4) then strip trailing zeros, so an integer-like value doesn't sprout ".0000".
    return n.toFixed(4).replace(/\.?0+$/, '') || '0'
  }
  // toPrecision(4) on a sub-1 value gives 4 significant digits total; since the leading digit is
  // always "0" for |n| < 1, this reads as 4 significant digits after the decimal point's leading
  // zeros — exactly "4 digits after the first non-zero digit" for values with several leading
  // zeros (0.000003624 -> "0.000003624"), and plain 4-significant-digit rounding otherwise
  // (0.123456 -> "0.1235"). Strips trailing zeros the same way the >=1 branch does.
  const fixed = n.toPrecision(4)
  return fixed.includes('.') ? fixed.replace(/0+$/, '').replace(/\.$/, '') : fixed
}
