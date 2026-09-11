import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import type { Candle, Position, PositionMode } from '../api/types'
import { CandleChart } from './CandleChart'

// The decision timeframes (CLAUDE.md §9). 4H/1D are collected for context but a chart of them
// carries almost no position history at this trade cadence, so they are not offered here.
const BARS = ['5m', '15m', '1H'] as const

// The modal is capped at 92vh; the header, legend, and the modal's own padding take roughly a
// fixed 150px of that, so the chart gets whatever is left. Clamped at both ends: below ~300px
// candles stop being readable, and above ~760px the chart stretches past what is useful.
function availableChartHeight(): number {
  const available = window.innerHeight * 0.92 - 150
  return Math.round(Math.min(Math.max(available, 300), 760))
}

/**
 * Candles for one token with its own position history drawn on top.
 *
 * Positions are filtered client-side from the page's already-loaded rows rather than refetched:
 * the caller is the positions table, so it necessarily has them, and a second request would show
 * a different slice than the table the user just clicked.
 */
export default function TokenChartModal({
  instId,
  mode,
  positions,
  onClose,
}: {
  instId: string
  mode: PositionMode
  positions: Position[]
  onClose: () => void
}) {
  const [bar, setBar] = useState<string>('5m')
  const [chartHeight, setChartHeight] = useState(() => availableChartHeight())
  const [candles, setCandles] = useState<Candle[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setCandles(null)
    setError(null)
    api
      .candles({ instId, bar, limit: 500 })
      .then((c) => {
        if (!cancelled) setCandles(c)
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      })
    return () => {
      cancelled = true
    }
  }, [instId, bar])

  // Closed orders are filtered to the chart's own timeframe: a 1H order's entry sits at a
  // timestamp the 5m candles never had, so its marker would land on the wrong candle.
  //
  // An OPEN position is deliberately exempt (2026-09-11). Its entry, stop and target are live
  // price levels — they are true at this instant regardless of which timeframe produced the
  // signal, and hiding them meant switching to 1H made a position you actually hold disappear.
  // Its entry marker may sit a little off the exact bar on a coarser chart; that is a far smaller
  // problem than not seeing the position at all.
  const shown = useMemo(
    () =>
      positions.filter(
        (p) => p.InstID === instId && (!p.ClosedAt || p.Bar === '' || p.Bar === bar),
      ),
    [positions, instId, bar],
  )

  const openCount = shown.filter((p) => !p.ClosedAt).length

  useEffect(() => {
    const onResize = () => setChartHeight(availableChartHeight())
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div
        className="modal"
        // Sized against the viewport so the whole thing — header, chart, legend — fits in one
        // screen without the modal's own 85vh scrollbar. Previously the chart was a hardcoded
        // 460px, which overflowed on a shorter display and hid the legend below the fold.
        // overflow hidden because the chart is already sized to fit: the shared .modal rule scrolls
        // at 85vh, and letting it scroll here would reintroduce exactly the "can't see it all at
        // once" problem this sizing exists to solve.
        style={{ maxWidth: '1280px', width: '95vw', maxHeight: '92vh', overflow: 'hidden' }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="chart-panel-head">
          <h3 style={{ margin: 0 }}>{instId}</h3>
          <span className="badge badge-dim">{mode}</span>
          <span style={{ fontSize: 12, color: '#858b96' }}>
            {shown.length} order{shown.length === 1 ? '' : 's'}
            {openCount > 0 ? ` · ${openCount} open` : ''}
          </span>
          <div className="chart-bar-tabs">
            {BARS.map((b) => (
              <button
                key={b}
                className={`chart-bar-tab${b === bar ? ' active' : ''}`}
                onClick={() => setBar(b)}
              >
                {b}
              </button>
            ))}
          </div>
          <button className="btn" onClick={onClose} style={{ marginLeft: 8 }}>
            Close
          </button>
        </div>

        {error && <p className="error">Could not load candles: {error}</p>}
        {!error && candles === null && <p style={{ color: '#858b96' }}>Loading candles…</p>}
        {!error && candles !== null && candles.length === 0 && (
          <p style={{ color: '#858b96' }}>
            No candles stored for {instId} on {bar} yet.
          </p>
        )}
        {!error && candles !== null && candles.length > 0 && (
          <>
            <CandleChart candles={candles} positions={shown} height={chartHeight} frameKey={bar} />
            <div className="chart-legend">
              <span>
                <i style={{ background: '#2ebd85' }} />
                entry long / profitable exit
              </span>
              <span>
                <i style={{ background: '#f6465d' }} />
                entry short / losing exit
              </span>
              <span>
                <i style={{ background: 'rgba(246,70,93,0.35)' }} />
                open risk to SL
              </span>
              <span>
                <i style={{ background: 'rgba(46,189,133,0.35)' }} />
                open room to TP
              </span>
              <span>hover a marker for detail</span>
            </div>
          </>
        )}
      </div>
    </div>
  )
}
