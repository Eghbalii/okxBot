import { useMemo, useState } from 'react'
import type { Orderbook, OrderbookLevel } from '../hooks/useOrderbook'
import { trimPrice } from '../utils/format'

// Grouping steps are anchored to the instrument's OWN REAL TICK SIZE (2026-09-19, fixed again same
// day after two wrong attempts): the smallest bucket must equal the exchange's actual tick — the
// real, per-instrument-variable price increment OKX itself trades in — never a value guessed from
// the price's own order of magnitude. Two earlier attempts both failed because that ratio isn't
// fixed across instruments: BTC's tick (0.1) sits 5 orders of magnitude below its own leading
// digit (~80000), while PUMP's tick (0.0001) sits only 1 order of magnitude below its leading
// digit (~0.004) — no single price-derived formula produces both correctly, confirmed directly
// with the operator. The real tick size (domain.Instrument.TickSz, already fetched for order
// sizing) is the only correct source. `tickSz` is optional (undefined while the instrument lookup
// hasn't resolved yet) — the log-scale estimate is kept ONLY as a same-tab fallback for that brief
// window, never as the steady-state behavior.
function groupingsFor(price: number, tickSz?: number): number[] {
  let base: number
  if (tickSz !== undefined && Number.isFinite(tickSz) && tickSz > 0) {
    base = tickSz
  } else {
    const p = Number.isFinite(price) && price > 0 ? price : 1
    base = 10 ** (Math.floor(Math.log10(p)) - 4)
  }
  return [base, base * 10, base * 100, base * 1000]
}

type Grouping = number

// Display mode — three states matching the reference screenshot's own row of icons: both sides at
// once (the default, the existing behavior), or ONE side only with the freed-up space going to
// MORE rows of that side. TOTAL_ROWS is split entirely to whichever side is showing, so choosing
// a single-side view is genuinely "see deeper into this side," not just "hide the other one."
type DisplayMode = 'both' | 'bids' | 'asks'
const TOTAL_ROWS = 16

// Buckets levels into `grouping`-wide price bands, summing size within each band, labeled by the
// band's LOWER edge on both sides (floor(px/step)*step) — the standard exchange convention. Both
// sides sharing one grid is what matters: bids and asks must never round toward each other (an
// earlier version rounded bids UP and asks DOWN, which could push a bid bucket's label past an ask
// bucket's and render a negative spread — caught in testing, not by inspection). Flooring both
// means a bucket's label is always <= every raw price that fell into it, so the best bid's bucket
// can never exceed the best ask's.
function group(levels: OrderbookLevel[], grouping: Grouping, side: 'ask' | 'bid'): OrderbookLevel[] {
  if (grouping <= 1) return levels
  const buckets = new Map<number, number>()
  for (const l of levels) {
    const bucket = Math.floor(l.px / grouping) * grouping
    buckets.set(bucket, (buckets.get(bucket) ?? 0) + l.sz)
  }
  const out = [...buckets.entries()].map(([px, sz]) => ({ px, sz }))
  out.sort((a, b) => (side === 'ask' ? a.px - b.px : b.px - a.px))
  return out
}

function withCumulative(levels: OrderbookLevel[]): (OrderbookLevel & { cum: number })[] {
  let running = 0
  return levels.map((l) => {
    running += l.sz
    return { ...l, cum: running }
  })
}

/**
 * One ladder row: price / size / cumulative-sum, with a depth bar filling from the right edge
 * behind the sum column — the single-stacked-book convention (asks above the spread, bids below,
 * both reading best-price-nearest-the-middle) used by Binance/most exchanges' own default layout,
 * explicit operator preference over a side-by-side split ladder.
 */
function Row({ level, maxCum, side }: { level: OrderbookLevel & { cum: number }; maxCum: number; side: 'ask' | 'bid' }) {
  const barPct = maxCum > 0 ? (level.cum / maxCum) * 100 : 0
  return (
    <div className="ob-row">
      <div className={'ob-row-bar ob-row-bar-' + side} style={{ width: `${barPct}%` }} />
      <span className={'ob-px mono ' + (side === 'ask' ? 'text-red' : 'text-green')}>{trimPrice(level.px)}</span>
      <span className="ob-sz mono">{level.sz.toFixed(3)}</span>
      <span className="ob-cum mono">{level.cum.toFixed(3)}</span>
    </div>
  )
}

/**
 * Single stacked order-book ladder — asks above the spread (best ask nearest the middle,
 * descending upward), the live price in the middle, bids below (best bid nearest the middle,
 * descending downward). Matches the reference layout the operator asked for directly (Binance's
 * own order-book panel), replacing an earlier side-by-side two-column attempt.
 */
export default function OrderbookLadder({
  book,
  lastPrice,
  tickSz,
}: {
  book: Orderbook | null
  lastPrice?: string
  // The instrument's real exchange tick size (e.g. "0.1" for BTC, "0.0001" for PUMP) — the only
  // correct source for the smallest grouping bucket (groupingsFor's own doc comment). Optional:
  // undefined during the brief window before TradePage's instrument lookup resolves, in which case
  // groupingsFor falls back to a rough estimate rather than blocking the whole ladder on it.
  tickSz?: string
}) {
  // Reference price for deriving groupings: the live last-traded price when available, falling
  // back to the book's own best ask/bid — a book can arrive slightly before the first tick does.
  const refPrice = useMemo(() => {
    const live = Number(lastPrice)
    if (Number.isFinite(live) && live > 0) return live
    if (book?.asks.length) return book.asks[0].px
    if (book?.bids.length) return book.bids[0].px
    return NaN
  }, [lastPrice, book])

  const tickSzNum = useMemo(() => {
    const n = Number(tickSz)
    return Number.isFinite(n) && n > 0 ? n : undefined
  }, [tickSz])

  const groupings = useMemo(() => groupingsFor(refPrice, tickSzNum), [refPrice, tickSzNum])
  const [grouping, setGrouping] = useState<Grouping>(groupings[0])
  const [mode, setMode] = useState<DisplayMode>('both')

  // If the instrument (and therefore its price scale) changes out from under an already-selected
  // grouping, snap back to the finest step for the new scale rather than silently keeping a
  // bucket size computed for a different token's price range.
  const groupingIsValid = groupings.includes(grouping)
  const effectiveGrouping = groupingIsValid ? grouping : groupings[0]

  const { asks, bids, maxCum } = useMemo(() => {
    if (!book) return { asks: [], bids: [], maxCum: 0 }
    const groupedAsks = withCumulative(group(book.asks, effectiveGrouping, 'ask'))
    const groupedBids = withCumulative(group(book.bids, effectiveGrouping, 'bid'))
    const max = Math.max(
      groupedAsks.length ? groupedAsks[groupedAsks.length - 1].cum : 0,
      groupedBids.length ? groupedBids[groupedBids.length - 1].cum : 0,
    )
    return { asks: groupedAsks, bids: groupedBids, maxCum: max }
  }, [book, effectiveGrouping])

  // Row budget per mode: 'both' splits TOTAL_ROWS evenly (the existing behavior), a single-side
  // mode gives that whole budget to the one side showing — "see deeper into this side," not just
  // hiding the other one (explicit operator request). The bar-depth scale (maxCum) is still
  // computed above from the FULL grouped set before slicing, so a single-side view's depth bars
  // read on the same scale as the two-sided view rather than re-normalizing to a smaller max.
  const askBudget = mode === 'asks' ? TOTAL_ROWS : mode === 'bids' ? 0 : TOTAL_ROWS / 2
  const bidBudget = mode === 'bids' ? TOTAL_ROWS : mode === 'asks' ? 0 : TOTAL_ROWS / 2

  const asksShown = useMemo(() => asks.slice(0, askBudget), [asks, askBudget])
  const bidsShown = useMemo(() => bids.slice(0, bidBudget), [bids, bidBudget])

  // Asks render top-to-bottom as farthest-from-spread-first, so the best ask sits directly above
  // the spread row — the array itself is already best-first, so this is a simple reverse.
  const asksTopDown = useMemo(() => [...asksShown].reverse(), [asksShown])

  const spread = asks.length > 0 && bids.length > 0 ? asks[0].px - bids[0].px : null

  return (
    <div className="orderbook-ladder">
      <div className="ob-head">
        <span className="ob-head-title">Order Book</span>
        {/* Both-sides / bids-only / asks-only toggle — matches the reference screenshot's own
            3-icon row (explicit operator request, asked three times the same day). The default
            ('both') is today's existing view; picking one side hands it the WHOLE row budget. */}
        <div className="ob-mode" role="group">
          <button
            type="button"
            className={'ob-mode-btn' + (mode === 'both' ? ' active' : '')}
            title="Both sides"
            onClick={() => setMode('both')}
          >
            <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
              <rect x="1" y="2" width="14" height="5" fill="currentColor" opacity="0.85" />
              <rect x="1" y="9" width="14" height="5" fill="currentColor" opacity="0.4" />
            </svg>
          </button>
          <button
            type="button"
            className={'ob-mode-btn' + (mode === 'bids' ? ' active' : '')}
            title="Bids only"
            onClick={() => setMode('bids')}
          >
            <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
              <rect x="1" y="2" width="14" height="12" fill="currentColor" className="text-green" opacity="0.85" />
            </svg>
          </button>
          <button
            type="button"
            className={'ob-mode-btn' + (mode === 'asks' ? ' active' : '')}
            title="Asks only"
            onClick={() => setMode('asks')}
          >
            <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
              <rect x="1" y="2" width="14" height="12" fill="currentColor" className="text-red" opacity="0.85" />
            </svg>
          </button>
        </div>
      </div>

      <div className="ob-col-head">
        <span>Price</span>
        <span>Size</span>
        <span>Sum</span>
      </div>

      {!book ? (
        <p className="text-dim ob-waiting">Waiting for order book data…</p>
      ) : (
        <>
          {mode !== 'bids' && (
            <div className="ob-asks">
              {asksTopDown.map((l) => (
                <Row key={l.px} level={l} maxCum={maxCum} side="ask" />
              ))}
            </div>
          )}

          <div className="ob-spread-row">
            <span className={'mono ob-spread-px ' + (spread !== null && spread >= 0 ? 'text-green' : 'text-red')}>
              {trimPrice(lastPrice)}
            </span>
            {spread !== null && <span className="text-dim ob-spread-val">spread {trimPrice(spread)}</span>}
          </div>

          {mode !== 'asks' && (
            <div className="ob-bids">
              {bidsShown.map((l) => (
                <Row key={l.px} level={l} maxCum={maxCum} side="bid" />
              ))}
            </div>
          )}
        </>
      )}

      {/* Grouping control at the bottom of the book: a single DROPDOWN, not one button per step
          (explicit operator correction — the reference screenshot shows one <select>-style menu,
          not a row of toggle buttons). Labels show the actual bucket size in the instrument's own
          price units, anchored to its real tick size. */}
      <div className="ob-grouping">
        <select
          className="ob-group-select"
          value={effectiveGrouping}
          onChange={(e) => setGrouping(Number(e.target.value))}
        >
          {groupings.map((g) => (
            <option key={g} value={g}>
              {trimPrice(g)}
            </option>
          ))}
        </select>
      </div>
    </div>
  )
}
