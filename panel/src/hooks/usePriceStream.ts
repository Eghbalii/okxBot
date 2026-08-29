import { useEffect, useRef, useState } from 'react'
import { openEventsSocket } from '../api/client'

// usePriceStream maintains a live instId -> last-traded-price map off cmd/api's WebSocket bridge
// (CLAUDE.md §11.4), fed by the same socket usePositionEvents uses. Lets the positions panel show
// a live price and moment-to-moment PnL without polling REST for it.
export function usePriceStream(enabled: boolean): Record<string, string> {
  const [prices, setPrices] = useState<Record<string, string>>({})
  const pricesRef = useRef(prices)
  pricesRef.current = prices

  useEffect(() => {
    if (!enabled) return
    return openEventsSocket((event) => {
      if (event.type !== 'price') return
      // Avoid a state update (and re-render) for every single tick when the price hasn't moved —
      // ticks arrive far more often than paper-order events, so this matters more here than it
      // would on that socket.
      if (pricesRef.current[event.instId] === event.price) return
      setPrices((prev) => ({ ...prev, [event.instId]: event.price }))
    })
  }, [enabled])

  return prices
}
