import type {
  IChartApi,
  ISeriesApi,
  ISeriesPrimitive,
  IPrimitivePaneView,
  IPrimitivePaneRenderer,
  SeriesType,
  Time,
  UTCTimestamp,
} from 'lightweight-charts'

/** One position's entry plus the ground it still has to cover to reach SL and TP. */
export interface ZonePosition {
  id: number
  side: 'buy' | 'sell'
  entry: number
  sl: number | null
  tp: number | null
  /** Bar the position opened on, already snapped to the chart's own timeframe. */
  openTime: UTCTimestamp
  /** Bar it closed on, or null while open — the box then runs to the live edge. */
  closeTime: UTCTimestamp | null
  /** Leverage, so the labels can report the percentage actually realized on margin. */
  leverage: number
  /**
   * Unsaved levels being edited right now, drawn INSTEAD of sl/tp when set. The saved values stay
   * in sl/tp so the renderer can show where the level currently sits on the exchange (a faint
   * "original" line) while the operator moves the new one — without that, a drag gives no reference
   * for how far it has moved.
   */
  pendingSl?: number | null
  pendingTp?: number | null
  /** Draws the round grab handles; only the one position being edited is draggable. */
  editable?: boolean
}

// Translucent: these are drawn UNDER the candles (zOrder 'bottom'), and an opaque fill would hide
// the price action the zones exist to give context to. The reward fill is lighter than the risk
// fill on purpose — a take-profit is usually several times further from entry than the stop, so at
// equal alpha the green reads as a background wash rather than a zone, and the risk band is the one
// worth drawing the eye.
const RISK_FILL = 'rgba(246, 70, 93, 0.16)'
const REWARD_FILL = 'rgba(46, 189, 133, 0.10)'
const RISK_EDGE = 'rgba(246, 70, 93, 0.55)'
const REWARD_EDGE = 'rgba(46, 189, 133, 0.55)'
const ENTRY_EDGE = 'rgba(190, 195, 205, 0.75)'
const LABEL_RED = '#f6465d'
const LABEL_GREEN = '#2ebd85'
const LABEL_TEXT = '#ffffff'

// Minimum on-screen width for a box whose position opened and closed inside one bar, so it stays
// visible rather than collapsing to a hairline when zoomed out.
const MIN_BOX_W = 24
// How far past the last candle an OPEN position's box extends, in pixels. A fixed overhang rather
// than running to the pane edge: the point of bounding the box was that a full-width wash tells you
// nothing about WHEN the position is live.
const OPEN_BOX_OVERHANG = 56
// Radius of the round grab handle on an editable level, and the vertical slack around a level that
// still counts as grabbing it. The hit area is deliberately larger than the dot: a 1px line is far
// too small a target to hit reliably with a mouse.
const HANDLE_R = 5
export const GRAB_TOLERANCE_PX = 7
// A level being edited is drawn solid and brighter than a saved one, so "this is the value you are
// changing" is visible at a glance rather than inferred from the side panel.
const PENDING_EDGE = 'rgba(255,255,255,0.9)'
const SAVED_GHOST = 'rgba(150,155,165,0.5)'

/**
 * Draws each position as a bounded box — risk above/below entry in red, reward in green — spanning
 * only the time the position was actually open, with its stop and target labelled.
 *
 * Revised 2026-09-12: the bands used to be filled across the FULL chart width, which covered the
 * whole pane and said nothing about when the trade was live. Bounding them horizontally is the
 * TradingView position-tool look, and it makes several positions on one chart readable at once
 * instead of overlapping washes.
 *
 * Why a primitive rather than createPriceLine: price lines are 1px horizontal rules with no notion
 * of an area between two of them. The "how much room is left" reading comes from the filled band,
 * so the shading IS the feature.
 *
 * Prices and times are converted to pixels inside draw() rather than cached, because the mapping
 * changes on every pan, zoom, and new candle; lightweight-charts calls draw() on each of those.
 */
class ZoneRenderer implements IPrimitivePaneRenderer {
  // Written as explicit fields rather than constructor parameter properties: this project's
  // tsconfig sets erasableSyntaxOnly, which rejects that TypeScript-only shorthand.
  private readonly series: ISeriesApi<SeriesType, Time>
  private readonly chart: IChartApi
  private readonly positions: readonly ZonePosition[]

  constructor(
    series: ISeriesApi<SeriesType, Time>,
    chart: IChartApi,
    positions: readonly ZonePosition[],
  ) {
    this.series = series
    this.chart = chart
    this.positions = positions
  }

  draw(target: {
    useMediaCoordinateSpace: (
      f: (scope: {
        context: CanvasRenderingContext2D
        mediaSize: { width: number; height: number }
      }) => void,
    ) => void
  }): void {
    target.useMediaCoordinateSpace(({ context: ctx, mediaSize }) => {
      const timeScale = this.chart.timeScale()

      for (const p of this.positions) {
        const entryY = this.series.priceToCoordinate(p.entry)
        if (entryY === null) continue

        const x1 = timeScale.timeToCoordinate(p.openTime)
        if (x1 === null) continue
        // An open position has no close bar, so its box runs a fixed distance past the newest
        // candle. timeToCoordinate returns null for a time outside the current data range, which is
        // why the fallback is computed from the pane rather than from a time value.
        const x2raw =
          p.closeTime === null ? null : timeScale.timeToCoordinate(p.closeTime)
        const x2 =
          x2raw !== null
            ? x2raw
            : Math.min(x1 + Math.max(OPEN_BOX_OVERHANG, mediaSize.width), mediaSize.width)

        const left = Math.min(x1, x2)
        const right = Math.max(x1, x2)
        const width = Math.max(right - left, MIN_BOX_W)

        const band = (price: number | null, fill: string, edge: string) => {
          if (price === null) return
          const y = this.series.priceToCoordinate(price)
          if (y === null) return
          const top = Math.min(entryY, y)
          const height = Math.abs(y - entryY)
          // A zero-height band is a level already reached; skip rather than draw a hairline that
          // reads as a real zone.
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

        // The level actually drawn is the pending (being-edited) one when there is one, so the box
        // and its label track the mouse live rather than snapping only on save.
        const slShown = p.pendingSl !== undefined ? p.pendingSl : p.sl
        const tpShown = p.pendingTp !== undefined ? p.pendingTp : p.tp

        // Where the level currently sits on the exchange, drawn faintly behind an edit so there is
        // a reference for how far it has been moved. Skipped when nothing is pending.
        const ghost = (price: number | null, pending: number | null | undefined) => {
          if (pending === undefined || price === null || price === pending) return
          const y = this.series.priceToCoordinate(price)
          if (y === null) return
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

        // Labels carry ONLY the leverage-adjusted percentage (2026-09-12 request). The raw price is
        // already on the axis and in the table, whereas the percentage actually realized on margin
        // is the number that is not obtainable by eye — a 0.6% move at 20x is a 12% outcome.
        this.label(ctx, tpShown, p.entry, p, left + width, LABEL_GREEN, mediaSize)
        this.label(ctx, slShown, p.entry, p, left + width, LABEL_RED, mediaSize)

        // Grab handles, drawn only for the position being edited so a chart showing many trades
        // does not sprout dots on all of them.
        if (p.editable) {
          const handle = (price: number | null, color: string) => {
            if (price === null) return
            const y = this.series.priceToCoordinate(price)
            if (y === null) return
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
    })
  }

  private label(
    ctx: CanvasRenderingContext2D,
    price: number | null,
    entry: number,
    p: ZonePosition,
    xRight: number,
    color: string,
    mediaSize: { width: number; height: number },
  ): void {
    if (price === null) return
    const y = this.series.priceToCoordinate(price)
    if (y === null) return

    const text = formatPct(pctOnMargin(entry, price, p.side, p.leverage))
    ctx.font = '11px ui-sans-serif, system-ui, sans-serif'
    const padX = 5
    const w = ctx.measureText(text).width + padX * 2
    const h = 16
    // Sits just inside the box's right edge, flipping inward when that would overflow the pane so a
    // label never renders half off-screen.
    let x = xRight - w
    if (x + w > mediaSize.width) x = mediaSize.width - w - 2
    if (x < 0) x = 0
    const top = Math.min(Math.max(y - h / 2, 0), mediaSize.height - h)

    ctx.fillStyle = color
    ctx.fillRect(x, top, w, h)
    ctx.fillStyle = LABEL_TEXT
    ctx.textBaseline = 'middle'
    ctx.fillText(text, x + padX, top + h / 2)
  }
}

// Percentage of MARGIN gained or lost if price reaches `target`, which is what the operator asked
// the labels to show. Mirrors usecase.unrealizedPnLPct Go-side and PositionsPage's own slTpPct:
// the raw price move scaled by leverage, signed by direction — so a stop reads negative and a
// target positive on a coherent order, whether long or short.
export function pctOnMargin(
  entry: number,
  target: number,
  side: 'buy' | 'sell',
  leverage: number,
): number {
  if (!entry) return 0
  const lev = leverage > 0 ? leverage : 1
  const direction = side === 'buy' ? 1 : -1
  return (direction * (target - entry) * 100 * lev) / entry
}

// One decimal, with an explicit sign so a target and a stop are distinguishable in isolation —
// matching the one-decimal convention the positions table already uses for these same numbers.
export function formatPct(pct: number): string {
  const truncated = Math.trunc(pct * 10) / 10
  return `${truncated > 0 ? '+' : ''}${truncated.toFixed(1)}%`
}

class ZonePaneView implements IPrimitivePaneView {
  private readonly series: ISeriesApi<SeriesType, Time>
  private readonly chart: IChartApi
  private readonly positions: readonly ZonePosition[]

  constructor(
    series: ISeriesApi<SeriesType, Time>,
    chart: IChartApi,
    positions: readonly ZonePosition[],
  ) {
    this.series = series
    this.chart = chart
    this.positions = positions
  }
  // 'bottom' keeps candles readable on top of the fill — the zones are context, not the subject.
  zOrder() {
    return 'bottom' as const
  }
  renderer() {
    return this.positions.length === 0
      ? null
      : new ZoneRenderer(this.series, this.chart, this.positions)
  }
}

/** Attach with series.attachPrimitive(...); call setPositions to update without re-creating. */
export class PositionZones implements ISeriesPrimitive<Time> {
  private series: ISeriesApi<SeriesType, Time> | null = null
  private chart: IChartApi | null = null
  private positions: readonly ZonePosition[] = []
  private views: ZonePaneView[] = []

  attached(param: { series: ISeriesApi<SeriesType, Time>; chart: IChartApi }): void {
    this.series = param.series
    this.chart = param.chart
    this.rebuild()
  }

  detached(): void {
    this.series = null
    this.chart = null
    this.views = []
  }

  setPositions(positions: readonly ZonePosition[]): void {
    this.positions = positions
    this.rebuild()
  }

  paneViews(): readonly IPrimitivePaneView[] {
    return this.views
  }

  updateAllViews(): void {
    // Coordinates are resolved at draw time, so nothing is cached to invalidate here.
  }

  private rebuild(): void {
    this.views =
      this.series && this.chart
        ? [new ZonePaneView(this.series, this.chart, this.positions)]
        : []
  }
}
