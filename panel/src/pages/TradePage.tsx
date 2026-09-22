import { useCallback, useEffect, useMemo, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { api } from '../api/client'
import type { ManualInstrument, ManualOrder, MarketToken, Position } from '../api/types'
import { useCachedResource } from '../hooks/useCachedResource'
import { useLiveCandles } from '../hooks/useLiveCandles'
import { usePriceStream } from '../hooks/usePriceStream'
import { useOrderbook } from '../hooks/useOrderbook'
import { usePolling } from '../hooks/usePolling'
import { useTradeDefaults, type TdMode } from '../hooks/useTradeDefaults'
import { useFavoriteTokens } from '../hooks/useFavoriteTokens'
import { CandleChart, ChartEngineToggle } from '../components/chart'
import OrderbookLadder from '../components/OrderbookLadder'
import PositionsTable from '../components/PositionsTable'
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
    // manual_orders has no exchange column (2026-09-22, multi-exchange paper trading only widened
    // paper_orders/bot's mode-scoped tables) — 'okx' matches every other non-paper table's fixed
    // value.
    Exchange: 'okx',
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

  // The instrument's real exchange tick size — the only correct anchor for the order book's
  // grouping steps (OrderbookLadder's own doc comment: a price-derived guess cannot match every
  // instrument's real tick, confirmed directly with the operator after two wrong attempts).
  const { data: instrument } = useCachedResource(
    `manual-instrument:${symbol}`,
    () => api.manualInstrument(symbol),
    { maxAgeMs: 60_000 },
  )

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

  // Manual trading's own trading cap/margin (2026-09-20 Account page) — the order ticket shows how
  // much of it is actually free before the operator sizes a new position, rather than only showing
  // the cap itself (which ignores what's already committed to other open manual positions).
  const { data: manualAccountStats, refresh: revalidateManualStats } = useCachedResource(
    'manual-account-stats',
    () => api.paperTradingStats('manual'),
    { maxAgeMs: 10_000, refetchMs: 15_000 },
  )

  const openOrders = openOrdersCached?.items ?? []
  const tokenOrder = useMemo(() => openOrders.find((o) => o.InstID === symbol), [openOrders, symbol])

  const chartPositions = useMemo(
    () => openOrders.filter((o) => o.InstID === symbol && o.EntryPx !== null).map(manualOrderToPosition),
    [openOrders, symbol],
  )

  function selectSymbol(sym: string) {
    setSearch('')
    setSearchFocused(false)
    navigate(`/trade/${sym.toUpperCase()}`)
  }

  return (
    <div className="trade-page">
      {/* Full-width symbol/stats header, above the 3-column workspace — the token picker, current
          price and 24h stats belong to the WHOLE page, not to any one column beneath it. */}
      <div className="trade-topbar">
        {/* onBlur on the CONTAINER, checking relatedTarget, rather than a fixed setTimeout on the
            search input — a setTimeout-based close raced real clicks inside the popover (tab
            buttons, exchange filters) and closed it before the click landed, which is exactly what
            made the token list look empty: the popover was gone before Playwright's (and a fast
            real click's) click event ever reached a row. relatedTarget tells us whether the NEW
            focus target is still inside this container; only close when it genuinely isn't. */}
        <div
          className="trade-token-picker"
          onBlur={(e) => {
            if (!e.currentTarget.contains(e.relatedTarget as Node | null)) {
              setSearchFocused(false)
            }
          }}
        >
          <button type="button" className="trade-symbol-btn" onClick={() => setSearchFocused(true)}>
            <TokenIcon symbol={symbol} size={22} />
            <span className="trade-symbol">{tokenSymbol(symbol)}</span>
            <span className="trade-symbol-suffix">Perp</span>
            <span className="trade-symbol-caret">▾</span>
          </button>
          {searchFocused && (
            <TokenPicker
              search={search}
              onSearchChange={setSearch}
              onSelect={selectSymbol}
              marketTokens={marketResp ?? []}
            />
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
            {/* 2026-09-21: compare the TradingView and OpenAlgo chart engines live — see
                components/chart/useChartEngine.ts. */}
            <ChartEngineToggle />
          </div>
          <div className="trade-chart-body">
            {/* Reduced from 560 — with the two side columns narrowed to 240px and the order book
                capped at TOTAL_ROWS=16 rows, a shorter chart height reads proportionate to them
                instead of towering over two narrow columns (explicit operator feedback). */}
            {candlesLoading && !candles ? (
              <p className="text-dim">Loading candles…</p>
            ) : (
              <CandleChart candles={candles ?? []} positions={chartPositions} frameKey={`${symbol}:${bar}`} height={460} />
            )}
          </div>
        </div>

        <div className="trade-col trade-col-book">
          <OrderbookLadder book={book} lastPrice={lastPrice} tickSz={instrument?.tickSz} />
        </div>

        <div className="trade-col trade-col-ticket">
          <OrderTicket
            symbol={symbol}
            lastPrice={lastPrice}
            openOrder={tokenOrder}
            hasStrategyPosition={false}
            instrument={instrument ?? undefined}
            availableMarginUsd={manualAccountStats?.availableMarginUsd}
            onOrderChanged={() => {
              revalidateOrders()
              revalidateManualStats()
            }}
          />
        </div>
      </div>

      {/* Manually-opened positions, at the bottom of the page they're opened from (2026-09-20
          request) — this is the exact same table Bot Trader/Paper use on the Positions tab
          (components/PositionsTable), filtered to mode="manual" server-side. It used to live as a
          third /positions/:mode tab, which was the wrong place: these orders are placed here, on
          Trade, so reviewing them here is where an operator actually looks for them. */}
      <div className="trade-manual-positions">
        <h2>Your manual positions</h2>
        <PositionsTable mode="manual" />
      </div>
    </div>
  )
}

type PickerTab = 'favorites' | 'top'

// dedupeBySymbol collapses a MarketToken[] (one row per (exchange,symbol), CLAUDE.md §53.6) down
// to one row per SYMBOL — the picker chooses a TOKEN to trade, not an exchange listing, and the
// same token can appear from several exchanges. Keeps the highest-score row per symbol (the same
// "best venue" reasoning the Home page's own market table already applies to price display).
function dedupeBySymbol(tokens: MarketToken[]): MarketToken[] {
  const bySymbol = new Map<string, MarketToken>()
  for (const t of tokens) {
    const existing = bySymbol.get(t.symbol)
    if (!existing || Number(t.score) > Number(existing.score)) bySymbol.set(t.symbol, t)
  }
  return [...bySymbol.values()]
}

/**
 * Token picker popover: a search box (always visible, filters whichever tab is active), a
 * Favorites/Top tab pair (Favorites first, per explicit request), and a dynamic per-exchange
 * filter derived from whatever exchanges are actually present in the scanned market data — never
 * a hardcoded list (explicit operator requirement), so a newly onboarded exchange appears here
 * with no panel code change.
 */
function TokenPicker({
  search,
  onSearchChange,
  onSelect,
  marketTokens,
}: {
  search: string
  onSearchChange: (v: string) => void
  onSelect: (symbol: string) => void
  marketTokens: MarketToken[]
}) {
  const [tab, setTab] = useState<PickerTab>('favorites')
  const [exchangeFilter, setExchangeFilter] = useState<string>('all')
  const { favorites, toggle, isFavorite } = useFavoriteTokens()

  const exchanges = useMemo(
    () => [...new Set(marketTokens.map((t) => t.exchange))].sort(),
    [marketTokens],
  )

  const byExchange = useMemo(
    () => (exchangeFilter === 'all' ? marketTokens : marketTokens.filter((t) => t.exchange === exchangeFilter)),
    [marketTokens, exchangeFilter],
  )

  const deduped = useMemo(() => dedupeBySymbol(byExchange), [byExchange])

  const query = search.trim().toUpperCase()

  const topRows = useMemo(() => {
    const sorted = [...deduped].sort((a, b) => Number(b.score) - Number(a.score))
    const filtered = query ? sorted.filter((t) => t.symbol.includes(query)) : sorted
    return filtered.slice(0, 20)
  }, [deduped, query])

  const favoriteRows = useMemo(() => {
    const rows = deduped.filter((t) => favorites.has(t.symbol))
    return query ? rows.filter((t) => t.symbol.includes(query)) : rows
  }, [deduped, favorites, query])

  const rows = tab === 'favorites' ? favoriteRows : topRows

  return (
    <div className="trade-search-popover">
      <input
        autoFocus
        className="trade-search"
        placeholder="Search token…"
        value={search}
        onChange={(e) => onSearchChange(e.target.value)}
      />

      <div className="picker-tabs" role="tablist">
        <button
          type="button"
          className={'picker-tab' + (tab === 'favorites' ? ' active' : '')}
          onClick={() => setTab('favorites')}
        >
          Favorites
        </button>
        <button type="button" className={'picker-tab' + (tab === 'top' ? ' active' : '')} onClick={() => setTab('top')}>
          Top
        </button>
      </div>

      {exchanges.length > 1 && (
        <div className="picker-exchanges" role="group">
          <button
            type="button"
            className={'picker-exchange-btn' + (exchangeFilter === 'all' ? ' active' : '')}
            onClick={() => setExchangeFilter('all')}
          >
            All
          </button>
          {exchanges.map((ex) => (
            <button
              key={ex}
              type="button"
              className={'picker-exchange-btn' + (exchangeFilter === ex ? ' active' : '')}
              onClick={() => setExchangeFilter(ex)}
            >
              {ex}
            </button>
          ))}
        </div>
      )}

      <ul className="trade-search-results picker-results">
        {rows.length === 0 && (
          <li className="picker-empty text-dim">
            {tab === 'favorites' ? 'No favorites yet — star a token to add one.' : 'No tokens found.'}
          </li>
        )}
        {rows.map((t) => (
          <li key={t.symbol} className="picker-row">
            <button type="button" className="picker-row-star" onClick={() => toggle(t.symbol)}>
              {isFavorite(t.symbol) ? '★' : '☆'}
            </button>
            <button type="button" className="picker-row-select" onClick={() => onSelect(t.symbol)}>
              <TokenIcon symbol={t.symbol} size={18} />
              <span className="picker-row-symbol">{tokenSymbol(t.symbol)}</span>
              <span className="picker-row-exchange">{t.exchange}</span>
              <span className={'mono picker-row-change ' + (Number(t.change24hPct) >= 0 ? 'text-green' : 'text-red')}>
                {Number.isFinite(Number(t.change24hPct)) ? `${Number(t.change24hPct) >= 0 ? '+' : ''}${Number(t.change24hPct).toFixed(2)}%` : '—'}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </div>
  )
}

type LevelMode = 'price' | 'pct'

function OrderTicket({
  symbol,
  lastPrice,
  openOrder,
  hasStrategyPosition,
  instrument,
  availableMarginUsd,
  onOrderChanged,
}: {
  symbol: string
  lastPrice?: string
  openOrder?: ManualOrder
  hasStrategyPosition: boolean
  instrument?: ManualInstrument
  availableMarginUsd?: string
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
      instrument={instrument}
      availableMarginUsd={availableMarginUsd}
      onOrderChanged={onOrderChanged}
    />
  )
}

function NewOrderTicket({
  symbol,
  lastPrice,
  hasStrategyPosition,
  instrument,
  availableMarginUsd,
  onOrderChanged,
}: {
  symbol: string
  lastPrice?: string
  hasStrategyPosition: boolean
  instrument?: ManualInstrument
  availableMarginUsd?: string
  onOrderChanged: () => void
}) {
  const [side, setSide] = useState<'buy' | 'sell'>('buy')
  const [orderType, setOrderType] = useState<'market' | 'limit'>('market')
  const [limitPx, setLimitPx] = useState('')
  const [sizeUsd, setSizeUsd] = useState('')
  const { tdMode, leverage, setTdMode, setLeverage } = useTradeDefaults()
  const [levelMode, setLevelMode] = useState<LevelMode>('pct')
  const [slText, setSlText] = useState('')
  const [tpText, setTpText] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [pendingIntentId, setPendingIntentId] = useState<number | null>(null)

  const refPx = orderType === 'limit' ? Number(limitPx) : Number(lastPrice ?? 0)

  async function handleSubmit() {
    // Caught here rather than only relying on the backend's own rejection (usecase.ManualTrader's
    // MinSz check) so the operator sees a clear reason immediately, without waiting on the
    // ~1-2s intent-processing round trip just to learn the order was always going to be refused.
    if (belowMinimum && instrument) {
      setError(`Order size is below ${symbol}'s minimum of ${instrument.minSz} contracts.`)
      return
    }
    setSubmitting(true)
    setError(null)
    try {
      const body: Parameters<typeof api.createManualOrder>[0] = {
        instId: symbol,
        side,
        orderType,
        sizeUsd,
        leverage,
        tdMode,
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
  // Raw contract count before OKX's own rounding rules — shown as "≈ N contracts" for a sense of
  // scale even before the instrument's own metadata has loaded (instrument is fetched separately
  // and can arrive after the operator has already started typing).
  const rawContracts = refPx > 0 ? (sizeNum * levNum) / refPx : null

  // OKX trades in whole CONTRACTS, not raw USD (2026-09-20 request): a contract's value (ctVal),
  // the minimum order size (minSz), and the required increment (lotSz) are all instrument-specific
  // and easy to miss — the backend already floors to the nearest lot (usecase.sizeToContracts) and
  // rejects a sized order below the minimum, but silently rounding what the operator asked for
  // without telling them is a bad experience even when it's technically safe. This mirrors that
  // same math client-side purely for the hint/warning below; the backend's own check is still what
  // actually protects the order — this can never be more permissive than that.
  let actualContracts: number | null = null
  let belowMinimum = false
  if (instrument && rawContracts !== null && rawContracts > 0) {
    const ctVal = Number(instrument.ctVal) || 1
    const lotSz = Number(instrument.lotSz) || 0
    const minSz = Number(instrument.minSz) || 0
    const baseUnits = rawContracts // sizeUsd*leverage/refPx already IS base-currency units here
    let contracts = baseUnits / ctVal
    if (lotSz > 0) contracts = Math.floor(contracts / lotSz) * lotSz
    actualContracts = contracts
    belowMinimum = minSz > 0 && contracts < minSz
  }

  const availableMarginNum = availableMarginUsd !== undefined ? Number(availableMarginUsd) : null
  const exceedsAvailableMargin = availableMarginNum !== null && sizeNum > availableMarginNum

  return (
    <div className="order-ticket">
      {/* Margin-mode / leverage / position-mode pills, matching an exchange's own ticket header
          (explicit reference) — all three are genuinely editable now (2026-09-19 fix): TdMode and
          leverage are per-request fields OKX already accepts on every order, so no exchange call is
          needed to change them here, just the panel's own persisted default (useTradeDefaults).
          Position mode (net/hedge) IS a real account-wide exchange setting, so it lives in its own
          component making its own live GetAccountConfig/SetPositionMode calls. */}
      <div className="ot-pills">
        <MarginModePill tdMode={tdMode} onChange={setTdMode} />
        <LeveragePill leverage={leverage} onChange={setLeverage} />
        <PositionModePill />
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
        <span
          title={
            instrument
              ? `Minimum order: ${instrument.minSz} contract${Number(instrument.minSz) === 1 ? '' : 's'} · ` +
                `orders must be a multiple of ${instrument.lotSz} contract${Number(instrument.lotSz) === 1 ? '' : 's'} · ` +
                `1 contract = ${instrument.ctVal} ${symbol}`
              : undefined
          }
        >
          Size (USD) {instrument && 'ⓘ'}
        </span>
        <input value={sizeUsd} onChange={(e) => setSizeUsd(e.target.value)} type="number" step="any" />
      </label>

      {availableMarginNum !== null && (
        <p className={'text-dim ot-contracts' + (exceedsAvailableMargin ? ' error' : '')}>
          Available to trade with: {availableMarginNum.toFixed(2)} USD
        </p>
      )}
      {exceedsAvailableMargin && (
        <p className="ot-warning">
          This is more than your available manual trading margin ({availableMarginNum!.toFixed(2)} USD).
          Set a larger trading cap on the Account page, or reduce this order's size.
        </p>
      )}

      {rawContracts !== null && (
        <p className="text-dim ot-contracts">
          {actualContracts !== null ? (
            <>≈ {actualContracts} contracts at {trimPrice(refPx)}</>
          ) : (
            <>≈ {rawContracts.toFixed(6)} contracts at {trimPrice(refPx)}</>
          )}
        </p>
      )}
      {belowMinimum && instrument && (
        <p className="ot-warning">
          This is below {symbol}'s minimum order size ({instrument.minSz} contracts) — the exchange
          will reject it. Increase the size or leverage.
        </p>
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

/** Editable margin-mode pill — click to open a Cross/Isolated toggle, closes on selection. */
function MarginModePill({ tdMode, onChange }: { tdMode: TdMode; onChange: (v: TdMode) => void }) {
  const [open, setOpen] = useState(false)
  return (
    <div className="ot-pill-editable">
      <button type="button" className="ot-pill ot-pill-btn" onClick={() => setOpen((v) => !v)}>
        {tdMode === 'isolated' ? 'Isolated' : 'Cross'}
      </button>
      {open && (
        <div className="ot-pill-menu">
          {(['cross', 'isolated'] as const).map((m) => (
            <button
              key={m}
              type="button"
              className={'ot-pill-menu-item' + (tdMode === m ? ' active' : '')}
              onClick={() => {
                onChange(m)
                setOpen(false)
              }}
            >
              {m === 'cross' ? 'Cross' : 'Isolated'}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

/** Editable leverage pill — click to reveal a number input, Enter/blur confirms. */
function LeveragePill({ leverage, onChange }: { leverage: string; onChange: (v: string) => void }) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(leverage)

  function commit() {
    const n = Number(draft)
    if (Number.isFinite(n) && n > 0) onChange(draft)
    else setDraft(leverage) // reject a bad value, revert to the last good one
    setEditing(false)
  }

  if (editing) {
    return (
      <input
        autoFocus
        className="ot-pill ot-pill-input"
        type="number"
        step="any"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === 'Enter') commit()
          if (e.key === 'Escape') {
            setDraft(leverage)
            setEditing(false)
          }
        }}
      />
    )
  }
  return (
    <button
      type="button"
      className="ot-pill ot-pill-btn"
      onClick={() => {
        setDraft(leverage)
        setEditing(true)
      }}
    >
      {leverage || '10'}x
    </button>
  )
}

/**
 * Position-mode pill (One-way / Hedge) — the one pill backed by a REAL account-wide exchange
 * setting (OKX's set-position-mode), unlike the other two which are per-request fields
 * (docs/MANUAL_TRADE_PLAN.md-adjacent, 2026-09-19 Trade page fixes). Reads the account's current
 * mode plus whether switching to hedge is currently possible (zero open positions) from
 * GET /api/manual/account-mode, disables the Hedge option with an explanatory tooltip when it
 * isn't, and calls POST /api/manual/account-mode on selection — the exchange's own rejection is
 * still authoritative if a position opened in the brief window since this last polled.
 */
function PositionModePill() {
  const [open, setOpen] = useState(false)
  const [switching, setSwitching] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const { data: mode, refresh } = useCachedResource(
    'manual-account-mode',
    () => api.manualAccountMode(),
    { maxAgeMs: 5_000, refetchMs: 15_000 },
  )

  async function selectMode(next: 'net' | 'hedge') {
    if (!mode || mode.posMode === next) {
      setOpen(false)
      return
    }
    setSwitching(true)
    setError(null)
    try {
      await api.setManualAccountMode(next)
      await refresh()
      setOpen(false)
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setSwitching(false)
    }
  }

  const label = mode?.posMode === 'hedge' ? 'Hedge' : 'One-way'

  return (
    <div className="ot-pill-editable">
      <button type="button" className="ot-pill ot-pill-btn" onClick={() => setOpen((v) => !v)}>
        {label}
      </button>
      {open && (
        <div className="ot-pill-menu ot-pill-menu-wide">
          <button
            type="button"
            className={'ot-pill-menu-item' + (mode?.posMode === 'net' ? ' active' : '')}
            disabled={switching}
            onClick={() => selectMode('net')}
          >
            One-way
          </button>
          <button
            type="button"
            className={'ot-pill-menu-item' + (mode?.posMode === 'hedge' ? ' active' : '')}
            disabled={switching || (mode ? !mode.canSwitchToHedge && mode.posMode !== 'hedge' : false)}
            title={
              mode && !mode.canSwitchToHedge && mode.posMode !== 'hedge'
                ? `Close every open position first (${mode.openPositionCount} open)`
                : undefined
            }
            onClick={() => selectMode('hedge')}
          >
            Hedge
          </button>
          {error && <p className="error ot-pill-menu-error">{error}</p>}
        </div>
      )}
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
