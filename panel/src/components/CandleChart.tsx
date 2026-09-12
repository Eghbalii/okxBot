import { useEffect, useRef, useState } from 'react'
import {
  CandlestickSeries,
  createChart,
  createSeriesMarkers,
  type IChartApi,
  type ISeriesApi,
  type ISeriesMarkersPluginApi,
  type SeriesMarker,
  type Time,
  type UTCTimestamp,
} from 'lightweight-charts'
import type { Candle, Position } from '../api/types'
import { PositionZones, type ZonePosition } from './PositionZones'

// Tooltip footprint, used to decide which side of the cursor it can fit on. Approximate by
// design — it only has to be right enough to pick a side, and measuring the real box would need a
// layout pass on every mouse move.
const TOOLTIP_W = 230
const TOOLTIP_H = 190

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
  // Changing this re-frames the view. The modal passes the selected timeframe, because switching
  // 5m -> 1H is the user asking for a different chart, whereas a 5s refetch is not — and only the
  // first must keep its own viewport.
  frameKey,
}: {
  candles: Candle[]
  positions: Position[]
  height?: number
  frameKey?: string
}) {
  const container = useRef<HTMLDivElement>(null)
  const chartRef = useRef<IChartApi | null>(null)
  const seriesRef = useRef<ISeriesApi<'Candlestick', Time> | null>(null)
  const zonesRef = useRef<PositionZones | null>(null)
  // ONE markers plugin for the chart's lifetime. createSeriesMarkers ATTACHES a new primitive each
  // time it is called and returns a handle to it — it is not an idempotent setter. Calling it per
  // effect run (which now includes every 5s positions refetch) left every previous instance still
  // attached and drawing its own copy of the arrows, so markers accumulated and rendered wrong.
  const markersRef = useRef<ISeriesMarkersPluginApi<Time> | null>(null)
  const detailsRef = useRef<Map<string, MarkerDetail>>(new Map())
  // Guards the one-time initial framing; see the data effect below.
  // Holds the frameKey the current viewport was framed for. The sentinel is deliberately not
  // undefined: frameKey itself is optional, and an undefined-vs-undefined comparison would skip the
  // very first framing.
  const framedRef = useRef<string | undefined>('\u0000unframed')
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
      // rightOffset keeps a few bars of empty space after the last candle: without it a position
      // opened in the most recent bars has its marker label clipped by the pane edge (observed on
      // a live ETH order, whose "#3044" read as "#304").
      timeScale: { borderColor: '#2a2e37', timeVisible: true, secondsVisible: false, rightOffset: 6 },
      // Normal mode plus explicit label styling: the axis labels showing the hovered time and
      // price DO appear by default, but at the library's own grey they were easy to miss against
      // this theme. Both axes get a readable badge instead.
      crosshair: {
        mode: 0,
        vertLine: { labelVisible: true, labelBackgroundColor: '#3a4150', color: '#5d6673', width: 1 },
        horzLine: { labelVisible: true, labelBackgroundColor: '#3a4150', color: '#5d6673', width: 1 },
      },
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
    markersRef.current = createSeriesMarkers(series, [])

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
      markersRef.current = null
    }
    // Deliberately NOT keyed on height: the modal re-sizes the chart when the window changes, and
    // tearing the chart down to rebuild it would discard the viewport the user had zoomed to —
    // the exact behaviour just fixed. Height is applied through applyOptions below instead.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Whole-series writes are keyed on the series IDENTITY, not on every candle change. A live tick
  // mutates only the newest bar, and setData() on each one would rebuild all 500 bars and every
  // marker several times a second — enough to make panning stutter. The tick path below calls
  // update() instead, which is what lightweight-charts provides for exactly this.
  //
  // barsKey changes when a bar is ADDED or REMOVED (a new candle closed, or the timeframe
  // switched) but not when the last bar's high/low/close move, so a tick never reaches here.
  const barsKey = `${frameKey ?? ''}|${candles.length}|${candles.length > 0 ? candles[0].Timestamp : ''}|${candles.length > 0 ? candles[candles.length - 1].Timestamp : ''}`

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
          ['Opened', shortTime(p.OpenedAt)],
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
          ['Closed', shortTime(p.ClosedAt)],
        ],
      })
    }

    // Markers must be time-ordered or the library rejects the set.
    markers.sort((a, b) => (a.time as number) - (b.time as number))
    detailsRef.current = details
    markersRef.current?.setMarkers(markers)

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

    // Only frame the view the FIRST time data arrives. The positions table refetches every 5s and
    // re-renders this component with it, so calling fitContent on each pass threw away whatever
    // the user had panned or zoomed to — reported as "I zoom in, move it, and it jumps back".
    // After the initial frame the viewport belongs to the user, and new candles simply extend it.
    if (framedRef.current !== frameKey && candles.length > 0) {
      framedRef.current = frameKey
      showRecent(chart, candles.length)
    }
    // candles is intentionally absent: barsKey is what says the SERIES changed, and depending on
    // the array itself would put this whole rebuild back on the per-tick path.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [barsKey, positions, frameKey])

  // Live tick: move only the newest bar. update() must be called with a time >= the series' last,
  // which barsKey's own effect guarantees — it has already written this bar via setData by the time
  // a tick for it can arrive, because both run off the same render.
  const lastCandle = candles.length > 0 ? candles[candles.length - 1] : null
  useEffect(() => {
    const series = seriesRef.current
    if (!series || !lastCandle) return
    series.update({
      time: secs(lastCandle.Timestamp),
      open: Number(lastCandle.Open),
      high: Number(lastCandle.High),
      low: Number(lastCandle.Low),
      close: Number(lastCandle.Close),
    })
  }, [lastCandle?.Timestamp, lastCandle?.Open, lastCandle?.High, lastCandle?.Low, lastCandle?.Close])

  return (
    <div className="candle-chart" style={{ position: 'relative' }}>
      {/* autoSize makes the chart track THIS element and ignore the height option, so the
          container is what carries the size — and a plain style change resizes the chart without
          recreating it, preserving the user's zoom. */}
      <div ref={container} style={{ width: '100%', height }} />
      {hover && (
        <div
          className="chart-tooltip"
          style={{
            // Flip to the left of the cursor near the right edge rather than clamping: clamping
            // still let the box overrun the pane (observed against a live order whose timestamp
            // wrapped and pushed "AM" past the edge), and a flipped tooltip stays fully readable.
            left:
              hover.x + TOOLTIP_W + 18 > (container.current?.clientWidth ?? 0)
                ? Math.max(hover.x - TOOLTIP_W - 14, 4)
                : hover.x + 14,
            top: Math.min(Math.max(hover.y - 10, 4), Math.max(height - TOOLTIP_H, 4)),
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

// Day + time, no year and no seconds: the axis underneath already says which day is in view, and
// the full locale string wrapped onto two lines and overran the tooltip.
function shortTime(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

// How many bars to show when the chart first opens. The full history is 500 candles, which on a
// 5m chart is nearly two days — far too wide to read individual candles, so the operator had to
// zoom in on every open. This frames the recent window instead; the rest stays one scroll away.
const INITIAL_BARS = 90

function showRecent(chart: IChartApi, total: number): void {
  if (total <= INITIAL_BARS) {
    chart.timeScale().fitContent()
    return
  }
  // Logical range indexes bars, not time, so this holds regardless of the timeframe. The +6 keeps
  // rightOffset's empty bars in view so a marker on the newest candle isn't against the edge.
  chart.timeScale().setVisibleLogicalRange({ from: total - INITIAL_BARS, to: total + 6 })
}

function held(from: string, to: string): string {
  const ms = Date.parse(to) - Date.parse(from)
  if (!Number.isFinite(ms) || ms < 0) return '—'
  const m = Math.round(ms / 60000)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  return h < 24 ? `${h}h ${m % 60}m` : `${Math.floor(h / 24)}d ${h % 24}h`
}
