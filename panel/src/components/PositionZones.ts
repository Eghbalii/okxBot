import type {
  ISeriesApi,
  ISeriesPrimitive,
  IPrimitivePaneView,
  IPrimitivePaneRenderer,
  SeriesType,
  Time,
} from 'lightweight-charts'

/** One open position's entry plus the ground it still has to cover to reach SL and TP. */
export interface ZonePosition {
  id: number
  side: 'buy' | 'sell'
  entry: number
  sl: number | null
  tp: number | null
}

// Deliberately translucent: these are bands drawn UNDER the candles (zOrder 'bottom'), and an
// opaque fill would hide the price action the zones exist to give context to.
// A take-profit is typically several times further from entry than the stop (a real open position
// measured 3.35% to TP against 0.60% to SL), so the reward band covers far more of the pane. At an
// equal alpha the green stopped reading as a zone and started reading as a background wash, so the
// reward fill is deliberately lighter than the risk fill — the two are not meant to be visually
// equal-weighted anyway: the risk band is the one worth drawing the eye.
const RISK_FILL = 'rgba(246, 70, 93, 0.16)'
const REWARD_FILL = 'rgba(46, 189, 133, 0.07)'
const RISK_EDGE = 'rgba(246, 70, 93, 0.55)'
const REWARD_EDGE = 'rgba(46, 189, 133, 0.55)'
const ENTRY_EDGE = 'rgba(190, 195, 205, 0.75)'

/**
 * Fills the band between entry and SL in red, and entry to TP in green — the TradingView
 * position-tool look the operator asked for.
 *
 * Why a primitive rather than createPriceLine: price lines are 1px horizontal rules with no notion
 * of an area between two of them. The "how much room is left" reading comes from the filled band,
 * not from the two edges, so the shading IS the feature.
 *
 * Prices are converted to pixels inside draw() rather than cached, because the mapping changes on
 * every pan, zoom, and new candle; lightweight-charts calls draw() on each of those.
 */
class ZoneRenderer implements IPrimitivePaneRenderer {
  // Written as explicit fields rather than constructor parameter properties: this project's
  // tsconfig sets erasableSyntaxOnly, which rejects that TypeScript-only shorthand.
  private readonly series: ISeriesApi<SeriesType, Time>
  private readonly positions: readonly ZonePosition[]

  constructor(series: ISeriesApi<SeriesType, Time>, positions: readonly ZonePosition[]) {
    this.series = series
    this.positions = positions
  }

  draw(target: { useMediaCoordinateSpace: (f: (scope: { context: CanvasRenderingContext2D; mediaSize: { width: number; height: number } }) => void) => void }): void {
    target.useMediaCoordinateSpace(({ context: ctx, mediaSize }) => {
      for (const p of this.positions) {
        const entryY = this.series.priceToCoordinate(p.entry)
        if (entryY === null) continue

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
          ctx.fillRect(0, top, mediaSize.width, height)
          ctx.strokeStyle = edge
          ctx.setLineDash([4, 3])
          ctx.lineWidth = 1
          ctx.beginPath()
          ctx.moveTo(0, y)
          ctx.lineTo(mediaSize.width, y)
          ctx.stroke()
          ctx.setLineDash([])
        }

        band(p.sl, RISK_FILL, RISK_EDGE)
        band(p.tp, REWARD_FILL, REWARD_EDGE)

        ctx.strokeStyle = ENTRY_EDGE
        ctx.lineWidth = 1
        ctx.beginPath()
        ctx.moveTo(0, entryY)
        ctx.lineTo(mediaSize.width, entryY)
        ctx.stroke()
      }
    })
  }
}

class ZonePaneView implements IPrimitivePaneView {
  private readonly series: ISeriesApi<SeriesType, Time>
  private readonly positions: readonly ZonePosition[]

  constructor(series: ISeriesApi<SeriesType, Time>, positions: readonly ZonePosition[]) {
    this.series = series
    this.positions = positions
  }
  // 'bottom' keeps candles readable on top of the fill — the zones are context, not the subject.
  zOrder() {
    return 'bottom' as const
  }
  renderer() {
    return this.positions.length === 0 ? null : new ZoneRenderer(this.series, this.positions)
  }
}

/** Attach with series.attachPrimitive(...); call setPositions to update without re-creating. */
export class PositionZones implements ISeriesPrimitive<Time> {
  private series: ISeriesApi<SeriesType, Time> | null = null
  private positions: readonly ZonePosition[] = []
  private views: ZonePaneView[] = []

  attached(param: { series: ISeriesApi<SeriesType, Time> }): void {
    this.series = param.series
    this.rebuild()
  }

  detached(): void {
    this.series = null
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
    this.views = this.series ? [new ZonePaneView(this.series, this.positions)] : []
  }
}
