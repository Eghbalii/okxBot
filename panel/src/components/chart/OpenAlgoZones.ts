import type { Chart, IPrimitive, PriceScale, PrimitiveHit, PrimitiveRenderContext, ZOrder } from 'openalgo-charts'

/**
 * OpenAlgo-engine port of PositionZones.ts (2026-09-21) — same rendering contract
 * (ZonePosition), same visual design (risk/reward bands, entry line, ghost of the saved level
 * while dragging, grab handles), reimplemented against this engine's IPrimitive/PriceScale/
 * TimeScale API instead of lightweight-charts'. Kept as a SEPARATE file rather than a shared
 * renderer: the two engines' coordinate APIs (`priceToY`/`yToPrice` vs `priceToCoordinate`/
 * `coordinateToPrice`, `timeToCoordinate` as a TimeScale method vs a getter on the chart) differ
 * enough that a shared abstraction would cost more than the ~150 lines of duplication it would
 * remove, and CLAUDE.md's own precedent (RealizedPnL, clampSLForLoss) is to duplicate small pure
 * logic across independent paths rather than force a coupling between them.
 */
export interface ZonePosition {
  id: number
  side: 'buy' | 'sell'
  entry: number
  sl: number | null
  tp: number | null
  openTime: number
  closeTime: number | null
  leverage: number
  pendingSl?: number | null
  pendingTp?: number | null
  editable?: boolean
  labelMode?: 'price' | 'pct'
}

const RISK_FILL = 'rgba(246, 70, 93, 0.16)'
const REWARD_FILL = 'rgba(46, 189, 133, 0.10)'
const RISK_EDGE = 'rgba(246, 70, 93, 0.55)'
const REWARD_EDGE = 'rgba(46, 189, 133, 0.55)'
const ENTRY_EDGE = 'rgba(190, 195, 205, 0.75)'
const LABEL_RED = '#f6465d'
const LABEL_GREEN = '#2ebd85'
const LABEL_TEXT = '#ffffff'

const MIN_BOX_W = 24
const HANDLE_R = 5
export const GRAB_TOLERANCE_PX = 7
const PENDING_EDGE = 'rgba(255,255,255,0.9)'
const SAVED_GHOST = 'rgba(150,155,165,0.5)'

// Same formulas as components/PositionZones.ts — kept in sync deliberately; see that file's own
// doc comment for the derivation. Exported here too so OpenAlgoCandleChart's drag handler and
// ChartAdjustPanel's price/pct switch read from one definition per engine rather than importing
// across the engine boundary.
export function pctOnMargin(entry: number, target: number, side: 'buy' | 'sell', leverage: number): number {
  if (!entry) return 0
  const lev = leverage > 0 ? leverage : 1
  const direction = side === 'buy' ? 1 : -1
  return (direction * (target - entry) * 100 * lev) / entry
}

export function formatLevelPrice(v: number): string {
  if (!Number.isFinite(v)) return ''
  if (Math.abs(v) >= 1) return v.toFixed(2)
  const s = v.toFixed(12).replace(/0+$/, '')
  const lead = /^0\.(0*)/.exec(s)
  return v.toFixed(Math.min((lead ? lead[1].length : 0) + 4, 10))
}

export function formatPct(pct: number): string {
  const truncated = Math.trunc(pct * 10) / 10
  return `${truncated > 0 ? '+' : ''}${truncated.toFixed(1)}%`
}

/**
 * Draws each position as a bounded risk/reward box, exactly like the TradingView-engine version.
 * `zOrder: 'bottom'` keeps candles readable over the fill. Coordinates are resolved in `draw`
 * every frame (never cached) since the mapping changes on every pan/zoom/new bar.
 *
 * Drag support: `hitTest` reports `draggable: true` with `externalId` 'zone:<id>:sl' or
 * 'zone:<id>:tp' when the cursor is within GRAB_TOLERANCE_PX of an editable position's handle —
 * `Chart.subscribeDrag` (a first-class engine API) then delivers the live price for that id
 * without this primitive needing to touch pointer events itself.
 */
export class PositionZones implements IPrimitive {
  private positions: readonly ZonePosition[] = []
  // Retained only for hitTest's price->y math (drag hit-testing runs outside draw()'s own rc);
  // the time-axis scales are used solely inside draw() via the rc argument passed there.
  private priceScale: PriceScale | null = null

  setPositions(positions: readonly ZonePosition[]): void {
    this.positions = positions
  }

  zOrder(): ZOrder {
    return 'bottom'
  }

  // Without this, the pane autoscales to the visible CANDLES only, so a stop or target sitting
  // outside the candles' own high/low range gets silently clipped to whatever range the price axis
  // happened to land on — this is the reported bug ("highlights for open positions shown wrong"):
  // the risk/reward boxes were being drawn, but their fill/edge lines beyond the autoscaled range
  // never appeared, and the visible sliver read as a badly-placed box rather than a correctly-drawn
  // one that just runs off both ends of the chart. TradingView's engine (CandleChart.tsx) does not
  // need this because lightweight-charts' own primitives participate in autoscale by default; this
  // engine requires a primitive to opt in explicitly via `autoscaleInfo()`.
  autoscaleInfo(): { min: number; max: number } | null {
    if (this.positions.length === 0) return null
    let min = Infinity
    let max = -Infinity
    for (const p of this.positions) {
      for (const price of [p.entry, p.sl, p.tp, p.pendingSl, p.pendingTp]) {
        if (price === null || price === undefined || !Number.isFinite(price)) continue
        if (price < min) min = price
        if (price > max) max = price
      }
    }
    return Number.isFinite(min) && Number.isFinite(max) ? { min, max } : null
  }

  // Time -> pixel, via a logical index and then TimeScale.indexToX (index -> media px). TimeScale
  // itself has no time-to-x method, only index-to-x; Chart exposes a convenience timeToCoordinate
  // but that is chart-level and not reachable from inside a primitive's draw(), which only
  // receives the pane's own scales.
  //
  // `timeToIndex` FIRST, `timeToIndexFloat` only as a fallback: the exact-bar lookup is what the
  // engine's own SeriesMarkers renderer uses (`dataLayer.timeToIndex(marker.time)`), so resolving
  // the same instant the same way is what keeps a position's entry marker and its zone box pinned
  // to the same candle. The float version interpolates/extrapolates, which is right for a time
  // between or beyond bars but would silently place the box a fraction of a bar away from its own
  // marker for a time that does have a bar. Both are fed the same `snapToBar`-aligned timestamp
  // from OpenAlgoCandleChart, so the exact lookup is expected to hit on every real position.
  private timeToX(rc: PrimitiveRenderContext, time: number): number {
    const exact = rc.dataLayer.timeToIndex(time)
    const index = exact !== undefined ? exact : rc.dataLayer.timeToIndexFloat(time)
    return rc.timeScale.indexToX(index)
  }

  draw(ctx: CanvasRenderingContext2D, rc: PrimitiveRenderContext): void {
    this.priceScale = rc.priceScale
    const { plotWidth, plotHeight } = rc

    for (const p of this.positions) {
      const entryY = rc.priceScale.priceToY(p.entry)
      const x1raw = this.timeToX(rc, p.openTime)
      // An OPEN position's box runs to the right edge of the plot, full stop. It used to be
      // `Math.min(x1 + Math.max(OPEN_BOX_OVERHANG, plotWidth), plotWidth)` — copied from the
      // TradingView-engine PositionZones (where `OPEN_BOX_OVERHANG` still lives), and correct
      // only there because lightweight-charts' `timeToCoordinate` returns NULL for a time outside
      // the loaded range and that renderer skips the position entirely. This engine's
      // `indexToX` instead EXTRAPOLATES, so a position
      // that opened before the leftmost visible candle yields a large NEGATIVE x1, and
      // `x1 + plotWidth` then lands somewhere in the middle of the chart: the box stopped early,
      // nowhere near "now", while the trade was still open (reported 2026-09-21 with a STRK
      // position opened 11:10 whose box died at ~16:15).
      const x2 = p.closeTime === null ? plotWidth : this.timeToX(rc, p.closeTime)
      // Clamped so a position that opened before the leftmost visible bar starts at the plot's
      // left edge rather than at an off-screen coordinate: the fill would be clipped identically
      // either way, but the dashed level lines and the price labels are drawn relative to `left`,
      // and without this they'd be placed off-canvas and simply vanish.
      //
      // Note this clamp also HIDES a wrong x1 (anything negative lands at 0), so when debugging a
      // placement complaint, log `x1raw` — not the drawn edge. Measuring the rendered fill is
      // worse still: the risk band is translucent red over a dark background, which a naive
      // "first reddish pixel" scan cannot tell apart from an ordinary red candle body, and that
      // mistake produced two bogus bug reports while chasing this.
      const x1 = Math.max(x1raw, 0)

      const left = Math.min(x1, x2)
      const right = Math.max(x1, x2)
      const width = Math.max(right - left, MIN_BOX_W)

      const band = (price: number | null, fill: string, edge: string) => {
        if (price === null) return
        const y = rc.priceScale.priceToY(price)
        const top = Math.min(entryY, y)
        const height = Math.abs(y - entryY)
        if (height < 0.5) return
        ctx.fillStyle = fill
        ctx.fillRect(left, top, width, height)
        ctx.strokeStyle = edge
        ctx.setLineDash([4, 3])
        ctx.lineWidth = 1
        ctx.beginPath()
        ctx.moveTo(left, y)
        ctx.lineTo(left + width, y)
        ctx.stroke()
        ctx.setLineDash([])
      }

      const slShown = p.pendingSl !== undefined ? p.pendingSl : p.sl
      const tpShown = p.pendingTp !== undefined ? p.pendingTp : p.tp

      const ghost = (price: number | null, pending: number | null | undefined) => {
        if (pending === undefined || price === null || price === pending) return
        const y = rc.priceScale.priceToY(price)
        ctx.strokeStyle = SAVED_GHOST
        ctx.setLineDash([2, 4])
        ctx.lineWidth = 1
        ctx.beginPath()
        ctx.moveTo(left, y)
        ctx.lineTo(left + width, y)
        ctx.stroke()
        ctx.setLineDash([])
      }

      band(slShown, RISK_FILL, RISK_EDGE)
      band(tpShown, REWARD_FILL, REWARD_EDGE)
      ghost(p.sl, p.pendingSl)
      ghost(p.tp, p.pendingTp)

      ctx.strokeStyle = ENTRY_EDGE
      ctx.lineWidth = 1
      ctx.beginPath()
      ctx.moveTo(left, entryY)
      ctx.lineTo(left + width, entryY)
      ctx.stroke()

      this.label(ctx, tpShown, p, left + width, LABEL_GREEN, plotWidth, plotHeight)
      this.label(ctx, slShown, p, left + width, LABEL_RED, plotWidth, plotHeight)

      if (p.editable) {
        const handle = (price: number | null, color: string) => {
          if (price === null) return
          const y = rc.priceScale.priceToY(price)
          ctx.beginPath()
          ctx.arc(left + 10, y, HANDLE_R, 0, Math.PI * 2)
          ctx.fillStyle = color
          ctx.fill()
          ctx.strokeStyle = PENDING_EDGE
          ctx.lineWidth = 1.5
          ctx.stroke()
        }
        handle(slShown, LABEL_RED)
        handle(tpShown, LABEL_GREEN)
      }
    }
  }

  private label(
    ctx: CanvasRenderingContext2D,
    price: number | null,
    p: ZonePosition,
    xRight: number,
    color: string,
    plotWidth: number,
    plotHeight: number,
  ): void {
    if (price === null || !this.priceScale) return
    const y = this.priceScale.priceToY(price)
    const text = p.labelMode === 'price' ? formatLevelPrice(price) : formatPct(pctOnMargin(p.entry, price, p.side, p.leverage))
    ctx.font = '11px ui-sans-serif, system-ui, sans-serif'
    const padX = 5
    const w = ctx.measureText(text).width + padX * 2
    const h = 16
    let x = xRight - w
    if (x + w > plotWidth) x = plotWidth - w - 2
    if (x < 0) x = 0
    const top = Math.min(Math.max(y - h / 2, 0), plotHeight - h)

    ctx.fillStyle = color
    ctx.fillRect(x, top, w, h)
    ctx.fillStyle = LABEL_TEXT
    ctx.textBaseline = 'middle'
    ctx.fillText(text, x + padX, top + h / 2)
  }

  // Only the editable (currently-being-edited) position's handles are grabbable — matches the
  // TradingView-engine PositionZones, which draws handles for exactly that one position.
  hitTest(_x: number, y: number): PrimitiveHit | null {
    if (!this.priceScale) return null
    for (const p of this.positions) {
      if (!p.editable) continue
      const slShown = p.pendingSl !== undefined ? p.pendingSl : p.sl
      const tpShown = p.pendingTp !== undefined ? p.pendingTp : p.tp
      for (const [which, price] of [
        ['sl', slShown],
        ['tp', tpShown],
      ] as const) {
        if (price === null) continue
        const ly = this.priceScale.priceToY(price)
        const dist = Math.abs(ly - y)
        if (dist <= GRAB_TOLERANCE_PX) {
          return {
            externalId: `zone:${p.id}:${which}`,
            zOrder: 'bottom',
            distance: dist,
            cursor: 'ns-resize',
            draggable: true,
          }
        }
      }
    }
    return null
  }
}

/** Parses a subscribeDrag externalId of the form `zone:<id>:sl|tp` produced by hitTest above. */
export function parseZoneDragId(externalId: string): { id: number; which: 'sl' | 'tp' } | null {
  const m = /^zone:(-?\d+):(sl|tp)$/.exec(externalId)
  if (!m) return null
  return { id: Number(m[1]), which: m[2] as 'sl' | 'tp' }
}

/** Attaches the primitive to pane 0 of the given chart and returns the handle to drive it. */
export function attachPositionZones(chart: Chart): PositionZones {
  const zones = new PositionZones()
  chart.addPrimitive(zones, 0)
  return zones
}
