import type { Candle, Position } from '../../api/types'

/**
 * The prop contract every chart engine implementation must satisfy (2026-09-21).
 *
 * Extracted from CandleChart.tsx's own props rather than invented fresh — this is what
 * TokenChartModal/TradePage already pass today, so both engines can be swapped behind the same
 * call sites with zero change to either caller.
 */
export interface CandleChartProps {
  candles: Candle[]
  positions: Position[]
  height?: number
  frameKey?: string
  editPositionId?: number | null
  editSl?: number | null
  editTp?: number | null
  /** Price/% for the edited position's on-chart labels, following the side panel's own switch. */
  editLabelMode?: 'price' | 'pct'
  onDragLevel?: (which: 'sl' | 'tp', price: number) => void
}

export type ChartEngine = 'tradingview' | 'openalgo'
