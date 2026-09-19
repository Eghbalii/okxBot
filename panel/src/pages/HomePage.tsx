import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import type {
  ExchangeBalance,
  MarketToken,
  PaperTradingStats,
  Position,
  ScanResult,
} from '../api/types'
import { useCachedResource } from '../hooks/useCachedResource'
import { usePriceStream } from '../hooks/usePriceStream'
import SortableTh from '../components/SortableTh'
import Pagination from '../components/Pagination'
import TokenIcon, { ExchangeBadges } from '../components/TokenIcon'
import TokenChartModal from '../components/TokenChartModal'

/**
 * The Home page (2026-09-13 request): balances across exchanges, then paper and real service status,
 * then the token market — sortable, with icons, exchange marks, and click-through to a chart.
 *
 * Every section reads through useCachedResource, so returning to this page from another tab paints
 * from cache in the same frame and revalidates behind it. That matters more here than elsewhere: this
 * is the landing route, so it is the page most often arrived at with nothing on screen yet.
 */

type SortField = 'symbol' | 'lastPx' | 'change24hPct' | 'vol24hUsd' | 'range24hPct' | 'score'

// A symbol may trade on several exchanges, each with its own instrument, price and volume. The table
// shows ONE row per token with those venues rolled up, because "which tokens look interesting" is a
// question about tokens; the per-venue numbers stay reachable through the row's own exchange marks.
//
// Volume is SUMMED across venues and price is taken from the deepest one. Summing volume is the point
// — total tradeable liquidity for a token is what decides whether it is worth trading at all — while
// averaging price across a thin venue and a deep one would produce a number that exists nowhere.
interface TokenRow {
  symbol: string
  exchanges: string[]
  lastPx: number
  change24hPct: number
  vol24hUsd: number
  range24hPct: number
  score: number
  inRoster: boolean
  enabledPaper: boolean
  enabledReal: boolean
  /** The venue the displayed price and change came from, so a reader can tell where it is from. */
  priceFrom: string
  /** priceFrom's own 24h volume, kept so a later venue can be compared against it directly. */
  priceFromVol: number
}

function rollUp(tokens: MarketToken[]): TokenRow[] {
  const by = new Map<string, TokenRow>()
  for (const t of tokens) {
    const vol = Number(t.vol24hUsd) || 0
    const existing = by.get(t.symbol)
    if (!existing) {
      by.set(t.symbol, {
        symbol: t.symbol,
        exchanges: [t.exchange],
        lastPx: Number(t.lastPx) || 0,
        change24hPct: Number(t.change24hPct) || 0,
        vol24hUsd: vol,
        range24hPct: Number(t.range24hPct) || 0,
        score: Number(t.score) || 0,
        inRoster: t.inRoster,
        enabledPaper: t.enabledPaper,
        enabledReal: t.enabledReal,
        priceFrom: t.exchange,
        priceFromVol: vol,
      })
      continue
    }
    existing.exchanges.push(t.exchange)
    // The deepest venue's price/change/range wins — it is the one a real order would actually be
    // filled near. Compared against that venue's OWN volume (priceFromVol), not against the running
    // cross-venue total, which already includes this row and would make the comparison meaningless.
    if (vol > existing.priceFromVol) {
      existing.lastPx = Number(t.lastPx) || existing.lastPx
      existing.change24hPct = Number(t.change24hPct) || existing.change24hPct
      existing.range24hPct = Number(t.range24hPct) || existing.range24hPct
      existing.priceFrom = t.exchange
      existing.priceFromVol = vol
    }
    existing.vol24hUsd += vol
    // Roster membership is per (exchange, symbol); a token counts as in the roster if ANY venue is,
    // since that is what decides whether the bot is already working with it.
    existing.inRoster = existing.inRoster || t.inRoster
    existing.enabledPaper = existing.enabledPaper || t.enabledPaper
    existing.enabledReal = existing.enabledReal || t.enabledReal
    existing.score = Math.max(existing.score, Number(t.score) || 0)
  }
  return [...by.values()]
}

function fmtUSD(n: number): string {
  if (n >= 1e9) return `$${(n / 1e9).toFixed(2)}B`
  if (n >= 1e6) return `$${(n / 1e6).toFixed(1)}M`
  if (n >= 1e3) return `$${(n / 1e3).toFixed(0)}K`
  return `$${n.toFixed(0)}`
}

// Prices here span nine orders of magnitude (BTC at ~77,000, PEPE at ~0.000008), so a fixed number of
// decimals is wrong for almost every row. Significant digits keep both readable.
function fmtPrice(n: number): string {
  if (n === 0) return '—'
  if (n >= 1000) return n.toLocaleString(undefined, { maximumFractionDigits: 1 })
  if (n >= 1) return n.toFixed(3)
  if (n >= 0.001) return n.toFixed(5)
  return n.toPrecision(3)
}

function fmtPct(n: number): string {
  return `${n >= 0 ? '+' : ''}${n.toFixed(2)}%`
}

function BalanceRow() {
  const { data, loading, error } = useCachedResource<ExchangeBalance[]>(
    'exchange-balances',
    () => api.exchangeBalances(),
    { maxAgeMs: 30_000 },
  )

  if (loading) return <div className="home-balances loading">Loading balances…</div>
  if (error) return <div className="home-balances error">Balances unavailable: {error}</div>

  return (
    <div className="home-balances">
      {(data ?? []).map((b) => (
        <div key={b.exchange} className={'balance-card' + (b.configured ? '' : ' unconfigured')}>
          <div className="balance-exchange">
            <ExchangeBadges exchanges={[b.exchange]} />
            <span>{b.exchange.toUpperCase()}</span>
          </div>
          {/* Three distinct states, never collapsed into one: a missing key, a reachable exchange
              that failed, and a real balance. Showing "$0.00" for the first two would be a
              plausible-looking lie about how much capital exists. */}
          {!b.configured ? (
            <div className="balance-amount muted">not configured</div>
          ) : b.err ? (
            <div className="balance-amount error" title={b.err}>
              unavailable
            </div>
          ) : (
            <>
              <div className="balance-amount">
                ${Number(b.equityUsd).toFixed(2)} <span className="balance-ccy">{b.ccy}</span>
              </div>
              <div className="balance-avail">avail ${Number(b.availUsd).toFixed(2)}</div>
            </>
          )}
        </div>
      ))}
    </div>
  )
}

function ServiceCard({ mode }: { mode: 'paper' | 'bot' }) {
  const { data, loading, error } = useCachedResource<PaperTradingStats>(
    `pt-stats:${mode}`,
    () => api.paperTradingStats(mode),
    { maxAgeMs: 15_000 },
  )

  const pnl = data ? Number(data.pnl24hUsd) : 0
  return (
    <div className={`service-card service-card-${mode}`}>
      <div className="service-card-head">
        <span className="service-card-title">{mode === 'paper' ? 'Paper Trading' : 'Bot Trader'}</span>
        {/* Link, not a bare href: this app uses BrowserRouter, so a hash URL would not route at
            all and a plain href would full-page-reload the SPA. */}
        <Link className="service-card-link" to={`/positions/${mode}`}>
          positions →
        </Link>
      </div>
      {loading && !data ? (
        <div className="muted">Loading…</div>
      ) : error ? (
        <div className="error">{error}</div>
      ) : (
        <div className="service-card-grid">
          <div>
            <span className="stat-label">Equity</span>
            <span className="stat-value">${Number(data!.totalEquityUsd).toFixed(2)}</span>
          </div>
          <div>
            <span className="stat-label">Balance</span>
            <span className="stat-value">${Number(data!.accountBalanceUsd).toFixed(2)}</span>
          </div>
          <div>
            <span className="stat-label">Open orders</span>
            <span className="stat-value">{data!.openCount}</span>
          </div>
          <div>
            <span className="stat-label">PnL 24h</span>
            <span className={'stat-value ' + (pnl >= 0 ? 'pos' : 'neg')}>
              {pnl >= 0 ? '+' : ''}${pnl.toFixed(2)}
            </span>
          </div>
          <div>
            <span className="stat-label">PnL 7d</span>
            <span className={'stat-value ' + (Number(data!.pnl7dUsd) >= 0 ? 'pos' : 'neg')}>
              {Number(data!.pnl7dUsd) >= 0 ? '+' : ''}${Number(data!.pnl7dUsd).toFixed(2)}
            </span>
          </div>
          <div>
            <span className="stat-label">PnL 30d</span>
            <span className={'stat-value ' + (Number(data!.pnl30dUsd) >= 0 ? 'pos' : 'neg')}>
              {Number(data!.pnl30dUsd) >= 0 ? '+' : ''}${Number(data!.pnl30dUsd).toFixed(2)}
            </span>
          </div>
        </div>
      )}
    </div>
  )
}

export default function HomePage() {
  const [sortBy, setSortBy] = useState<SortField>('score')
  const [sortDesc, setSortDesc] = useState(true)
  const [search, setSearch] = useState('')
  const [rosterOnly, setRosterOnly] = useState(false)
  const [chartToken, setChartToken] = useState<string | null>(null)
  const [page, setPage] = useState(0)
  // 10 per page by explicit request. Client-side: the whole snapshot is ~142 rows and already
  // arrives in one request, so paging in the browser is instant where a request per page would add a
  // VPN round trip to every click for no benefit at this size.
  const [pageSize, setPageSize] = useState(10)
  const [scanning, setScanning] = useState(false)
  const [scanResults, setScanResults] = useState<ScanResult[] | null>(null)

  const {
    data: tokens,
    loading,
    error,
    refresh,
  } = useCachedResource<MarketToken[]>('market-tokens', () => api.marketTokens({ limit: 400 }), {
    maxAgeMs: 60_000,
  })

  // Live prices for the tokens the bot actually tracks, from the same socket the positions page uses.
  // Scanned-but-untracked tokens have no stream — the ingestor only subscribes to the roster — so the
  // table falls back to the scan's own last price for those, which is the honest value to show.
  const livePrices = usePriceStream(true)

  const rows = useMemo(() => {
    let out = rollUp(tokens ?? [])
    if (rosterOnly) out = out.filter((r) => r.inRoster)
    const q = search.trim().toUpperCase()
    if (q) out = out.filter((r) => r.symbol.includes(q))
    const dir = sortDesc ? -1 : 1
    return out.sort((a, b) => {
      if (sortBy === 'symbol') return a.symbol.localeCompare(b.symbol) * dir
      return (a[sortBy] - b[sortBy]) * dir
    })
  }, [tokens, rosterOnly, search, sortBy, sortDesc])

  // Clamp rather than reset to 0: re-sorting should keep you where you were reading, but filtering
  // down to fewer rows than the current offset would otherwise show a blank table that looks broken.
  const pageCount = Math.max(1, Math.ceil(rows.length / pageSize))
  const safePage = Math.min(page, pageCount - 1)
  const pageRows = useMemo(
    () => rows.slice(safePage * pageSize, safePage * pageSize + pageSize),
    [rows, safePage, pageSize],
  )

  function onSort(field: SortField) {
    if (field === sortBy) {
      setSortDesc((d) => !d)
      return
    }
    setSortBy(field)
    // A new numeric column starts descending (biggest movers and deepest markets first, which is
    // what the column was clicked for); the symbol column starts ascending, since A-Z is what
    // alphabetical means.
    setSortDesc(field !== 'symbol')
  }

  async function runScan() {
    setScanning(true)
    setScanResults(null)
    try {
      const res = await api.runScan()
      setScanResults(res)
      await refresh()
    } catch (e) {
      setScanResults([{ exchange: 'scan', scanned: 0, candidates: 0, admitted: 0, err: String(e) }])
    } finally {
      setScanning(false)
    }
  }

  return (
    <div className="home-page">
      <BalanceRow />

      <div className="home-services">
        <ServiceCard mode="paper" />
        <ServiceCard mode="bot" />
      </div>

      <div className="home-market">
        <div className="home-market-head">
          <h2>Market</h2>
          <input
            className="home-search"
            placeholder="Filter symbol…"
            value={search}
            onChange={(e) => {
              setSearch(e.target.value)
              setPage(0)
            }}
          />
          <label className="home-toggle">
            <input
              type="checkbox"
              checked={rosterOnly}
              onChange={(e) => {
                setRosterOnly(e.target.checked)
                setPage(0)
              }}
            />
            Traded only
          </label>
          <button className="btn-primary" onClick={runScan} disabled={scanning}>
            {scanning ? 'Scanning…' : 'Scan now'}
          </button>
          <span className="home-market-count">{rows.length} tokens</span>
        </div>

        {scanResults && (
          <div className="scan-results">
            {scanResults.map((r) => (
              <span key={r.exchange} className={'scan-result' + (r.err ? ' error' : '')}>
                {r.exchange}: {r.err ? r.err : `${r.candidates} candidates, ${r.admitted} admitted`}
              </span>
            ))}
          </div>
        )}

        {error && <div className="error">Market data unavailable: {error}</div>}
        {loading && !tokens ? (
          <div className="muted">Loading market…</div>
        ) : rows.length === 0 ? (
          <div className="muted">
            No tokens yet — the discovery scan runs a few times a day, or press “Scan now”.
          </div>
        ) : (
          <table className="home-market-table">
            <thead>
              <tr>
                <SortableTh field="symbol" sortBy={sortBy} sortDesc={sortDesc} onSort={onSort}>
                  Token
                </SortableTh>
                <th>Exchanges</th>
                <SortableTh
                  field="lastPx"
                  sortBy={sortBy}
                  sortDesc={sortDesc}
                  onSort={onSort}
                  title="A green dot means the price is streaming live. Without one it is the last scan's price — only tokens in the trading roster have a live feed."
                >
                  Price
                </SortableTh>
                <SortableTh field="change24hPct" sortBy={sortBy} sortDesc={sortDesc} onSort={onSort}>
                  24h
                </SortableTh>
                <SortableTh
                  field="range24hPct"
                  sortBy={sortBy}
                  sortDesc={sortDesc}
                  onSort={onSort}
                  title="24h high minus low, as a % of price. Unlike 24h change it does not cancel out on a token that moved hard both ways and came back."
                >
                  Range
                </SortableTh>
                <SortableTh field="vol24hUsd" sortBy={sortBy} sortDesc={sortDesc} onSort={onSort}>
                  Volume 24h
                </SortableTh>
                <SortableTh
                  field="score"
                  sortBy={sortBy}
                  sortDesc={sortDesc}
                  onSort={onSort}
                  title="The scan's ranking composite: 55% volume, 25% absolute 24h change, 20% range — each relative to the day's largest. Volume dominates because liquidity decides whether this bot can trade a token at all."
                >
                  Score
                </SortableTh>
                <th>Status</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {pageRows.map((r) => {
                // A live price when the bot is streaming this token, the scan's own last price
                // otherwise. Never blank: an untracked token still has a real, if slightly older,
                // price, and showing nothing would read as missing data.
                const live = livePrices[r.symbol]
                const price = live ? Number(live) : r.lastPx
                return (
                  <tr key={r.symbol} className="home-market-row">
                    {/* Only the token name opens the chart (explicit request). A whole-row click
                        fires on any stray click in the table — selecting a price, missing a header —
                        and a table where every click navigates is hard to read from. */}
                    <td className="token-cell">
                      <TokenIcon symbol={r.symbol} />
                      <button
                        type="button"
                        className="token-symbol token-symbol-link"
                        onClick={() => setChartToken(r.symbol)}
                        title={`Open ${r.symbol} chart`}
                      >
                        {r.symbol}
                      </button>
                    </td>
                    <td>
                      <ExchangeBadges exchanges={r.exchanges} />
                    </td>
                    <td className="num" title={live ? 'live price (streaming)' : `last scan price, from ${r.priceFrom}`}>
                      {fmtPrice(price)}
                      {live && <span className="live-dot" title="streaming live" />}
                    </td>
                    <td className={'num ' + (r.change24hPct >= 0 ? 'pos' : 'neg')}>
                      {fmtPct(r.change24hPct)}
                    </td>
                    <td className="num">{r.range24hPct.toFixed(2)}%</td>
                    <td className="num">{fmtUSD(r.vol24hUsd)}</td>
                    <td className="num">{r.score.toFixed(3)}</td>
                    <td className="status-cell">
                      {/* Three independent flags, shown as three marks rather than one word: a token
                          can be collecting data and paper-trading while deliberately off for real
                          money, which is exactly what a discovered token looks like. */}
                      {r.inRoster ? (
                        <>
                          <span className="flag flag-on" title="collecting data / paper trading">
                            paper
                          </span>
                          {/* The real-money flag is distinguished by its TEXT, not only its color.
                              A headless pass over this page read both states as the same word
                              "real" and only the class differed — which is precisely how a glance
                              misreads the highest-stakes distinction on the page. Color alone is
                              not a label for whether real capital is at risk. */}
                          {r.enabledReal ? (
                            <span className="flag flag-on" title="enabled for real money">
                              bot ✓
                            </span>
                          ) : (
                            <span className="flag flag-off" title="not enabled for real money">
                              bot off
                            </span>
                          )}
                        </>
                      ) : (
                        <span className="flag flag-off" title="not in the trading roster">
                          untracked
                        </span>
                      )}
                    </td>
                    <td>
                      <Link to={`/trade/${r.symbol}`} className="btn-trade-link" title={`Trade ${r.symbol}`}>
                        Trade
                      </Link>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
        {rows.length > 0 && (
          <Pagination
            page={safePage}
            pageSize={pageSize}
            total={rows.length}
            onPageChange={setPage}
            onPageSizeChange={(size) => {
              setPageSize(size)
              setPage(0)
            }}
          />
        )}
      </div>

      {chartToken && (
        <TokenChartModal
          instId={chartToken}
          mode="paper"
          // The Home page has no positions of its own to overlay; the chart renders price alone here.
          // Passing the page's own (absent) positions rather than fetching them keeps this page from
          // pulling the two largest payloads in the app just to draw a chart someone clicked.
          positions={[] as Position[]}
          livePrices={livePrices}
          onClose={() => setChartToken(null)}
        />
      )}
    </div>
  )
}
