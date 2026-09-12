import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../api/client'
import type { Candle, Position, PositionMode } from '../api/types'
import { useLiveCandles } from '../hooks/useLiveCandles'
import { CandleChart } from './CandleChart'
import ChartAdjustPanel, { type LevelMode } from './ChartAdjustPanel'
import { pctOnMargin } from './PositionZones'

// How often the stored series is refetched. Live movement now arrives over the WebSocket, so this
// is purely a consistency/backfill pass — it reconciles history and covers a dropped socket, and
// does not drive anything the user watches. 60s rather than 15s because each call reads 500 rows
// per open chart, and nothing waits on it any more.
const CANDLE_REFETCH_MS = 60_000

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

  // Refetch the finalized series on an interval as well as on (instId, bar), so a bar that closes
  // while the modal is open shows up as a real candle. The series is cleared to null ONLY when the
  // identity changes, never on a refetch — blanking it every 15s would drop the chart back to its
  // "Loading candles…" state and (worse) remount CandleChart, discarding the user's pan/zoom.
  const seriesKey = `${instId}:${bar}`
  const seriesKeyRef = useRef(seriesKey)
  useEffect(() => {
    let cancelled = false
    if (seriesKeyRef.current !== seriesKey) {
      seriesKeyRef.current = seriesKey
      setCandles(null)
    }
    setError(null)

    async function load() {
      try {
        const c = await api.candles({ instId, bar, limit: 500 })
        if (!cancelled) setCandles(c)
      } catch (e: unknown) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      }
    }

    load()
    const id = setInterval(load, CANDLE_REFETCH_MS)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [instId, bar, seriesKey])

  // Folds OKX's own live candle pushes into the series (over the same WebSocket the price comes
  // from), so the forming bar carries the exchange's real OHLC instead of extremes reconstructed
  // from whichever ticks this browser happened to receive.
  const liveCandles = useLiveCandles(candles, instId, bar)

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

  // --- chart-side SL/TP editing (2026-09-12 request) -----------------------------------------
  // Only a REAL, still-open position can be edited: the endpoint is real-only by construction
  // (paper ids route to a table with no manual-edit path) and a closed position has no resting
  // order on the exchange to amend.
  const editable = useMemo(
    () => shown.filter((p) => !p.ClosedAt && mode === 'real'),
    [shown, mode],
  )
  const [editId, setEditId] = useState<number | null>(null)
  const target = editable.find((p) => p.ID === editId) ?? editable[0] ?? null

  // Pending levels are held as PRICES — the unit the chart draws and the exchange enforces;
  // percentage is only a display of it. null means "being cleared", undefined means "untouched".
  const [pendingSl, setPendingSl] = useState<number | null>(null)
  const [pendingTp, setPendingTp] = useState<number | null>(null)
  const [levelMode, setLevelMode] = useState<LevelMode>('price')

  const savedSl = target?.SLPx ? Number(target.SLPx) : null
  const savedTp = target?.TPPx ? Number(target.TPPx) : null

  // Re-seed from the saved levels when the edited position changes — the fields show what is
  // actually in force rather than starting blank (the operator's explicit difference from the
  // table's modal).
  const seededFor = useRef<number | null>(null)
  useEffect(() => {
    if (target === null) {
      seededFor.current = null
      return
    }
    if (seededFor.current === target.ID) return
    seededFor.current = target.ID
    setPendingSl(target.SLPx ? Number(target.SLPx) : null)
    setPendingTp(target.TPPx ? Number(target.TPPx) : null)
  }, [target])

  // Compared with a tolerance rather than exactly: a dragged level is a float derived from a pixel,
  // so it is never bit-identical to the stored value even when visually unchanged.
  const near = (a: number | null, b: number | null) => {
    if (a === null || b === null) return a === b
    if (!Number.isFinite(a) || !Number.isFinite(b)) return false
    return Math.abs(a - b) <= Math.abs(b || a) * 1e-9
  }
  const dirty = target !== null && (!near(pendingSl, savedSl) || !near(pendingTp, savedTp))

  const onDragLevel = useCallback((which: 'sl' | 'tp', price: number) => {
    if (which === 'sl') setPendingSl(price)
    else setPendingTp(price)
  }, [])

  const onChangeLevel = useCallback((which: 'sl' | 'tp', price: number | null) => {
    if (which === 'sl') setPendingSl(price)
    else setPendingTp(price)
  }, [])

  const resetLevels = useCallback(() => {
    setPendingSl(savedSl)
    setPendingTp(savedTp)
  }, [savedSl, savedTp])

  // The backend takes a SIGNED MARGIN PERCENTAGE, not a price (internal/api.priceFromMarginPct), so
  // whatever unit was typed or dragged is converted back here. pctOnMargin is that formula's exact
  // inverse — verified to round-trip for every side/sign/leverage, including tiny-priced tokens.
  async function submitLevels() {
    if (target === null) return
    const entry = Number(target.EntryPx)
    const lev = Number(target.Leverage) || 1
    const body: { slPct?: number; tpPct?: number } = {}
    if (!near(pendingSl, savedSl) && pendingSl !== null) {
      body.slPct = pctOnMargin(entry, pendingSl, target.Side, lev)
    }
    if (!near(pendingTp, savedTp) && pendingTp !== null) {
      body.tpPct = pctOnMargin(entry, pendingTp, target.Side, lev)
    }
    if (body.slPct === undefined && body.tpPct === undefined) return
    const res = await api.adjustPosition(target.ID, mode, body)
    // Adopt what the exchange actually stored rather than what was requested: the backend rounds to
    // the instrument's tick size (§38), so the panel would otherwise keep showing an unrounded
    // value and read as dirty forever.
    setPendingSl(res.slPx ? Number(res.slPx) : null)
    setPendingTp(res.tpPx ? Number(res.tpPx) : null)
    seededFor.current = null // let the next positions refetch re-seed from the stored values
  }

  // Closing with unsaved level changes asks first (explicit request) — a dragged stop that was
  // never sent is a silent, dangerous no-op otherwise.
  const requestClose = useCallback(() => {
    if (
      dirty &&
      !window.confirm(
        'You changed SL/TP but did not press Update, so nothing was sent to the exchange.\n\nLeave anyway and discard the changes?',
      )
    ) {
      return
    }
    onClose()
  }, [dirty, onClose])

  useEffect(() => {
    const onResize = () => setChartHeight(availableChartHeight())
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') requestClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [requestClose])

  return (
    <div className="modal-backdrop" onClick={requestClose}>
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
          {editable.length > 1 && (
            // Only shown when there is a real choice to make — with one open position the chart
            // already makes it obvious which is being edited.
            <select
              className="chart-pos-select"
              value={target?.ID ?? ''}
              onChange={(e) => setEditId(Number(e.target.value))}
              title="Which position to edit"
            >
              {editable.map((p) => (
                <option key={p.ID} value={p.ID}>
                  #{p.ID} {p.Side === 'buy' ? 'long' : 'short'}
                </option>
              ))}
            </select>
          )}
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
          <button className="btn" onClick={requestClose} style={{ marginLeft: 8 }}>
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
        {!error && liveCandles !== null && liveCandles.length > 0 && (
          <>
            <div
              className={'chart-with-panel' + (target ? ' has-panel' : '')}
              // The row is sized to the chart so the side panel's `align-self: stretch` has a
              // definite height to match — without it the panel would resolve to its own content.
              style={{ height: chartHeight }}
            >
              <div className="chart-main">
                <CandleChart
                  candles={liveCandles}
                  positions={shown}
                  height={chartHeight}
                  frameKey={bar}
                  editPositionId={target?.ID ?? null}
                  editSl={pendingSl}
                  editTp={pendingTp}
                  editLabelMode={levelMode}
                  onDragLevel={onDragLevel}
                />
              </div>
              {target && (
                <ChartAdjustPanel
                  position={target}
                  sl={pendingSl}
                  tp={pendingTp}
                  mode={levelMode}
                  onModeChange={setLevelMode}
                  onChange={onChangeLevel}
                  onSubmit={submitLevels}
                  onReset={resetLevels}
                  dirty={dirty}
                />
              )}
            </div>
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
