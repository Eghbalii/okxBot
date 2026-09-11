import { useEffect, useRef, useState } from 'react'
import {
  CandlestickSeries,
  createChart,
  createSeriesMarkers,
  type IChartApi,
  type ISeriesApi,
  type SeriesMarker,
  type Time,
  type UTCTimestamp,
} from 'lightweight-charts'
import type { Candle, Position } from '../api/types'
import { PositionZones, type ZonePosition } from './PositionZones'

const UP = '#2ebd85'
const DOWN = '#f6465d'

// Hover detail for one marker. Kept as plain resolved strings rather than the Position itself so
// the tooltip renders without re-deriving anything while the mouse moves.
interface MarkerDetail {
  id: number
  heading: string
  rows: [string, string][]
  color: string
}

function fmt(n: string | null): string {
  if (n === null || n === '') return '—'
  const v = Number(n)
  if (!Number.isFinite(v)) return n
  // Prices on this roster span 0.01 (SOL) to 1e-9 (PEPE), so a fixed precision would be wrong for
  // most of them — significant digits travel better across that range.
  return v >= 1 ? v.toFixed(2) : v.toPrecision(6).replace(/0+$/, '').replace(/\.$/, '')
}

function pnlColor(pnl: string | null): string {
  if (pnl === null) return '#d5d8de'
  return Number(pnl) >= 0 ? UP : DOWN
}

const secs = (iso: string) => (Date.parse(iso) / 1000) as UTCTimestamp

/**
 * Candles for one instrument with its position history overlaid.
 *
 * Entry and exit are arrow markers (lightweight-charts ships circle/square/arrowUp/arrowDown —
 * there is no triangle shape, and the arrows carry direction, which a triangle would not).
 * Hovering a marker shows that order's detail, resolved through the marker's own `id` and the
 * `hoveredInfo.objectId` the crosshair reports.
 *
 * Open positions additionally get the shaded entry→SL (red) and entry→TP (green) bands, drawn by
 * the PositionZones primitive.
 */
export function CandleChart({
  candles,
  positions,
  height = 420,
}: {
  candles: Candle[]
  positions: Position[]
  height?: number
}) {
  const container = useRef<HTMLDivElement>(null)
  const chartRef = useRef<IChartApi | null>(null)
  const seriesRef = useRef<ISeriesApi<'Candlestick', Time> | null>(null)
  const zonesRef = useRef<PositionZones | null>(null)
  const detailsRef = useRef<Map<string, MarkerDetail>>(new Map())
  const [hover, setHover] = useState<{ detail: MarkerDetail; x: number; y: number } | null>(null)

  // Create the chart once. Data updates go through the effect below, so a new candle does not
  // tear down and rebuild the canvas (which would lose the user's pan/zoom).
  useEffect(() => {
    if (!container.current) return
    const chart = createChart(container.current, {
      height,
      autoSize: true,
      layout: {
        background: { color: 'transparent' },
        textColor: '#9aa0ab',
        // Required by the Apache-2.0 licence: a visible link back to TradingView.
        attributionLogo: true,
      },
      grid: { vertLines: { color: '#21242b' }, horzLines: { color: '#21242b' } },
      rightPriceScale: { borderColor: '#2a2e37' },
      timeScale: { borderColor: '#2a2e37', timeVisible: true, secondsVisible: false },
      crosshair: { mode: 0 },
    })
    const series = chart.addSeries(CandlestickSeries, {
      upColor: UP,
      downColor: DOWN,
      wickUpColor: UP,
      wickDownColor: DOWN,
      borderVisible: false,
    })
    const zones = new PositionZones()
    series.attachPrimitive(zones)

    chart.subscribeCrosshairMove((param) => {
      const id = param.hoveredInfo?.objectId
      const detail = typeof id === 'string' ? detailsRef.current.get(id) : undefined
      if (!detail || !param.point) {
        setHover(null)
        return
      }
      setHover({ detail, x: param.point.x, y: param.point.y })
    })

    chartRef.current = chart
    seriesRef.current = series
    zonesRef.current = zones
    return () => {
      chart.remove()
      chartRef.current = null
      seriesRef.current = null
      zonesRef.current = null
    }
  }, [height])

  useEffect(() => {
    const series = seriesRef.current
    const chart = chartRef.current
    if (!series || !chart) return

    series.setData(
      candles.map((c) => ({
        time: secs(c.Timestamp),
        open: Number(c.Open),
        high: Number(c.High),
        low: Number(c.Low),
        close: Number(c.Close),
      })),
    )

    const details = new Map<string, MarkerDetail>()
    const markers: SeriesMarker<Time>[] = []

    for (const p of positions) {
      const long = p.Side === 'buy'
      const openID = `open-${p.ID}`
      markers.push({
        id: openID,
        time: secs(p.OpenedAt),
        position: long ? 'belowBar' : 'aboveBar',
        shape: long ? 'arrowUp' : 'arrowDown',
        color: long ? UP : DOWN,
        text: `#${p.ID}`,
      })
      details.set(openID, {
        id: p.ID,
        heading: `#${p.ID} ${long ? 'LONG' : 'SHORT'} opened`,
        color: long ? UP : DOWN,
        rows: [
          ['Entry', fmt(p.EntryPx)],
          ['SL', fmt(p.SLPx)],
          ['TP', fmt(p.TPPx)],
          ['Size', `${fmt(p.Size)} @ ${fmt(p.Leverage)}x`],
          ['Strategy', p.StrategyName || '—'],
          ['Bar', p.Bar || '—'],
          ['Opened', new Date(p.OpenedAt).toLocaleString()],
        ],
      })

      if (!p.ClosedAt) continue

      // The exit arrow points opposite the entry: a long is closed by selling.
      const closeID = `close-${p.ID}`
      const won = p.RealizedPnL !== null && Number(p.RealizedPnL) >= 0
      markers.push({
        id: closeID,
        time: secs(p.ClosedAt),
        position: long ? 'aboveBar' : 'belowBar',
        shape: long ? 'arrowDown' : 'arrowUp',
        // Coloured by OUTCOME, not by side — §11.3's own rule that a win is positive realized PnL,
        // not close_reason='tp' (a ratcheted stop can close in profit).
        color: won ? UP : DOWN,
        text: p.CloseReason ?? 'closed',
      })
      details.set(closeID, {
        id: p.ID,
        heading: `#${p.ID} closed — ${p.CloseReason ?? 'unknown'}`,
        color: won ? UP : DOWN,
        rows: [
          ['Entry', fmt(p.EntryPx)],
          ['Exit', fmt(p.ClosePx)],
          ['PnL', p.RealizedPnL === null ? '—' : `${Number(p.RealizedPnL) >= 0 ? '+' : ''}${Number(p.RealizedPnL).toFixed(4)}`],
          ['Fees', fmt(p.FeesUSD)],
          ['Held', held(p.OpenedAt, p.ClosedAt)],
          ['Closed', new Date(p.ClosedAt).toLocaleString()],
        ],
      })
    }

    // Markers must be time-ordered or the library rejects the set.
    markers.sort((a, b) => (a.time as number) - (b.time as number))
    detailsRef.current = details
    createSeriesMarkers(series, markers)

    const open: ZonePosition[] = positions
      .filter((p) => !p.ClosedAt)
      .map((p) => ({
        id: p.ID,
        side: p.Side,
        entry: Number(p.EntryPx),
        sl: p.SLPx === null ? null : Number(p.SLPx),
        tp: p.TPPx === null ? null : Number(p.TPPx),
      }))
    zonesRef.current?.setPositions(open)

    chart.timeScale().fitContent()
  }, [candles, positions])

  return (
    <div className="candle-chart" style={{ position: 'relative' }}>
      <div ref={container} style={{ width: '100%', height }} />
      {hover && (
        <div
          className="chart-tooltip"
          style={{
            left: Math.min(hover.x + 14, (container.current?.clientWidth ?? 0) - 210),
            top: Math.max(hover.y - 10, 4),
            borderLeftColor: hover.detail.color,
          }}
        >
          <div className="chart-tooltip-head" style={{ color: hover.detail.color }}>
            {hover.detail.heading}
          </div>
          {hover.detail.rows.map(([k, v]) => (
            <div className="chart-tooltip-row" key={k}>
              <span>{k}</span>
              <b style={k === 'PnL' ? { color: pnlColor(v.replace('+', '')) } : undefined}>{v}</b>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

function held(from: string, to: string): string {
  const ms = Date.parse(to) - Date.parse(from)
  if (!Number.isFinite(ms) || ms < 0) return '—'
  const m = Math.round(ms / 60000)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  return h < 24 ? `${h}h ${m % 60}m` : `${Math.floor(h / 24)}d ${h % 24}h`
}
