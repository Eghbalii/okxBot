import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import type { EquityPoint, PositionMode } from '../api/types'

// Plain inline SVG, no charting library — the same "hand-rolled CSS/minimal deps" convention
// ParamChangeChart already follows (CLAUDE.md §14 Phase 3). Two lines over one timeline: Total
// Equity (what new positions size against) and Account Balance (the real continuous total, which
// diverges from equity only after a reset/cap change, CLAUDE.md §31.2).
//
// account_equity_history carries EquityUSD per row but NOT the balance — balance is reconstructed
// by walking the deltas forward from the window's own starting point, which is exact because both
// numbers move by the identical realized-PnL delta on every trade, and differ only at a "reset"
// row (where equity jumps to the chosen baseline and balance does not, or vice versa for an
// operator cap change). A reset inside the window is drawn as a marker so a jump is explained
// rather than looking like a data glitch.

const HEIGHT = 180
const PAD = { top: 12, right: 12, bottom: 22, left: 52 }

type Range = 'day' | 'week' | 'month' | 'year'

const RANGE_DAYS: Record<Range, number> = { day: 1, week: 7, month: 30, year: 365 }
const RANGES: Range[] = ['day', 'week', 'month', 'year']

// One history row is written per balance change, i.e. per closed trade — measured at ~200/day on
// the live paper account, so a year window is tens of thousands of rows. That is a wasteful
// payload and an unreadable SVG path (far more points than the chart has horizontal pixels), so
// the series is thinned to a fixed budget before drawing. Thinning happens AFTER the balance walk
// so dropped rows still contribute their delta — only the drawn resolution is reduced, never the
// arithmetic.
const MAX_PLOT_POINTS = 400

// Keeps at most `budget` evenly-spaced entries, always including the first and last so the
// window's own endpoints are never invented by the thinning.
function thin<T>(values: T[], budget: number): T[] {
  if (values.length <= budget) return values
  const stride = (values.length - 1) / (budget - 1)
  const out: T[] = []
  for (let i = 0; i < budget; i++) out.push(values[Math.round(i * stride)])
  return out
}

// A "day"/"week" window wants time of day; a "month"/"year" window wants the date.
function tickLabel(ms: number, range: Range): string {
  const d = new Date(ms)
  if (range === 'day') return d.toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit', hour12: false })
  if (range === 'week') return d.toLocaleDateString('en-GB', { weekday: 'short' })
  return d.toLocaleDateString('en-GB', { day: '2-digit', month: 'short' })
}

export default function BalanceChart({
  mode,
  currentEquity,
  currentBalance,
  refreshSignal,
}: {
  mode: PositionMode
  currentEquity: number
  currentBalance: number
  refreshSignal: number
}) {
  const [range, setRange] = useState<Range>('week')
  const [points, setPoints] = useState<EquityPoint[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    const since = new Date(Date.now() - RANGE_DAYS[range] * 24 * 60 * 60 * 1000)
    api
      .accountHistory(mode, since)
      .then((p) => {
        if (cancelled) return
        setPoints(p)
        setError(null)
      })
      .catch((err) => {
        if (!cancelled) setError((err as Error).message)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [mode, range, refreshSignal])

  const series = useMemo<Series | null>(() => {
    if (points.length === 0) return null

    // Reconstruct the balance series. account_equity_history stores EquityUSD per row but not the
    // balance, so balance is derived by anchoring on the CURRENT balance (known exactly) and
    // walking BACKWARD — the right-hand edge then always agrees with the tiles above the chart,
    // where a forward walk from an unknown starting balance would drift.
    //
    // The subtlety is the "reset" rows, of which there are two kinds writing the SAME reason:
    //   - an automatic drain-to-zero top-up (ApplyRealizedPnL, CLAUDE.md §15.7) moves equity ONLY
    //     — the real balance stays drained, which is exactly the divergence the two lines exist
    //     to show;
    //   - an operator cap change (SetAccountCap, CLAUDE.md §32.3) moves equity AND balance to the
    //     same new value together.
    // Nothing in the row distinguishes them, but the second kind is identifiable by its result:
    // it sets balance := equity, so at a cap change the balance simply IS the row's EquityUSD.
    // Walking backward, a reset row therefore RESTARTS the balance walk from that row's equity
    // rather than continuing to subtract deltas through it — which is correct for a cap change,
    // and for an auto-reset leaves the pre-reset balance reading as the drained equity it was
    // topped up from. Guessing the other way (assuming no reset moves balance) was measurably
    // wrong against real server data: it put balance $32.70 ABOVE equity at a window's start, a
    // state no real trade ever produced.
    const equities = points.map((p) => Number(p.EquityUSD))
    const balances = new Array<number>(points.length)
    let bal = currentBalance
    for (let i = points.length - 1; i >= 0; i--) {
      balances[i] = bal
      if (points[i].Reason === 'reset') {
        bal = equities[i] - Number(points[i].DeltaUSD)
      } else {
        bal -= Number(points[i].DeltaUSD)
      }
    }

    // Thin as one zipped series so the three arrays stay index-aligned.
    const zipped = thin(
      points.map((p, i) => ({ t: new Date(p.CreatedAt).getTime(), e: equities[i], b: balances[i] })),
      MAX_PLOT_POINTS,
    )
    const times = zipped.map((z) => z.t)
    const eqs = zipped.map((z) => z.e)
    const bals = zipped.map((z) => z.b)

    // Extend to "now" with the live values so a quiet account doesn't render as a line that
    // stops hours ago.
    times.push(Date.now())
    eqs.push(currentEquity)
    bals.push(currentBalance)

    const all = [...eqs, ...bals].filter(Number.isFinite)
    let minV = Math.min(...all)
    let maxV = Math.max(...all)
    if (minV === maxV) {
      // A perfectly flat account would otherwise divide by zero; give it a visible band.
      minV -= 1
      maxV += 1
    }
    const pad = (maxV - minV) * 0.08
    minV -= pad
    maxV += pad

    const minT = Math.min(...times)
    const maxT = Math.max(...times)
    return { times, equities: eqs, balances: bals, minV, maxV, minT, maxT }
  }, [points, currentEquity, currentBalance])

  const selector = (
    <div className="chart-range-tabs">
      {RANGES.map((r) => (
        <button
          key={r}
          className={'chart-range-tab' + (r === range ? ' active' : '')}
          onClick={() => setRange(r)}
        >
          {r}
        </button>
      ))}
    </div>
  )

  return (
    <div className="balance-chart">
      <div className="balance-chart-header">
        <div className="balance-chart-legend">
          <span>
            <i className="legend-swatch" style={{ background: 'var(--chart-equity, #4ea1ff)' }} /> Total Equity
          </span>
          <span>
            <i className="legend-swatch" style={{ background: 'var(--chart-balance, #9d7bff)' }} /> Account Balance
          </span>
        </div>
        {selector}
      </div>
      {error && <div className="error-banner">{error}</div>}
      {!error && loading && <div className="text-dim">Loading…</div>}
      {!error && !loading && !series && (
        <div className="text-dim">No balance history in the last {range}.</div>
      )}
      {!error && !loading && series && <Plot series={series} range={range} />}
    </div>
  )
}

interface Series {
  times: number[]
  equities: number[]
  balances: number[]
  minV: number
  maxV: number
  minT: number
  maxT: number
}

// Plot renders at a fixed viewBox width and scales to its container, so the chart is responsive
// without needing a resize observer.
function Plot({ series, range }: { series: Series; range: Range }) {
  const WIDTH = 900
  const plotW = WIDTH - PAD.left - PAD.right
  const plotH = HEIGHT - PAD.top - PAD.bottom
  const { times, equities, balances, minV, maxV, minT, maxT } = series

  const xf = (t: number) => PAD.left + (maxT === minT ? plotW / 2 : ((t - minT) / (maxT - minT)) * plotW)
  const yf = (v: number) => PAD.top + plotH - ((v - minV) / (maxV - minV)) * plotH

  const pathFor = (vals: number[]) =>
    vals.map((v, i) => `${i === 0 ? 'M' : 'L'} ${xf(times[i]).toFixed(1)} ${yf(v).toFixed(1)}`).join(' ')

  // Five evenly spaced x ticks across the window, and min/mid/max on y.
  const xTicks = Array.from({ length: 5 }, (_, i) => minT + ((maxT - minT) * i) / 4)
  const yTicks = [minV, (minV + maxV) / 2, maxV]

  return (
    <svg viewBox={`0 0 ${WIDTH} ${HEIGHT}`} className="balance-chart-svg" preserveAspectRatio="none">
      {yTicks.map((v) => (
        <g key={v}>
          <line
            x1={PAD.left}
            y1={yf(v)}
            x2={WIDTH - PAD.right}
            y2={yf(v)}
            stroke="var(--border, #2a2a2a)"
            strokeWidth={1}
          />
          <text x={4} y={yf(v) + 4} className="chart-axis-label">
            ${v.toFixed(2)}
          </text>
        </g>
      ))}
      {xTicks.map((t) => (
        <text key={t} x={xf(t)} y={HEIGHT - 6} className="chart-axis-label" textAnchor="middle">
          {tickLabel(t, range)}
        </text>
      ))}
      <path d={pathFor(balances)} fill="none" stroke="var(--chart-balance, #9d7bff)" strokeWidth={1.5} />
      <path d={pathFor(equities)} fill="none" stroke="var(--chart-equity, #4ea1ff)" strokeWidth={1.8} />
    </svg>
  )
}
