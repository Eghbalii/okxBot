import { useEffect, useMemo, useState } from 'react'
import { openEventsSocket } from '../api/client'
import type { Candle } from '../api/types'

/**
 * useLiveCandles folds live candle pushes into a fetched series, so the chart behaves the way an
 * exchange's does: the forming bar updates continuously and a closed bar stays put once it closes.
 *
 * It reads candles off the SAME WebSocket the live price comes from, not a REST poll. That matters
 * for correctness, not just latency — the two problems it fixes were both reported from real use:
 *
 *  1. The forming bar used to be synthesized from the price ticks this browser happened to receive,
 *     so its high/low were only ever "the extremes the panel saw". A wick that happened before the
 *     chart was opened, or in a tick the socket deduped, was simply missing — and only appeared
 *     later when the finalized row arrived from Postgres. That is the "shadows appear late" symptom.
 *  2. That correction lands a FULL BAR late, because OKX marks a bar confirm=1 only when the NEXT
 *     bar closes (measured on the live stream: the 12:20 bar was confirmed at 12:25:01).
 *
 * OKX's own confirm=0 pushes already carry the true OHLC of the forming bar (verified against the
 * live topic: open 0.003600, high 0.003610, low 0.003594 while still forming), so the exchange's
 * numbers replace the reconstruction entirely.
 *
 * `fetched` stays the authority for history: anything at or before its newest row is dropped here,
 * so a streamed bar can never contradict a stored one, and that same filter bounds this map rather
 * than letting it grow for as long as the chart stays open.
 *
 * exchange (added 2026-09-23, same reasoning as usePriceStream's own parameter): several tokens
 * (SOL, XRP, DOGE, ...) trade on both OKX and MEXC under the same short instId, so the live candle
 * stream is ambiguous without this filter. Defaults to "okx" so every pre-existing caller keeps its
 * exact prior behavior.
 */
export function useLiveCandles(
  fetched: Candle[] | null,
  instId: string,
  bar: string,
  exchange: string = 'okx',
): Candle[] | null {
  // Streamed bars for this (instId, bar), keyed by timestamp so a repeated push for the same bar
  // REPLACES it rather than appending a duplicate — OKX pushes the forming bar continuously.
  const [live, setLive] = useState<Map<number, Candle>>(new Map())

  useEffect(() => {
    // Reset on identity change: a 5m bar carried onto a 1H chart would render at a timestamp that
    // chart has no bar for.
    setLive(new Map())
    return openEventsSocket((event) => {
      if (event.type !== 'candle') return
      if (event.instId !== instId || event.bar !== bar) return
      if ((event.exchange || 'okx') !== exchange) return
      const ts = Number(event.ts)
      if (!Number.isFinite(ts)) return
      setLive((prev) => {
        const existing = prev.get(ts)
        // Skip the state update when nothing moved: OKX re-pushes the forming bar several times a
        // second and most pushes carry an identical OHLC, which would otherwise re-render the chart
        // for no visible change.
        if (
          existing !== undefined &&
          existing.Open === event.open &&
          existing.High === event.high &&
          existing.Low === event.low &&
          existing.Close === event.close &&
          existing.Volume === event.volume
        ) {
          return prev
        }
        const next = new Map(prev)
        next.set(ts, {
          InstID: instId,
          Bar: bar,
          Timestamp: new Date(ts).toISOString(),
          Open: event.open,
          High: event.high,
          Low: event.low,
          Close: event.close,
          Volume: event.volume,
        })
        return next
      })
    })
  }, [instId, bar, exchange])

  return useMemo(() => {
    if (fetched === null) return null
    const newestFetchedMs =
      fetched.length > 0 ? Date.parse(fetched[fetched.length - 1].Timestamp) : null
    // A streamed bar the server has already stored is dropped in favour of the fetched row: both
    // carry the same OHLC once finalized, and appending both would duplicate a timestamp (which
    // lightweight-charts rejects as unsorted data).
    const extra = [...live.entries()]
      .filter(([ts]) => newestFetchedMs === null || ts > newestFetchedMs)
      .sort((a, b) => a[0] - b[0])
      .map(([, c]) => c)
    if (extra.length === 0) return fetched
    return [...fetched, ...extra]
  }, [fetched, live])
}
