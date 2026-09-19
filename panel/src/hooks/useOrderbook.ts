import { useEffect, useState } from 'react'
import { openEventsSocket } from '../api/client'
import type { OrderbookUpdate } from '../api/client'

export interface OrderbookLevel {
  px: number
  sz: number
}

export interface Orderbook {
  asks: OrderbookLevel[] // lowest ask first
  bids: OrderbookLevel[] // highest bid first
  ts: number
}

/**
 * useOrderbook maintains the latest books5 snapshot for one instrument (docs/MANUAL_TRADE_PLAN.md
 * §7), read off the same WebSocket bridge prices/candles already come from.
 *
 * Every push from cmd/api is a full 5-level snapshot (OKX's own books5 shape), so this is a plain
 * "keep the latest one for this instId" — no delta merge, no checksum, unlike the deeper `books`
 * channel this project deliberately does not use yet (§7's own note: 5 levels isn't enough real
 * depth to aggregate into wide buckets like 100/1000 accurately, so that upgrade is left for when
 * a viewer actually asks for a wider grouping).
 *
 * The book is broadcast for EVERY configured instrument, all the time (a permanent, full-roster
 * subscription in cmd/ingestor, not a per-viewer one) — this hook's only job is to pick out the one
 * instId the caller wants, the same client-side-filter pattern usePriceStream/useLiveCandles use.
 */
export function useOrderbook(instId: string): Orderbook | null {
  const [book, setBook] = useState<Orderbook | null>(null)

  useEffect(() => {
    // Reset on token switch — a stale BTC book must never render as if it were ETH's while the
    // fresh one is in flight.
    setBook(null)
    return openEventsSocket((event) => {
      if (event.type !== 'orderbook' || event.instId !== instId) return
      const e = event as OrderbookUpdate
      setBook({
        asks: e.asks.map((l) => ({ px: Number(l.px), sz: Number(l.sz) })),
        bids: e.bids.map((l) => ({ px: Number(l.px), sz: Number(l.sz) })),
        ts: Number(e.ts),
      })
    })
  }, [instId])

  return book
}
