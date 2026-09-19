import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { api } from '../api/client'
import type { ManualOrder, Position } from '../api/types'
import { useCachedResource } from '../hooks/useCachedResource'
import { useLiveCandles } from '../hooks/useLiveCandles'
import { usePriceStream } from '../hooks/usePriceStream'
import { useOrderbook } from '../hooks/useOrderbook'
import { usePolling } from '../hooks/usePolling'
import { CandleChart } from '../components/CandleChart'
import OrderbookLadder from '../components/OrderbookLadder'
import { fmtPctLabel } from '../components/ChartAdjustPanel'
import { pctOnMargin } from '../components/PositionZones'
import { tokenSymbol, trimPrice } from '../utils/format'

// The manual/discretionary trading page ("Trade" tab, docs/MANUAL_TRADE_PLAN.md). §9's suggested
// build order ships this BEFORE the orderbook WebSocket (§7) deliberately: a page with a chart and
// order ticket but no live orderbook ladder is still a functioning trading page, and the orderbook
// is the single largest/riskiest remaining piece. The middle column is a placeholder until it lands.

const BARS = ['5m', '15m', '1H'] as const
const DEFAULT_SYMBOL = 'BTC'

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

  const livePrices = usePriceStream(true)
  const lastPrice = livePrices[symbol]
  const book = useOrderbook(symbol)

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
      <div className="trade-header">
        <div className="trade-token-picker">
          <input
            className="trade-search"
            placeholder="Search token…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
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
        <span className="trade-symbol">{tokenSymbol(symbol)}</span>
        {lastPrice && <span className="trade-price mono">{trimPrice(lastPrice)}</span>}
        <div className="trade-bars">
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

      <div className="trade-columns">
        <div className="trade-col trade-col-chart">
          {candlesLoading && !candles ? (
            <p className="text-dim">Loading candles…</p>
          ) : (
            <CandleChart candles={candles ?? []} positions={chartPositions} frameKey={`${symbol}:${bar}`} height={520} />
          )}
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

      <div className="ot-type" role="group">
        {(['market', 'limit'] as const).map((t) => (
          <button
            key={t}
            type="button"
            className={'ot-type-btn' + (orderType === t ? ' active' : '')}
            onClick={() => setOrderType(t)}
          >
            {t === 'market' ? 'Market' : 'Limit'}
          </button>
        ))}
      </div>

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
