// Plain inline SVG, no charting library — matches this project's own established convention
// (BalanceChart.tsx's own comment: "the same hand-rolled CSS/minimal-deps pattern used elsewhere").
//
// One exchange's real balance, split into named slices (today: Bot Trader, Manual trading,
// Reserve/unallocated). Built as a generic ring rather than hardcoding "bot vs manual" so a third
// trading surface later is one more slice, not a rewrite.
export interface DonutSlice {
  label: string
  usd: number
  color: string
}

function polarToXY(cx: number, cy: number, r: number, angleDeg: number) {
  const rad = ((angleDeg - 90) * Math.PI) / 180
  return { x: cx + r * Math.cos(rad), y: cy + r * Math.sin(rad) }
}

function arcPath(cx: number, cy: number, r: number, startDeg: number, endDeg: number): string {
  const start = polarToXY(cx, cy, r, startDeg)
  const end = polarToXY(cx, cy, r, endDeg)
  const largeArc = endDeg - startDeg > 180 ? 1 : 0
  return `M ${cx} ${cy} L ${start.x} ${start.y} A ${r} ${r} 0 ${largeArc} 1 ${end.x} ${end.y} Z`
}

function fmtUsd(n: number): string {
  if (!Number.isFinite(n)) return '—'
  return `$${n.toFixed(2)}`
}

export default function CapitalDonut({
  slices,
  totalUsd,
  centerLabel,
}: {
  slices: DonutSlice[]
  totalUsd: number
  centerLabel: string
}) {
  // Shrunk from 180/78 (2026-09-21 request: "خیلی الکی بزرگن") — a donut this size next to a pair
  // of compact slider tiles looked oversized for what it shows.
  const size = 132
  const cx = size / 2
  const cy = size / 2
  const r = 56
  const total = totalUsd > 0 ? totalUsd : 1 // avoid /0; an all-zero account renders as one full-circle "unallocated" wedge below

  let cursor = 0
  const arcs = slices
    .filter((s) => s.usd > 0)
    .map((s) => {
      const startDeg = (cursor / total) * 360
      cursor += s.usd
      const endDeg = (cursor / total) * 360
      // A single slice spanning the whole circle (100%) has coincident start/end points, which
      // degenerates the arc path to nothing — nudge it a hair short of 360 so it still renders.
      const clampedEnd = endDeg - startDeg >= 359.99 ? startDeg + 359.98 : endDeg
      return { ...s, path: arcPath(cx, cy, r, startDeg, clampedEnd) }
    })

  return (
    <div className="capital-donut">
      <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} className="capital-donut-svg">
        {totalUsd <= 0 ? (
          <circle cx={cx} cy={cy} r={r} fill="none" stroke="var(--border)" strokeWidth={22} />
        ) : (
          arcs.map((a) => (
            <path key={a.label} d={a.path} fill={a.color} stroke="var(--bg-panel)" strokeWidth={2}>
              <title>
                {a.label}: {fmtUsd(a.usd)}
              </title>
            </path>
          ))
        )}
        {/* Inner hole, punched out with the panel's own background so the ring reads as a donut
            rather than a filled pie — cheaper and simpler than a mask for a solid background. */}
        <circle cx={cx} cy={cy} r={r * 0.72} fill="var(--bg-panel)" />
        <text x={cx} y={cy - 6} textAnchor="middle" className="capital-donut-total">
          {fmtUsd(totalUsd)}
        </text>
        <text x={cx} y={cy + 14} textAnchor="middle" className="capital-donut-caption">
          {centerLabel}
        </text>
      </svg>
      <div className="capital-donut-legend">
        {slices.map((s) => {
          const pct = totalUsd > 0 ? (s.usd / totalUsd) * 100 : 0
          return (
            <div key={s.label} className="capital-donut-legend-row">
              <span className="capital-donut-swatch" style={{ background: s.color }} />
              <span className="capital-donut-legend-label">{s.label}</span>
              <span className="capital-donut-legend-value">
                {fmtUsd(s.usd)} <span className="text-dim">({pct.toFixed(0)}%)</span>
              </span>
            </div>
          )
        })}
      </div>
    </div>
  )
}
