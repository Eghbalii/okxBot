import { useMemo, useState } from 'react'
import type { Orderbook, OrderbookLevel } from '../hooks/useOrderbook'
import { trimPrice } from '../utils/format'

// Grouping levels, matching every major exchange's own ladder (docs/MANUAL_TRADE_PLAN.md §7).
// "1" means no grouping — the raw book as OKX sent it. Widening beyond books5's own 5-level depth
// is honest here (levels group whatever 5 rows this instrument's book actually has), but wide
// buckets on a 5-level book will often collapse to one or two rows — that IS the accurate picture
// at this depth, not a bug. A future per-instrument upgrade to the full `books` channel (400
// levels) when a viewer picks a wide grouping is the intended fix, not built yet; this component
// already has the seam for it (the `grouping` state below), since only the DATA needs to widen,
// not this rendering logic.
const GROUPINGS = [1, 10, 100, 1000] as const
type Grouping = (typeof GROUPINGS)[number]

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
export default function OrderbookLadder({ book, lastPrice }: { book: Orderbook | null; lastPrice?: string }) {
  const [grouping, setGrouping] = useState<Grouping>(1)

  const { asks, bids, maxCum } = useMemo(() => {
    if (!book) return { asks: [], bids: [], maxCum: 0 }
    const groupedAsks = withCumulative(group(book.asks, grouping, 'ask'))
    const groupedBids = withCumulative(group(book.bids, grouping, 'bid'))
    const max = Math.max(
      groupedAsks.length ? groupedAsks[groupedAsks.length - 1].cum : 0,
      groupedBids.length ? groupedBids[groupedBids.length - 1].cum : 0,
    )
    return { asks: groupedAsks, bids: groupedBids, maxCum: max }
  }, [book, grouping])

  // Asks render top-to-bottom as farthest-from-spread-first, so the best ask sits directly above
  // the spread row — the array itself is already best-first, so this is a simple reverse.
  const asksTopDown = useMemo(() => [...asks].reverse(), [asks])

  const spread = asks.length > 0 && bids.length > 0 ? asks[0].px - bids[0].px : null

  return (
    <div className="orderbook-ladder">
      <div className="ob-head">
        <span className="ob-head-title">Order Book</span>
        <div className="ob-grouping" role="group">
          {GROUPINGS.map((g) => (
            <button
              key={g}
              type="button"
              className={'ob-group-btn' + (grouping === g ? ' active' : '')}
              onClick={() => setGrouping(g)}
            >
              {g}
            </button>
          ))}
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
          <div className="ob-asks">
            {asksTopDown.map((l) => (
              <Row key={l.px} level={l} maxCum={maxCum} side="ask" />
            ))}
          </div>

          <div className="ob-spread-row">
            <span className={'mono ob-spread-px ' + (spread !== null && spread >= 0 ? 'text-green' : 'text-red')}>
              {trimPrice(lastPrice)}
            </span>
            {spread !== null && <span className="text-dim ob-spread-val">spread {trimPrice(spread)}</span>}
          </div>

          <div className="ob-bids">
            {bids.map((l) => (
              <Row key={l.px} level={l} maxCum={maxCum} side="bid" />
            ))}
          </div>
        </>
      )}
    </div>
  )
}
