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

function Row({
  level,
  maxCum,
  side,
}: {
  level: OrderbookLevel & { cum: number }
  maxCum: number
  side: 'ask' | 'bid'
}) {
  const barPct = maxCum > 0 ? (level.cum / maxCum) * 100 : 0
  return (
    <div className={'ob-row ob-row-' + side}>
      <div className="ob-row-bar" style={{ width: `${barPct}%` }} />
      <span className={'ob-px mono ' + (side === 'ask' ? 'text-red' : 'text-green')}>{trimPrice(level.px)}</span>
      <span className="ob-sz mono">{level.sz.toFixed(3)}</span>
    </div>
  )
}

/**
 * Split two-column order book ladder (asks right, bids left) — matches the layout used by most
 * exchanges' desktop UIs (docs/MANUAL_TRADE_PLAN.md §7, explicit operator preference over the
 * single-stacked-ladder alternative).
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

  const spread = asks.length > 0 && bids.length > 0 ? asks[0].px - bids[0].px : null

  return (
    <div className="orderbook-ladder">
      <div className="ob-head">
        <span className="ob-head-title">Order book</span>
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

      {!book ? (
        <p className="text-dim">Waiting for order book data…</p>
      ) : (
        <>
          <div className="ob-columns">
            <div className="ob-col ob-col-bids">
              {/* Bids read top-to-bottom as best-first, same direction as asks, so the two columns'
                  rows sit at matching heights either side of the spread. */}
              {bids.map((l) => (
                <Row key={l.px} level={l} maxCum={maxCum} side="bid" />
              ))}
            </div>
            <div className="ob-col ob-col-asks">
              {asks.map((l) => (
                <Row key={l.px} level={l} maxCum={maxCum} side="ask" />
              ))}
            </div>
          </div>
          <div className="ob-spread">
            {lastPrice && <span className="mono">{trimPrice(lastPrice)}</span>}
            {spread !== null && (
              <span className="text-dim ob-spread-val">spread {trimPrice(spread)}</span>
            )}
          </div>
        </>
      )}
    </div>
  )
}
