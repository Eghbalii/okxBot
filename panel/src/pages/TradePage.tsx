import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { api } from '../api/client'
import type { ManualOrder, MarketToken, Position } from '../api/types'
import { useCachedResource } from '../hooks/useCachedResource'
import { useLiveCandles } from '../hooks/useLiveCandles'
import { usePriceStream } from '../hooks/usePriceStream'
import { useOrderbook } from '../hooks/useOrderbook'
import { usePolling } from '../hooks/usePolling'
import { CandleChart } from '../components/CandleChart'
import OrderbookLadder from '../components/OrderbookLadder'
import TokenIcon from '../components/TokenIcon'
import { fmtPctLabel } from '../components/ChartAdjustPanel'
import { pctOnMargin } from '../components/PositionZones'
import { tokenSymbol, trimPrice } from '../utils/format'

// The manual/discretionary trading page ("Trade" tab, docs/MANUAL_TRADE_PLAN.md), laid out to
// match a real exchange's own trading screen (explicit operator reference, 2026-09-19 redesign):
// a full-width symbol/stats header, a chart panel that owns its own timeframe toolbar (not a
// page-level control sitting outside the chart), a single STACKED order book (asks above the
// spread, bids below — not the side-by-side split ladder an earlier pass tried), and an order
// ticket styled like an exchange's own leverage/margin-mode pills + order-type tabs.

const BARS = ['1m', '5m', '15m', '1H', '4H', '1D'] as const
const DEFAULT_SYMBOL = 'BTC'

function fmtUsdCompact(n: number): string {
  if (!Number.isFinite(n)) return '—'
  if (n >= 1e9) return `$${(n / 1e9).toFixed(2)}B`
  if (n >= 1e6) return `$${(n / 1e6).toFixed(1)}M`
  if (n >= 1e3) return `$${(n / 1e3).toFixed(0)}K`
  return `$${n.toFixed(0)}`
}

function fmtPctSigned(n: number): string {
  if (!Number.isFinite(n)) return '—'
  return `${n >= 0 ? '+' : ''}${n.toFixed(2)}%`
}

// manualOrderToPosition adapts a ManualOrder onto the minimal shape CandleChart/PositionZones
// actually read (ID/Side/EntryPx/SLPx/TPPx/OpenedAt/ClosedAt/Leverage) — safe because neither
// component mutates or round-trips this value, they only render it (verified against
// components/CandleChart.tsx's own positions.map(...) — CLAUDE.md's own "reuse without forcing a
// shared type" pattern). manual_orders is a fully independent table from paper_orders/bot_orders,
// so this conversion, not a shared Position type, is the right boundary.
function manualOrderToPosition(o: ManualOrder): Position {
  return {
    ID: o.ID,
    InstID: o.InstID,
    StrategyID: null,
    Side: o.Side,
    EntryPx: o.EntryPx ?? '0',
    SLPx: o.SLPx,
    TPPx: o.TPPx,
    Size: o.Size,
    Leverage: o.Leverage,
    OpenedAt: o.OpenedAt ?? o.CreatedAt,
    ClosedAt: o.ClosedAt,
    // ManualOrder's close-reason vocabulary ('canceled'/'liquidation') is wider than Position's
    // shared CloseReason type — safe to widen here since this conversion is display-only and
    // CandleChart/PositionZones never branch on the value, only pass it through.
    CloseReason: o.CloseReason as Position['CloseReason'],
    ClosePx: o.ClosePx,
    RealizedPnL: o.RealizedPnL,
    FeesUSD: null,
    FundingUSD: null,
    Mode: 'bot',
    ParentOrderID: null,
    AdjustmentCount: 0,
    Variant: 'baseline',
    Bar: '',
    StrategyName: '',
    PnLMaxPct: '0',
    PnLMinPct: '0',
    FeaturesJSON: null,
    Status: null,
    ExchangeOrderID: o.ExchangeOrderID,
    ExchangeCloseOrderID: o.ExchangeCloseOrderID,
    ExchangeRealizedPnL: null,
    ExchangeFee: o.ExchangeFee,
    ExchangeClosePx: null,
    LastError: o.LastError,
    LastErrorAt: o.LastErrorAt,
  }
}

function floatPtr(v: string): number | undefined {
  const n = Number(v)
  return Number.isFinite(n) && v.trim() !== '' ? n : undefined
}

export default function TradePage() {
  const { symbol: routeSymbol } = useParams<{ symbol?: string }>()
  const navigate = useNavigate()
  // The URL is the source of truth for which token is showing (matches PositionsPage's own
  // mode-from-URL principle) — defaulting here, in the page, rather than assuming a component
  // always receives one (docs/MANUAL_TRADE_PLAN.md §5.4).
  const symbol = (routeSymbol ?? DEFAULT_SYMBOL).toUpperCase()
  const [bar, setBar] = useState<string>('5m')
  const [search, setSearch] = useState('')
  const [searchFocused, setSearchFocused] = useState(false)

  const livePrices = usePriceStream(true)
  const lastPrice = livePrices[symbol]
  const book = useOrderbook(symbol)

  // 24h stats for the header row (high/low/volume/change) — the same market-scan data the Home
  // page's table already reads, just filtered to this one symbol.
  const { data: marketResp } = useCachedResource(
    'market-tokens:all',
    () => api.marketTokens({}),
    { maxAgeMs: 30_000, refetchMs: 30_000 },
  )
  const marketToken = useMemo<MarketToken | undefined>(
    () => marketResp?.find((t) => t.symbol === symbol),
    [marketResp, symbol],
  )
  const changePct = marketToken ? Number(marketToken.change24hPct) : NaN
  const high24h = marketToken ? Number(marketToken.high24h) : NaN
  const low24h = marketToken ? Number(marketToken.low24h) : NaN
  const vol24hUsd = marketToken ? Number(marketToken.vol24hUsd) : NaN

  // Candles cached the same way TokenChartModal does (CLAUDE.md §50), so switching here from a
  // Home-page chart modal for the same token shares one cache entry.
  const seriesKey = `candles:${symbol}:${bar}`
  const { data: fetchedCandles, loading: candlesLoading } = useCachedResource(
    seriesKey,
    () => api.candles({ instId: symbol, bar, limit: 500 }),
    { maxAgeMs: 15_000, refetchMs: 60_000 },
  )
  const candles = useLiveCandles(fetchedCandles, symbol, bar)

  // Open manual orders, cached + revalidated on an interval (CLAUDE.md §50.2b's fix for exactly
  // this pattern — a page switch should not blank an already-loaded table).
  const { data: openOrdersCached, refresh: revalidateOrders } = useCachedResource(
    'manual-orders:open',
    () => api.listManualOrders({ open: true }),
    { maxAgeMs: 4_000, refetchMs: 5_000 },
  )

  const openOrders = openOrdersCached?.items ?? []
  const tokenOrder = useMemo(() => openOrders.find((o) => o.InstID === symbol), [openOrders, symbol])

  const chartPositions = useMemo(
    () => openOrders.filter((o) => o.InstID === symbol && o.EntryPx !== null).map(manualOrderToPosition),
    [openOrders, symbol],
  )

  // Token search — resolves against the existing roster first (fast, matches most cases), falling
  // through to a live GetInstrument lookup for anything not yet in it (§8.3: manual trading is not
  // limited to the pre-scanned roster).
  const { data: rosterResp } = useCachedResource(
    'instruments:all',
    () => api.instruments({}),
    { maxAgeMs: 60_000 },
  )
  const roster = rosterResp?.items ?? []
  const searchResults = useMemo(() => {
    const q = search.trim().toUpperCase()
    if (!q) return []
    // One row per SYMBOL, not per (symbol, exchange) roster row — the same token can be listed by
    // several exchanges (CLAUDE.md §53.6's "one row per token" precedent), and the picker is
    // choosing a token to trade, not an exchange listing.
    const seen = new Set<string>()
    const out: string[] = []
    for (const i of roster) {
      if (!i.symbol.includes(q) || seen.has(i.symbol)) continue
      seen.add(i.symbol)
      out.push(i.symbol)
      if (out.length >= 8) break
    }
    return out
  }, [roster, search])

  function selectSymbol(sym: string) {
    setSearch('')
    navigate(`/trade/${sym.toUpperCase()}`)
  }

  return (
    <div className="trade-page">
      {/* Full-width symbol/stats header, above the 3-column workspace — the token picker, current
          price and 24h stats belong to the WHOLE page, not to any one column beneath it. */}
      <div className="trade-topbar">
        <div className="trade-token-picker">
          <button type="button" className="trade-symbol-btn" onClick={() => setSearchFocused(true)}>
            <TokenIcon symbol={symbol} size={22} />
            <span className="trade-symbol">{tokenSymbol(symbol)}</span>
            <span className="trade-symbol-suffix">Perp</span>
            <span className="trade-symbol-caret">▾</span>
          </button>
          {searchFocused && (
            <div className="trade-search-popover">
              <input
                autoFocus
                className="trade-search"
                placeholder="Search token…"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                onBlur={() => window.setTimeout(() => setSearchFocused(false), 150)}
              />
              {searchResults.length > 0 && (
                <ul className="trade-search-results">
                  {searchResults.map((sym) => (
                    <li key={sym}>
                      <button type="button" onClick={() => selectSymbol(sym)}>
                        {sym}
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}
        </div>

        <div className="trade-topbar-stats">
          <div className="trade-stat trade-stat-price">
            <span className={'mono trade-price ' + (changePct >= 0 ? 'text-green' : 'text-red')}>
              {trimPrice(lastPrice)}
            </span>
            {Number.isFinite(changePct) && (
              <span className={'trade-stat-sub ' + (changePct >= 0 ? 'text-green' : 'text-red')}>
                {fmtPctSigned(changePct)}
              </span>
            )}
          </div>
          <div className="trade-stat">
            <span className="trade-stat-label">24h High</span>
            <span className="mono">{Number.isFinite(high24h) ? trimPrice(high24h) : '—'}</span>
          </div>
          <div className="trade-stat">
            <span className="trade-stat-label">24h Low</span>
            <span className="mono">{Number.isFinite(low24h) ? trimPrice(low24h) : '—'}</span>
          </div>
          <div className="trade-stat">
            <span className="trade-stat-label">24h Volume</span>
            <span className="mono">{Number.isFinite(vol24hUsd) ? fmtUsdCompact(vol24hUsd) : '—'}</span>
          </div>
        </div>
      </div>

      <div className="trade-columns">
        <div className="trade-col trade-col-chart">
          {/* The chart panel's OWN toolbar — timeframe buttons live here, inside the chart card,
              never as a page-level control floating above it (explicit operator correction). */}
          <div className="trade-chart-toolbar">
            <div className="trade-bars" role="group">
              {BARS.map((b) => (
                <button
                  key={b}
                  className={'trade-bar-btn' + (bar === b ? ' active' : '')}
                  onClick={() => setBar(b)}
                  type="button"
                >
                  {b}
                </button>
              ))}
            </div>
          </div>
          <div className="trade-chart-body">
            {candlesLoading && !candles ? (
              <p className="text-dim">Loading candles…</p>
            ) : (
              <CandleChart candles={candles ?? []} positions={chartPositions} frameKey={`${symbol}:${bar}`} height={560} />
            )}
          </div>
        </div>

        <div className="trade-col trade-col-book">
          <OrderbookLadder book={book} lastPrice={lastPrice} />
        </div>

        <div className="trade-col trade-col-ticket">
          <OrderTicket
            symbol={symbol}
            lastPrice={lastPrice}
            openOrder={tokenOrder}
            hasStrategyPosition={false}
            onOrderChanged={() => revalidateOrders()}
          />
        </div>
      </div>
    </div>
  )
}


type LevelMode = 'price' | 'pct'

function OrderTicket({
  symbol,
  lastPrice,
  openOrder,
  hasStrategyPosition,
  onOrderChanged,
}: {
  symbol: string
  lastPrice?: string
  openOrder?: ManualOrder
  hasStrategyPosition: boolean
  onOrderChanged: () => void
}) {
  if (openOrder && (openOrder.Status === 'filled' || openOrder.Status === 'partial')) {
    return (
      <OpenPositionTicket order={openOrder} onOrderChanged={onOrderChanged} />
    )
  }
  if (openOrder && openOrder.Status === 'resting') {
    return <RestingOrderTicket order={openOrder} onOrderChanged={onOrderChanged} />
  }
  return (
    <NewOrderTicket
      symbol={symbol}
      lastPrice={lastPrice}
      hasStrategyPosition={hasStrategyPosition}
      onOrderChanged={onOrderChanged}
    />
  )
}

function NewOrderTicket({
  symbol,
  lastPrice,
  hasStrategyPosition,
  onOrderChanged,
}: {
  symbol: string
  lastPrice?: string
  hasStrategyPosition: boolean
  onOrderChanged: () => void
}) {
  const [side, setSide] = useState<'buy' | 'sell'>('buy')
  const [orderType, setOrderType] = useState<'market' | 'limit'>('market')
  const [limitPx, setLimitPx] = useState('')
  const [sizeUsd, setSizeUsd] = useState('')
  const [leverage, setLeverage] = useState('10')
  const [levelMode, setLevelMode] = useState<LevelMode>('pct')
  const [slText, setSlText] = useState('')
  const [tpText, setTpText] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [pendingIntentId, setPendingIntentId] = useState<number | null>(null)

  const refPx = orderType === 'limit' ? Number(limitPx) : Number(lastPrice ?? 0)

  async function handleSubmit() {
    setSubmitting(true)
    setError(null)
    try {
      const body: Parameters<typeof api.createManualOrder>[0] = {
        instId: symbol,
        side,
        orderType,
        sizeUsd,
        leverage,
      }
      if (orderType === 'limit') body.limitPx = limitPx
      if (slText.trim() !== '') {
        if (levelMode === 'pct') body.slPct = -Math.abs(floatPtr(slText) ?? 0)
        else body.slPx = slText
      }
      if (tpText.trim() !== '') {
        if (levelMode === 'pct') body.tpPct = Math.abs(floatPtr(tpText) ?? 0)
        else body.tpPx = tpText
      }
      const res = await api.createManualOrder(body)
      setPendingIntentId(res.intentId)
      onOrderChanged()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  // Poll the intent until it resolves, so the ticket can tell the operator whether the order
  // actually opened rather than leaving them guessing during the ~1-2s ManualTrader poll window.
  const { data: intent } = usePolling(
    () => (pendingIntentId ? api.manualOrderIntent(pendingIntentId) : Promise.resolve(null)),
    1000,
    [pendingIntentId],
  )
  useEffect(() => {
    if (intent && (intent.Status === 'done' || intent.Status === 'failed')) {
      onOrderChanged()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [intent?.Status])

  const sizeNum = Number(sizeUsd) || 0
  const levNum = Number(leverage) || 1
  const ctVal = refPx > 0 ? ((sizeNum * levNum) / refPx).toFixed(6) : null

  return (
    <div className="order-ticket">
      {/* Margin-mode / leverage pills, matching an exchange's own ticket header (explicit
          reference) — this account is always cross-margin/manual leverage, so these are read-only
          badges rather than switches; leverage itself is the actual input further down. */}
      <div className="ot-pills">
        <span className="ot-pill">Cross</span>
        <span className="ot-pill">{leverage || '10'}x</span>
      </div>

      <div className="ot-type" role="group">
        {(['limit', 'market'] as const).map((t) => (
          <button
            key={t}
            type="button"
            className={'ot-type-tab' + (orderType === t ? ' active' : '')}
            onClick={() => setOrderType(t)}
          >
            {t === 'market' ? 'Market' : 'Limit'}
          </button>
        ))}
      </div>

      <div className="order-ticket-side" role="group">
        <button
          type="button"
          className={'ot-side-btn ot-long' + (side === 'buy' ? ' active' : '')}
          onClick={() => setSide('buy')}
        >
          Long
        </button>
        <button
          type="button"
          className={'ot-side-btn ot-short' + (side === 'sell' ? ' active' : '')}
          onClick={() => setSide('sell')}
        >
          Short
        </button>
      </div>

      {hasStrategyPosition && (
        <p className="ot-warning">This token has an open position from a strategy.</p>
      )}

      {orderType === 'limit' && (
        <label className="ot-field">
          <span>Limit price</span>
          <input value={limitPx} onChange={(e) => setLimitPx(e.target.value)} type="number" step="any" />
        </label>
      )}

      <label className="ot-field">
        <span>Size (USD)</span>
        <input value={sizeUsd} onChange={(e) => setSizeUsd(e.target.value)} type="number" step="any" />
      </label>

      <label className="ot-field">
        <span>Leverage</span>
        <input value={leverage} onChange={(e) => setLeverage(e.target.value)} type="number" step="any" />
      </label>

      {ctVal && (
        <p className="text-dim ot-contracts">≈ {ctVal} contracts at {trimPrice(refPx)}</p>
      )}

      <div className="ot-mode" role="group">
        {(['pct', 'price'] as const).map((m) => (
          <button
            key={m}
            type="button"
            className={'ot-mode-btn' + (levelMode === m ? ' active' : '')}
            onClick={() => setLevelMode(m)}
          >
            {m === 'price' ? 'Price' : '% of margin'}
          </button>
        ))}
      </div>

      <label className="ot-field">
        <span>Stop loss{levelMode === 'pct' ? ' (%)' : ''}</span>
        <input value={slText} onChange={(e) => setSlText(e.target.value)} type="number" step="any" />
      </label>
      <label className="ot-field">
        <span>Take profit{levelMode === 'pct' ? ' (%)' : ''}</span>
        <input value={tpText} onChange={(e) => setTpText(e.target.value)} type="number" step="any" />
      </label>

      {error && <p className="error">{error}</p>}
      {intent?.Status === 'pending' || intent?.Status === 'claimed' ? (
        <p className="text-dim">Placing order…</p>
      ) : intent?.Status === 'failed' ? (
        <p className="error">{intent.Error ?? 'Order failed.'}</p>
      ) : null}

      <button
        type="button"
        className={side === 'buy' ? 'btn-primary ot-submit-long' : 'btn-danger ot-submit-short'}
        disabled={submitting || !sizeUsd || (orderType === 'limit' && !limitPx)}
        onClick={handleSubmit}
      >
        {submitting ? 'Submitting…' : side === 'buy' ? 'Buy / Long' : 'Sell / Short'}
      </button>
    </div>
  )
}

function RestingOrderTicket({ order, onOrderChanged }: { order: ManualOrder; onOrderChanged: () => void }) {
  const [canceling, setCanceling] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function handleCancel() {
    setCanceling(true)
    setError(null)
    try {
      await api.cancelManualOrder(order.ID)
      onOrderChanged()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setCanceling(false)
    }
  }

  return (
    <div className="order-ticket">
      <p>
        Resting {order.Side} limit order at <span className="mono">{trimPrice(order.LimitPx)}</span>, waiting to
        fill.
      </p>
      {error && <p className="error">{error}</p>}
      <button type="button" className="btn-danger" onClick={handleCancel} disabled={canceling}>
        {canceling ? 'Canceling…' : 'Cancel order'}
      </button>
    </div>
  )
}

function OpenPositionTicket({ order, onOrderChanged }: { order: ManualOrder; onOrderChanged: () => void }) {
  const entry = Number(order.EntryPx)
  const leverage = Number(order.Leverage) || 1
  const side = order.Side
  const [levelMode, setLevelMode] = useState<LevelMode>('pct')
  const [slText, setSlText] = useState('')
  const [tpText, setTpText] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [closing, setClosing] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const display = useCallback(
    (price: string | null) => {
      if (price === null) return ''
      const p = Number(price)
      return levelMode === 'price' ? trimPrice(p) : fmtPctLabel(pctOnMargin(entry, p, side, leverage))
    },
    [levelMode, entry, side, leverage],
  )

  useEffect(() => setSlText(display(order.SLPx)), [order.SLPx, display])
  useEffect(() => setTpText(display(order.TPPx)), [order.TPPx, display])

  async function handleUpdate() {
    setSubmitting(true)
    setError(null)
    try {
      const body: { slPct?: number; tpPct?: number } = {}
      if (levelMode === 'pct') {
        const slPct = floatPtr(slText)
        const tpPct = floatPtr(tpText)
        if (slPct !== undefined) body.slPct = slPct
        if (tpPct !== undefined) body.tpPct = tpPct
      } else {
        const slPx = floatPtr(slText)
        const tpPx = floatPtr(tpText)
        if (slPx !== undefined) body.slPct = pctOnMargin(entry, slPx, side, leverage)
        if (tpPx !== undefined) body.tpPct = pctOnMargin(entry, tpPx, side, leverage)
      }
      await api.adjustManualOrder(order.ID, body)
      onOrderChanged()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  async function handleClose() {
    setClosing(true)
    setError(null)
    try {
      await api.closeManualOrder(order.ID)
      onOrderChanged()
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setClosing(false)
    }
  }

  return (
    <div className="order-ticket">
      <div className="ot-position-head">
        <span className={side === 'buy' ? 'cae-side cae-side-long' : 'cae-side cae-side-short'}>
          {side === 'buy' ? 'LONG' : 'SHORT'}
        </span>
        <span className="mono">{leverage.toFixed(1)}x</span>
      </div>
      <p className="text-dim">
        Entry <span className="mono">{trimPrice(order.EntryPx)}</span>
      </p>

      {order.ProtectedByStrategy && (
        <p className="ot-warning">
          SL/TP is protected by an open strategy position on this token; this manual position has no
          independent stop/target.
        </p>
      )}

      {!order.ProtectedByStrategy && (
        <>
          <div className="ot-mode" role="group">
            {(['pct', 'price'] as const).map((m) => (
              <button
                key={m}
                type="button"
                className={'ot-mode-btn' + (levelMode === m ? ' active' : '')}
                onClick={() => setLevelMode(m)}
              >
                {m === 'price' ? 'Price' : '% of margin'}
              </button>
            ))}
          </div>
          <label className="ot-field">
            <span>Stop loss</span>
            <input value={slText} onChange={(e) => setSlText(e.target.value)} type="text" />
          </label>
          <label className="ot-field">
            <span>Take profit</span>
            <input value={tpText} onChange={(e) => setTpText(e.target.value)} type="text" />
          </label>
          {error && <p className="error">{error}</p>}
          <button type="button" className="btn-primary" onClick={handleUpdate} disabled={submitting}>
            {submitting ? 'Updating…' : 'Update'}
          </button>
        </>
      )}

      <button type="button" className="btn-danger" onClick={handleClose} disabled={closing}>
        {closing ? 'Closing…' : 'Close position'}
      </button>
    </div>
  )
}
