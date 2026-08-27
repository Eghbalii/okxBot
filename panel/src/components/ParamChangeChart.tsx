import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import type { Candle, ParamChange } from '../api/types'

// Plain inline SVG, no charting library (CLAUDE.md §14 Phase 3's "hand-rolled CSS/minimal deps"
// convention, extended here to the strategy parameter optimizer's chart requirement, CLAUDE.md
// §16): a simple price line for instId/bar with a vertical marker line at each parameter-change
// timestamp for the selected strategy. Hovering a marker shows a tooltip diffing old vs new
// param values.

const WIDTH = 900
const HEIGHT = 220
const PAD = { top: 12, right: 12, bottom: 24, left: 56 }

function diffConfig(
  oldConfig: Record<string, number> | null,
  newConfig: Record<string, number>,
): [string, number | undefined, number][] {
  const out: [string, number | undefined, number][] = []
  for (const [k, nv] of Object.entries(newConfig)) {
    const ov = oldConfig?.[k]
    if (ov === undefined || ov !== nv) {
      out.push([k, ov, nv])
    }
  }
  return out
}

export default function ParamChangeChart({
  strategyId,
  instId,
  bar,
}: {
  strategyId: number
  instId: string
  bar: string
}) {
  const [candles, setCandles] = useState<Candle[]>([])
  const [changes, setChanges] = useState<ParamChange[]>([])
  const [hovered, setHovered] = useState<ParamChange | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    async function load() {
      try {
        const [c, pc] = await Promise.all([
          api.candles({ instId, bar, limit: 300 }),
          api.paramChanges(strategyId, { instId }),
        ])
        if (cancelled) return
        setCandles(c)
        setChanges(pc)
        setError(null)
      } catch (err) {
        if (!cancelled) setError((err as Error).message)
      }
    }
    load()
    return () => {
      cancelled = true
    }
  }, [strategyId, instId, bar])

  const { path, xForTime, yForPrice, minPrice, maxPrice, minTime, maxTime } = useMemo(() => {
    if (candles.length === 0) {
      return {
        path: '',
        xForTime: () => PAD.left,
        yForPrice: () => PAD.top,
        minPrice: 0,
        maxPrice: 0,
        minTime: 0,
        maxTime: 0,
      }
    }
    const closes = candles.map((c) => Number(c.Close))
    const times = candles.map((c) => new Date(c.Timestamp).getTime())
    const minP = Math.min(...closes)
    const maxP = Math.max(...closes)
    const minT = Math.min(...times)
    const maxT = Math.max(...times)
    const plotW = WIDTH - PAD.left - PAD.right
    const plotH = HEIGHT - PAD.top - PAD.bottom

    const xf = (t: number) => PAD.left + (maxT === minT ? 0 : ((t - minT) / (maxT - minT)) * plotW)
    const yf = (p: number) => PAD.top + plotH - (maxP === minP ? plotH / 2 : ((p - minP) / (maxP - minP)) * plotH)

    const d = candles
      .map((c, i) => `${i === 0 ? 'M' : 'L'} ${xf(new Date(c.Timestamp).getTime())} ${yf(Number(c.Close))}`)
      .join(' ')

    return { path: d, xForTime: xf, yForPrice: yf, minPrice: minP, maxPrice: maxP, minTime: minT, maxTime: maxT }
  }, [candles])

  if (error) return <div className="error-banner">{error}</div>
  if (candles.length === 0) return <div className="text-dim">No candle history yet for {instId}/{bar}.</div>

  const visibleChanges = changes.filter((c) => {
    const t = new Date(c.CreatedAt).getTime()
    return t >= minTime && t <= maxTime
  })

  return (
    <div style={{ position: 'relative' }}>
      <svg width={WIDTH} height={HEIGHT} className="param-change-chart">
        {/* price line */}
        <path d={path} fill="none" stroke="var(--chart-line, #4ea1ff)" strokeWidth={1.5} />

        {/* y-axis min/max labels */}
        <text x={4} y={yForPrice(maxPrice) + 4} className="chart-axis-label">
          {maxPrice.toFixed(2)}
        </text>
        <text x={4} y={yForPrice(minPrice) + 4} className="chart-axis-label">
          {minPrice.toFixed(2)}
        </text>

        {/* param-change marker lines */}
        {visibleChanges.map((c) => {
          const x = xForTime(new Date(c.CreatedAt).getTime())
          return (
            <g
              key={c.ID}
              onMouseEnter={() => setHovered(c)}
              onMouseLeave={() => setHovered((h) => (h?.ID === c.ID ? null : h))}
              style={{ cursor: 'pointer' }}
            >
              {/* wide invisible hit-area so hovering the thin line is easy */}
              <rect x={x - 4} y={PAD.top} width={8} height={HEIGHT - PAD.top - PAD.bottom} fill="transparent" />
              <line
                x1={x}
                y1={PAD.top}
                x2={x}
                y2={HEIGHT - PAD.bottom}
                stroke={c.Source === 'optimizer' ? 'var(--chart-marker-optimizer, #ffb84e)' : 'var(--chart-marker-manual, #9d7bff)'}
                strokeWidth={hovered?.ID === c.ID ? 2 : 1}
                strokeDasharray="3,2"
              />
            </g>
          )
        })}
      </svg>

      {hovered && (
        <div
          className="param-hover-box"
          style={{
            position: 'absolute',
            left: Math.min(xForTime(new Date(hovered.CreatedAt).getTime()) + 8, WIDTH - 220),
            top: PAD.top,
          }}
        >
          <div style={{ marginBottom: '0.3rem' }}>
            <span className={'badge ' + (hovered.Source === 'optimizer' ? 'badge-green' : 'badge-dim')}>
              {hovered.Source}
            </span>{' '}
            <span className="text-dim">{new Date(hovered.CreatedAt).toLocaleString()}</span>
          </div>
          {diffConfig(hovered.OldConfig, hovered.NewConfig).map(([k, ov, nv]) => (
            <div key={k} className="mono" style={{ fontSize: '0.8rem' }}>
              {k}: {ov ?? '—'} → {nv}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
