import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import type { Candle, Position, PositionMode } from '../api/types'
import { CandleChart } from './CandleChart'

// The decision timeframes (CLAUDE.md §9). 4H/1D are collected for context but a chart of them
// carries almost no position history at this trade cadence, so they are not offered here.
const BARS = ['5m', '15m', '1H'] as const

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

  // Only this token's orders, and only those whose own bar matches the chart — a 1H order drawn on
  // a 5m chart would sit at a timestamp the 5m candles never had.
  const shown = useMemo(
    () => positions.filter((p) => p.InstID === instId && (p.Bar === '' || p.Bar === bar)),
    [positions, instId, bar],
  )

  const openCount = shown.filter((p) => !p.ClosedAt).length

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
        style={{ maxWidth: '1040px', width: '94vw' }}
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
            <CandleChart candles={candles} positions={shown} height={460} frameKey={bar} />
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
