import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import { useCachedResource } from '../hooks/useCachedResource'
import type { Position, PositionMode } from '../api/types'
import { useLiveCandles } from '../hooks/useLiveCandles'
import { CandleChart } from './CandleChart'
import ChartAdjustPanel, { type LevelMode } from './ChartAdjustPanel'
import { pctOnMargin } from './PositionZones'
import { tokenSymbol } from '../utils/format'
import { unrealizedPnL } from '../lib/pnl'

// Colour by sign, dimmed when there is no live price yet rather than defaulting to green/red —
// "unknown" and "flat" are different states and should not look the same.
function pnlClass(pct: number | null): string {
  if (pct === null) return 'text-dim'
  if (pct > 0) return 'text-green'
  if (pct < 0) return 'text-red'
  return 'text-dim'
}

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
  // Overhead measured against the current header/legend rather than the 150 this carried from an
  // older layout: modal padding 40 + the position strip ~44 + the row gap 12 + the legend ~46.
  // Rounded up a little for safety — undershooting costs a few pixels of chart, overshooting costs
  // a scrollbar, and those are not equally bad.
  const available = window.innerHeight * 0.92 - 148
  // Ceiling raised 760 -> 900 (2026-09-12 request): on a tall display the chart was being held
  // well below the space actually free, which is what left the side panel looking mismatched.
  return Math.round(Math.min(Math.max(available, 320), 900))
}

/**
 * Candles for one token with its own position history drawn on top.
 *
 * Positions are filtered client-side from the page's already-loaded rows rather than refetched:
 * the caller is the positions table, so it necessarily has them, and a second request would show
 * a different slice than the table the user just clicked.
 */
export default function TokenChartModal({
  instId: initialInstId,
  mode,
  positions,
  livePrices,
  onClose,
}: {
  /** Token the chart opens on; the strip can switch to another without closing the modal. */
  instId: string
  mode: PositionMode
  positions: Position[]
  /** instId -> last traded price, from the page's existing socket (CLAUDE.md §11.4). */
  livePrices: Record<string, string>
  onClose: () => void
}) {
  // Which token is charted. Seeded from the prop and then owned here, so clicking another position
  // in the strip re-points the SAME modal rather than closing and reopening it (2026-09-12
  // request) — reopening would lose the selected timeframe and the chart's pan/zoom.
  const [instId, setInstId] = useState(initialInstId)
  useEffect(() => setInstId(initialInstId), [initialInstId])
  const [bar, setBar] = useState<string>('5m')
  const [chartHeight, setChartHeight] = useState(() => availableChartHeight())

  // Candles come from a stale-while-revalidate cache keyed by (instId, bar), so switching between
  // open positions shows an already-loaded series in the SAME frame instead of blanking to
  // "Loading candles…" and re-fetching (2026-09-13 request).
  //
  // The previous version cleared the series to null on every identity change, which is what made
  // switching feel slow: the backend answers in ~10ms, but the round trip plus a full remount of
  // CandleChart discarded the rendered chart and the user's pan/zoom every time.
  //
  // refetchMs keeps the original behaviour of picking up a bar that closes while the modal is open.
  // maxAgeMs is deliberately shorter than that interval so a revisit after a while still
  // revalidates promptly, while a rapid back-and-forth between two positions serves from cache.
  const seriesKey = `candles:${instId}:${bar}`
  const {
    data: candles,
    error,
    loading: candlesLoading,
  } = useCachedResource(seriesKey, () => api.candles({ instId, bar, limit: 500 }), {
    maxAgeMs: 30_000,
    refetchMs: CANDLE_REFETCH_MS,
  })

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

  // Every open position across ALL tokens, one entry per token with its live PnL. Aggregated by
  // token rather than listed per order: the strip's job is "how is each token doing", and two
  // positions on one token would otherwise render as two chips claiming to be the same thing.
  const otherOpen = useMemo(() => {
    const byToken = new Map<string, { instId: string; pct: number | null }>()
    for (const p of positions) {
      if (p.ClosedAt) continue
      const live = unrealizedPnL(p, livePrices[p.InstID])
      const prev = byToken.get(p.InstID)
      const pct = live?.pct ?? null
      // Summed across a token's positions, so a token showing +3% means the token is up overall.
      byToken.set(p.InstID, {
        instId: p.InstID,
        pct: prev?.pct != null && pct != null ? prev.pct + pct : (prev?.pct ?? pct),
      })
    }
    return [...byToken.values()].sort((a, b) => a.instId.localeCompare(b.instId))
  }, [positions, livePrices])

  // The charted token always appears in the strip, even with no open position on it — the strip is
  // now the ONLY thing naming which token is displayed (the separate heading was removed), so a
  // token opened from a closed row would otherwise leave the chart unlabelled. It carries no PnL,
  // which is correct: there is no open position to have any.
  const chips = useMemo(
    () =>
      otherOpen.some((o) => o.instId === instId)
        ? otherOpen
        : [...otherOpen, { instId, pct: null }].sort((a, b) => a.instId.localeCompare(b.instId)),
    [otherOpen, instId],
  )

  // --- chart-side SL/TP editing (2026-09-12 request) -----------------------------------------
  // Only a BOT TRADER, still-open position can be edited: the endpoint is bot-only by construction
  // (paper ids route to a table with no manual-edit path) and a closed position has no resting
  // order on the exchange to amend.
  const editable = useMemo(
    () => shown.filter((p) => !p.ClosedAt && mode === 'bot'),
    [shown, mode],
  )
  // Which position the side panel edits. With the header's old dropdown gone, this is set by
  // clicking a chip; it falls back to the first editable position on the charted token.
  const [editId, setEditId] = useState<number | null>(null)
  useEffect(() => setEditId(null), [instId]) // re-resolve when the charted token changes
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

  // Closes the position at the live price. Deliberately the SAME call, confirm text and
  // close_reason as the positions table's own Close button (2026-09-12 request: "exactly like the
  // one in the positions panel") — two buttons that flatten real money must not behave differently
  // depending on where they were pressed.
  const [closing, setClosing] = useState(false)
  async function closeTargetPosition() {
    if (target === null) return
    if (
      !confirm(
        `Close ${tokenSymbol(target.InstID)} #${target.ID} now at the live price? Reason will be recorded as "manual".`,
      )
    ) {
      return
    }
    setClosing(true)
    try {
      await api.closePosition(target.ID, target.Mode)
      // Pending edits are moot once the position is gone; clearing them also stops the
      // unsaved-changes guard from challenging the operator on the way out.
      setPendingSl(null)
      setPendingTp(null)
      seededFor.current = null
      onClose()
    } catch (err) {
      alert(`Failed to request close: ${(err as Error).message}`)
    } finally {
      setClosing(false)
    }
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
          {/* Every open position, each clickable to re-point this same chart — one screen for "how
              is everything doing, and let me look at that one" without going back to the table.
              The charted token is NOT also shown separately on the left (2026-09-12 request): it is
              already in this strip, marked as selected, and repeating it said nothing new. */}
          {chips.length > 0 && (
            <div className="chart-pos-strip">
              {chips.map((o) => (
                <button
                  key={o.instId}
                  className={'chart-pos-chip' + (o.instId === instId ? ' active' : '')}
                  // Genuinely disabled, not just styled that way: clicking the chart you are
                  // already looking at does nothing, so it should not be focusable or clickable.
                  disabled={o.instId === instId}
                  onClick={() => setInstId(o.instId)}
                  title={o.instId === instId ? `${o.instId} (showing)` : `Show ${o.instId}`}
                >
                  <span className="chip-sym">{tokenSymbol(o.instId)}</span>
                  <span className={'chip-pnl ' + pnlClass(o.pct)}>
                    {o.pct === null ? '—' : `${o.pct > 0 ? '+' : ''}${o.pct.toFixed(1)}%`}
                  </span>
                </button>
              ))}
            </div>
          )}

          {/* Deep-links to the manual/discretionary trading page for the token currently showing
              (docs/MANUAL_TRADE_PLAN.md §5.3), not the strip's active one if they differ. */}
          <Link to={`/trade/${instId}`} className="btn-trade-link chart-trade-link" title={`Trade ${tokenSymbol(instId)}`}>
            Trade
          </Link>

          <button
            className="chart-close-x"
            onClick={requestClose}
            aria-label="Close chart"
            title="Close chart"
          >
            ✕
          </button>
        </div>

        {error && <p className="error">Could not load candles: {error}</p>}
        {!error && candlesLoading && <p style={{ color: '#858b96' }}>Loading candles…</p>}
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
                {/* Overlaid on the chart rather than sitting above it (2026-09-12 request): the
                    header is now about the position, and the timeframe belongs to the chart it
                    changes. Absolutely positioned so it costs the chart no vertical space. */}
                <div className="chart-bar-tabs chart-bar-tabs-overlay">
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
                  onClosePosition={closeTargetPosition}
                  closing={closing}
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
