import { useEffect, useRef, useState } from 'react'
import { openEventsSocket } from '../api/client'

// usePriceStream maintains a live instId -> last-traded-price map off cmd/api's WebSocket bridge
// (CLAUDE.md §11.4), fed by the same socket usePositionEvents uses. Lets the positions panel show
// a live price and moment-to-moment PnL without polling REST for it.
//
// exchange (added 2026-09-23) filters the stream to one exchange's ticks before they ever reach the
// map — several tokens (SOL, AVAX, XRP, DOGE, ...) trade on both OKX and MEXC under the same short
// instId, and without this filter a MEXC page could silently show an OKX position's price on a
// same-named MEXC row, or vice versa, whichever tick happened to arrive last. Defaults to "okx"
// (an event with no exchange tag is an OKX tick, matching the backend's own convention) so every
// pre-existing caller that never passed this argument keeps its exact prior behavior.
export function usePriceStream(enabled: boolean, exchange: string = 'okx'): Record<string, string> {
  const [prices, setPrices] = useState<Record<string, string>>({})
  const pricesRef = useRef(prices)
  pricesRef.current = prices

  useEffect(() => {
    if (!enabled) return
    // A profile switch must not keep showing the previous exchange's stale prices under the new
    // one's token symbols — clear rather than merge.
    setPrices({})
    return openEventsSocket((event) => {
      if (event.type !== 'price') return
      if ((event.exchange || 'okx') !== exchange) return
      // Avoid a state update (and re-render) for every single tick when the price hasn't moved —
      // ticks arrive far more often than paper-order events, so this matters more here than it
      // would on that socket.
      if (pricesRef.current[event.instId] === event.price) return
      setPrices((prev) => ({ ...prev, [event.instId]: event.price }))
    })
  }, [enabled, exchange])

  return prices
}
