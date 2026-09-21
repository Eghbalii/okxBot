import { useEffect, useRef, useState } from 'react'
import { darkTheme, type Bar, type Chart, type ChartTheme, type SeriesApi, type SeriesMarker, type SeriesMarkers } from 'openalgo-charts'
import { createWidget, type Widget } from 'openalgo-charts/widget'
// Side-effect imports (2026-09-21 request): both tiers self-register their built-ins at module
// load — confirmed against the compiled source (openalgo-charts.indicators.mjs/.draw.mjs each
// call their own registerBuiltin*() once at the top level, guarded so importing twice is a no-op)
// rather than assumed from the README alone. Without these, createWidget's Indicators button and
// drawing rail render with nothing in them: the widget shell only PRESENTS whatever is registered,
// it does not register anything itself.
import 'openalgo-charts/indicators'
import 'openalgo-charts/draw'
import { barSeconds } from '../../lib/bars'
import type { Candle } from '../../api/types'
import { attachPositionZones, parseZoneDragId, GRAB_TOLERANCE_PX, PositionZones, type ZonePosition } from './OpenAlgoZones'
import type { CandleChartProps } from './types'

// Mirrors components/CandleChart.tsx's own constants/helpers exactly — kept duplicated rather than
// shared (see OpenAlgoZones.ts's own note on why) since the two engines' render loops don't share
// a call shape either.
const TOOLTIP_W = 230
const TOOLTIP_H = 190
const UP = '#2ebd85'
const DOWN = '#f6465d'

// Dark theme, matching the panel's own dark palette (2026-09-21 fix — the engine's `createChart`
// default is `lightTheme`, not dark as an earlier version of this file's own comment wrongly
// assumed; confirmed by grepping the shipped source rather than trusting the doc-comment claim
// that `theme: undefined` meant "the engine's own dark theme"). Built on the library's `darkTheme`
// preset rather than the CandleChart.tsx/App.css palette from scratch, then only the few fields
// that need to match the TV engine's exact look are overridden — everything else (grid dash style,
// crosshair styling, baseline/area colors this chart never uses) keeps the library's own
// already-coherent dark defaults instead of a second, hand-maintained full palette.
const PANEL_DARK_THEME: ChartTheme = {
  ...darkTheme,
  background: '#14161b', // matches App.css's --bg-panel-ish dark card background, not pure black
  grid: '#21242b', // CandleChart.tsx's own grid.vertLines/horzLines color
  axisText: '#9aa0ab', // CandleChart.tsx's own layout.textColor
  axisLine: '#2a2e37', // CandleChart.tsx's own rightPriceScale/timeScale borderColor
  paneSeparator: '#2a2e37',
  crosshair: '#5d6673', // CandleChart.tsx's own crosshair line color
  upColor: UP,
  downColor: DOWN,
  wickUpColor: UP,
  wickDownColor: DOWN,
  buy: UP,
  sell: DOWN,
  profit: UP,
  loss: DOWN,
}

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
  return v >= 1 ? v.toFixed(2) : v.toPrecision(6).replace(/0+$/, '').replace(/\.$/, '')
}

function pnlColor(pnl: string | null): string {
  if (pnl === null) return '#d5d8de'
  return Number(pnl) >= 0 ? UP : DOWN
}

// openalgo-charts' Bar.time is UTC SECONDS (an integer), same unit as lightweight-charts' Time —
// so this is identical to CandleChart.tsx's `secs`, kept local rather than imported since it's a
// one-line function and importing across the engine-implementation boundary would be the wrong
// direction of coupling (the shared contract is CandleChartProps, not per-engine helpers).
const secs = (iso: string) => Math.floor(Date.parse(iso) / 1000)

function snapToBar(iso: string, bar: string): number {
  const ms = Date.parse(iso)
  const secsPerBar = barSeconds(bar)
  if (!secsPerBar || !Number.isFinite(ms)) return Math.floor(ms / 1000)
  return Math.floor(ms / (secsPerBar * 1000)) * secsPerBar
}

function pricePrecision(candles: Candle[]): number {
  let needed = 2
  for (const c of candles) {
    for (const v of [c.Open, c.High, c.Low, c.Close]) {
      const n = Number(v)
      if (!Number.isFinite(n) || n === 0) continue
      if (Math.abs(n) >= 1) continue
      const dec = v.includes('.') ? v.split('.')[1].replace(/0+$/, '').length : 0
      const lead = /^0\.(0*)/.exec(v)
      const zeros = lead ? lead[1].length : 0
      needed = Math.max(needed, Math.min(Math.max(dec, zeros + 2), 10))
    }
  }
  return needed
}

const INITIAL_BARS = 90

function shortTime(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
}

// Matches TradePage.tsx's own fmtUsdCompact convention (contracts, not USD, here — but the same
// "don't print 8-digit volume numbers" reasoning applies) rather than a raw fixed-decimal number.
function fmtVolume(v: number): string {
  if (!Number.isFinite(v)) return '—'
  if (v >= 1e9) return `${(v / 1e9).toFixed(2)}B`
  if (v >= 1e6) return `${(v / 1e6).toFixed(2)}M`
  if (v >= 1e3) return `${(v / 1e3).toFixed(2)}K`
  return v.toFixed(2)
}

function held(from: string, to: string): string {
  const ms = Date.parse(to) - Date.parse(from)
  if (!Number.isFinite(ms) || ms < 0) return '—'
  const m = Math.round(ms / 60000)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  return h < 24 ? `${h}h ${m % 60}m` : `${Math.floor(h / 24)}d ${h % 24}h`
}

/**
 * Second chart-engine implementation of CandleChartProps (2026-09-21 request), built on
 * openalgo-charts (Apache-2.0, from-scratch canvas engine, zero runtime deps — CLAUDE.md-external
 * research comparing it against TradingView lightweight-charts and klinecharts/pro) rather than
 * replacing CandleChart.tsx (the TradingView-lightweight-charts implementation), so an open-sourced
 * deployment can keep TradingView as an option (attribution requirement + familiarity) while this
 * project's own panel can run the richer engine. Selected by ../chart/index.tsx's engine switch —
 * this file has no opinion on which engine is active.
 *
 * Feature parity with CandleChart.tsx, ported rather than reduced: candles, a volume histogram
 * pane, entry/exit markers with a hover tooltip, and the same PositionZones-shaped SL/TP
 * risk/reward boxes with drag-to-adjust — because dropping any of those on the "new" engine would
 * make switching engines a regression, not a lateral choice.
 */
export function OpenAlgoCandleChart({
  candles,
  positions,
  height = 420,
  frameKey,
  editPositionId,
  editSl,
  editTp,
  editLabelMode,
  onDragLevel,
}: CandleChartProps) {
  const container = useRef<HTMLDivElement>(null)
  const chartRef = useRef<Chart | null>(null)
  const seriesRef = useRef<SeriesApi | null>(null)
  const volumeRef = useRef<SeriesApi | null>(null)
  const zonesRef = useRef<PositionZones | null>(null)
  const markersRef = useRef<SeriesMarkers | null>(null)
  const detailsRef = useRef<Map<string, MarkerDetail>>(new Map())
  const widgetRef = useRef<Widget | null>(null)
  const onDragRef = useRef(onDragLevel)
  onDragRef.current = onDragLevel
  const framedRef = useRef<string | undefined>('\u0000unframed')
  const [hover, setHover] = useState<{ detail: MarkerDetail; x: number; y: number } | null>(null)
  // OHLCV readout (2026-09-21 request — "show candle info like every exchange"): the hovered bar
  // while the cursor is over the plot, or null while it isn't — the render below falls back to the
  // LAST candle whenever this is null, which is what makes "default to the latest candle, follow
  // the cursor otherwise" work without two separate code paths.
  const [hoveredBar, setHoveredBar] = useState<Bar | null>(null)
  // Drawing rail hidden by default (2026-09-21 request): the tool column costs real chart width
  // on every load whether or not it's ever used, so it starts collapsed behind an arrow button
  // instead of always-on. Toggled via a plain CSS class rather than the engine's own API (there is
  // none for this — confirmed against the widget's public surface) since `.oac-rail` is a normal
  // child element this component already has a container class around.
  const [railOpen, setRailOpen] = useState(false)

  // Create the widget once — data updates go through the effect below so a new candle never tears
  // down the canvas (which would discard the user's pan/zoom), matching CandleChart.tsx's own
  // split between the identity effect and the data effect.
  //
  // createWidget (not the bare createChart) is what actually delivers "the tools and indicators" —
  // it mounts the toolbar, symbol/interval chrome, the Indicators picker, and the drawing rail
  // around a Chart, rather than being a separate product from it (widget.chart is the exact same
  // Chart type the bare createChart returns, so every existing zones/markers/subscribeDrag call
  // below is untouched). Deliberately NO `feed` option: the widget's own DataFeed/OpenAlgoDataFeed
  // machinery is for pulling candles from an exchange/broker itself, which would mean this chart's
  // history comes from somewhere other than THIS project's own OKX ingestion + paper-trading
  // pipeline — the hard "only our data" requirement. Omitting `feed` is explicitly documented
  // ("without one the chart shows what the host sets on `widget.series` itself"), so this project
  // keeps driving candles exactly as before via setData/update, and only gains the chrome.
  useEffect(() => {
    if (!container.current) return
    const widget = createWidget(container.current, {
      theme: PANEL_DARK_THEME,
      timeScale: { rightOffset: 6 },
      // Symbol/interval/search TOPBAR is switched off: this component receives its candles as
      // props from the panel's own token/timeframe pickers (TokenChartModal's strip, TradePage's
      // bar buttons) — a second, competing symbol/interval picker inside the chart, wired to no
      // `feed`, would look functional and silently do nothing when clicked, which is worse than not
      // having it. `indicators: true` alone does NOT surface a button for this — per the widget's
      // own doc comment the Indicators button lives ON the topbar, so hiding the topbar hides it
      // too (the actual cause of the reported "why didn't you activate indicators" gap: they were
      // registered and addable via code, just with no visible entry point). Fixed below by calling
      // `widget.openIndicatorPicker()` from this project's OWN "Indicators" button instead of
      // exposing the whole topbar just to reach one of its buttons.
      topbar: false,
      indicators: true,
      rail: true,
      statusline: true,
      mobile: 'never',
    })
    widgetRef.current = widget
    const chart = widget.chart
    const series = widget.series
    series.applyOptions({ upColor: UP, downColor: DOWN, wickUpColor: UP, wickDownColor: DOWN, borderVisible: false })
    // Volume as an overlay-scale histogram pinned to the bottom of pane 0, the pattern from the
    // engine's own shipped yfinance example (examples/yfinance/src/volume.js) — priceScaleId: ''
    // gives it a hidden axis sharing the price pane rather than a second scrollable pane.
    const volume = chart.addSeries('histogram', {
      paneIndex: 0,
      priceScaleId: '',
      priceFormat: { type: 'volume' },
      style: { priceLineVisible: false, lastValueVisible: false },
    })
    volume.priceScale().setOptions({ marginTop: 0.82, marginBottom: 0 })

    const zones = attachPositionZones(chart)
    const markers = series.createMarkers(() => chart.primaryBars())

    chart.subscribeCrosshairMove((e) => {
      // Marker hover: this engine's crosshair event carries the hovered bar/price, not a hit id,
      // so hover-matching is done here by nearest marker within a small pixel radius rather than
      // relying on a hit-test the crosshair path doesn't report. Good enough for a tooltip, which
      // only needs "close enough to the glyph", not exact hit geometry.
      if (!e.point || e.time === null) {
        setHover(null)
        setHoveredBar(null)
        return
      }
      setHoveredBar(e.bar)
      let found: MarkerDetail | null = null
      for (const [id, detail] of detailsRef.current) {
        const t = Number(id.split('@')[1])
        if (Math.abs(t - e.time) < barSeconds(frameKey ?? '') / 2) {
          found = detail
          break
        }
      }
      if (!found) {
        setHover(null)
        return
      }
      setHover({ detail: found, x: e.point.x, y: e.point.y })
    })

    // Drag: chart.subscribeDrag is a first-class engine API (fires (externalId, price, time) for
    // anything the primitive's hitTest reported draggable:true) — no manual pointer capture is
    // needed here, unlike CandleChart.tsx's TradingView-engine version, which has to install its
    // own capture-phase listeners because lightweight-charts has no equivalent subscription.
    chart.subscribeDrag((externalId, price) => {
      const parsed = parseZoneDragId(externalId)
      if (!parsed) return
      onDragRef.current?.(parsed.which, price)
    })

    chartRef.current = chart
    seriesRef.current = series
    volumeRef.current = volume
    zonesRef.current = zones
    markersRef.current = markers
    return () => {
      // widget.destroy() (not chart.destroy()) — the widget owns the chart plus every mounted rail/
      // toolbar/dialog piece and their own listeners; calling chart.destroy() alone would leave the
      // widget shell's DOM and event bindings behind on every unmount (a modal close, an engine
      // switch), leaking one full toolbar's worth of listeners per open.
      widget.destroy()
      widgetRef.current = null
      chartRef.current = null
      seriesRef.current = null
      volumeRef.current = null
      zonesRef.current = null
      markersRef.current = null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Container resize: this engine sizes to its container's actual box (no autoSize flag exists in
  // ChartOptions), so an explicit resize call is needed when `height` changes — CandleChart.tsx's
  // autoSize does this implicitly via ResizeObserver, this engine leaves sizing to the host.
  useEffect(() => {
    if (container.current) container.current.style.height = `${height}px`
  }, [height])

  const barsKey = `${frameKey ?? ''}|${candles.length}|${candles.length > 0 ? candles[0].Timestamp : ''}|${candles.length > 0 ? candles[candles.length - 1].Timestamp : ''}`

  useEffect(() => {
    const series = seriesRef.current
    const chart = chartRef.current
    if (!series || !chart) return

    const precision = pricePrecision(candles)
    series.applyOptions({ precision })

    const bars = candles.map((c) => ({
      time: secs(c.Timestamp),
      open: Number(c.Open),
      high: Number(c.High),
      low: Number(c.Low),
      close: Number(c.Close),
    }))
    series.setData(bars)

    volumeRef.current?.setData(
      candles.map((c) => ({
        time: secs(c.Timestamp),
        open: 0,
        high: Number(c.Volume) || 0,
        low: 0,
        close: Number(c.Volume) || 0,
        color: Number(c.Close) >= Number(c.Open) ? 'rgba(46,189,133,0.5)' : 'rgba(246,70,93,0.5)',
      })),
    )

    const details = new Map<string, MarkerDetail>()
    const markers: SeriesMarker[] = []

    // Defensive filter (2026-09-21) — a position whose entry sits nowhere near the candles just
    // written above is stale/mismatched data, not a real signal: it never means "this token moved
    // 1000x", it means positions and candles briefly disagreed about which instrument is showing
    // (TokenChartModal's own instId-transition race, guarded there too — see its candlesMatchInstId
    // comment) or some other future case with the same shape. Dropping it here is a second,
    // independent line of defence: even if a race slips past the caller's own guard, this chart
    // will never draw a zone box for a position that could not possibly belong to what's on
    // screen, rather than drawing a plausible-looking box in the wrong place. The bound is
    // deliberately wide (10x the visible candle range) — this is a sanity check against a
    // completely different instrument's price, not a tight validity check on a real move.
    let candleLow = Infinity
    let candleHigh = -Infinity
    for (const c of candles) {
      const lo = Number(c.Low)
      const hi = Number(c.High)
      if (Number.isFinite(lo) && lo < candleLow) candleLow = lo
      if (Number.isFinite(hi) && hi > candleHigh) candleHigh = hi
    }
    const candleSpan = candleHigh - candleLow
    const plausible = (price: number) => {
      if (!Number.isFinite(candleLow) || !Number.isFinite(candleHigh)) return true
      const pad = Math.max(candleSpan * 10, candleHigh * 0.5, 1e-9)
      return price >= candleLow - pad && price <= candleHigh + pad
    }
    const plausiblePositions = positions.filter((p) => plausible(Number(p.EntryPx)))

    for (const p of plausiblePositions) {
      const long = p.Side === 'buy'
      const openTime = snapToBar(p.OpenedAt, frameKey ?? '')
      // Keyed by "id@time" so the crosshair hover-match above can recover the bar time without a
      // separate lookup table — this engine's own SeriesMarker has no id-based hover reporting.
      const openKey = `open-${p.ID}@${openTime}`
      markers.push({
        time: openTime,
        position: long ? 'belowBar' : 'aboveBar',
        shape: long ? 'arrowUp' : 'arrowDown',
        size: 'small',
        color: long ? UP : DOWN,
        text: `#${p.ID}`,
        id: openKey,
      })
      details.set(openKey, {
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

      const closeTime = snapToBar(p.ClosedAt, frameKey ?? '')
      const closeKey = `close-${p.ID}@${closeTime}`
      const won = p.RealizedPnL !== null && Number(p.RealizedPnL) >= 0
      markers.push({
        time: closeTime,
        position: long ? 'aboveBar' : 'belowBar',
        shape: long ? 'arrowDown' : 'arrowUp',
        size: 'small',
        color: won ? UP : DOWN,
        text: p.CloseReason ?? 'closed',
        id: closeKey,
      })
      details.set(closeKey, {
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

    detailsRef.current = details
    markersRef.current?.setMarkers(markers)

    const zonePositions: ZonePosition[] = plausiblePositions.map((p) => ({
      id: p.ID,
      side: p.Side,
      entry: Number(p.EntryPx),
      sl: p.SLPx === null ? null : Number(p.SLPx),
      tp: p.TPPx === null ? null : Number(p.TPPx),
      openTime: snapToBar(p.OpenedAt, frameKey ?? ''),
      closeTime: p.ClosedAt ? snapToBar(p.ClosedAt, frameKey ?? '') : null,
      leverage: Number(p.Leverage) || 1,
      pendingSl: p.ID === editPositionId ? (editSl ?? null) : undefined,
      pendingTp: p.ID === editPositionId ? (editTp ?? null) : undefined,
      editable: p.ID === editPositionId,
      labelMode: p.ID === editPositionId ? editLabelMode : undefined,
    }))
    zonesRef.current?.setPositions(zonePositions)

    if (framedRef.current !== frameKey && candles.length > 0) {
      framedRef.current = frameKey
      if (candles.length <= INITIAL_BARS) {
        chart.fitContent()
      } else {
        chart.setVisibleLogicalRange({ from: candles.length - INITIAL_BARS, to: candles.length + 6 })
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [barsKey, positions, frameKey, editPositionId, editSl, editTp, editLabelMode])

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
    volumeRef.current?.update({
      time: secs(lastCandle.Timestamp),
      open: 0,
      high: Number(lastCandle.Volume) || 0,
      low: 0,
      close: Number(lastCandle.Volume) || 0,
      color: Number(lastCandle.Close) >= Number(lastCandle.Open) ? 'rgba(46,189,133,0.5)' : 'rgba(246,70,93,0.5)',
    })
  }, [
    lastCandle?.Timestamp,
    lastCandle?.Open,
    lastCandle?.High,
    lastCandle?.Low,
    lastCandle?.Close,
    lastCandle?.Volume,
  ])

  // The readout candle: whatever the cursor is over, falling back to the LAST candle when the
  // cursor is off the plot — this single fallback is the entire "default to latest, follow the
  // cursor otherwise" behavior (2026-09-21 request), no separate default-vs-hover code path.
  // `hoveredBar`'s fields are already numbers (openalgo-charts' own Bar shape); `lastCandle`'s are
  // strings (this project's own Candle shape) — normalized to one shape here so the JSX below
  // doesn't need to branch on which source it came from.
  //
  // Volume is looked up from `candles` BY TIME rather than trusted from `hoveredBar.volume`:
  // `CrosshairMoveEvent.bar` is "the hovered bar of the PRIMARY price series" per the engine's own
  // doc comment — that's the candlestick series, whose bars were never given a `.volume` field
  // (only the separate histogram series' own bars carry it, set in the data effect above) — so
  // `hoveredBar.volume` reads `undefined` on every hover, always falling back to 0.
  const hoveredVolume =
    hoveredBar !== null ? (candles.find((c) => secs(c.Timestamp) === hoveredBar.time)?.Volume ?? null) : null
  const displayCandle =
    hoveredBar !== null
      ? {
          open: hoveredBar.open,
          high: hoveredBar.high,
          low: hoveredBar.low,
          close: hoveredBar.close,
          volume: hoveredVolume !== null ? Number(hoveredVolume) || 0 : 0,
          time: hoveredBar.time,
        }
      : lastCandle !== null
        ? {
            open: Number(lastCandle.Open),
            high: Number(lastCandle.High),
            low: Number(lastCandle.Low),
            close: Number(lastCandle.Close),
            volume: Number(lastCandle.Volume) || 0,
            time: secs(lastCandle.Timestamp),
          }
        : null
  const displayUp = displayCandle !== null && displayCandle.close >= displayCandle.open
  const displayChangePct =
    displayCandle !== null && displayCandle.open !== 0
      ? ((displayCandle.close - displayCandle.open) / displayCandle.open) * 100
      : null

  return (
    <div className={'candle-chart oac-host' + (railOpen ? ' oac-rail-open' : '')} style={{ position: 'relative' }}>
      <div ref={container} style={{ width: '100%', height }} />
      {/* Rail toggle (2026-09-21 request): the drawing-tool column is hidden by default (see
          railOpen's own comment) and revealed by this arrow — placed at the chart's own left edge,
          where the rail itself would sit, so it reads as "open the tool column" rather than a
          floating unrelated button. CSS (.oac-host:not(.oac-rail-open) .oac-rail) does the actual
          hiding; this only flips the class and the arrow's own direction. */}
      <button
        type="button"
        className={'oac-rail-toggle' + (railOpen ? ' is-open' : '')}
        onClick={() => setRailOpen((v) => !v)}
        title={railOpen ? 'Hide drawing tools' : 'Show drawing tools'}
      >
        {railOpen ? '‹' : '›'}
      </button>
      {/* The widget's own Indicators button lives on its topbar, which this chart keeps hidden
          (see the widget-creation effect's own comment on why) — this is that entry point,
          reachable without pulling in a symbol/interval picker wired to no feed. Sits in the SAME
          row as the parent's timeframe tabs, right after them (2026-09-21 request — was a second
          row below, now one row: chart-bar-tabs-overlay-openalgo's own left offset plus its
          rendered width is what `left` here lines up after). */}
      <button
        type="button"
        className="oac-indicators-trigger"
        onClick={() => widgetRef.current?.openIndicatorPicker()}
        title="Add an indicator"
      >
        Indicators
      </button>
      {/* OHLCV readout (2026-09-21 request — "show candle info like every exchange does"). */}
      {displayCandle && (
        <div className="oac-ohlcv">
          <span className="oac-ohlcv-date">{shortTime(new Date(displayCandle.time * 1000).toISOString())}</span>
          <span className="oac-ohlcv-item">
            <span className="oac-ohlcv-label">O</span>
            <b style={{ color: displayUp ? UP : DOWN }}>{fmt(String(displayCandle.open))}</b>
          </span>
          <span className="oac-ohlcv-item">
            <span className="oac-ohlcv-label">H</span>
            <b style={{ color: displayUp ? UP : DOWN }}>{fmt(String(displayCandle.high))}</b>
          </span>
          <span className="oac-ohlcv-item">
            <span className="oac-ohlcv-label">L</span>
            <b style={{ color: displayUp ? UP : DOWN }}>{fmt(String(displayCandle.low))}</b>
          </span>
          <span className="oac-ohlcv-item">
            <span className="oac-ohlcv-label">C</span>
            <b style={{ color: displayUp ? UP : DOWN }}>{fmt(String(displayCandle.close))}</b>
          </span>
          {displayChangePct !== null && (
            <span className="oac-ohlcv-item">
              <b style={{ color: displayUp ? UP : DOWN }}>
                {displayChangePct >= 0 ? '+' : ''}
                {displayChangePct.toFixed(2)}%
              </b>
            </span>
          )}
          <span className="oac-ohlcv-item">
            <span className="oac-ohlcv-label">Vol</span>
            <b className="text-dim">{fmtVolume(displayCandle.volume)}</b>
          </span>
        </div>
      )}
      {hover && (
        <div
          className="chart-tooltip"
          style={{
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

// Re-exported for parity with CandleChart.tsx's own export shape, in case a future caller wants
// the raw grab tolerance constant without reaching into OpenAlgoZones directly.
export { GRAB_TOLERANCE_PX }
