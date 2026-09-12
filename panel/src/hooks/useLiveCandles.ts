import { useEffect, useMemo, useState } from 'react'
import type { Candle } from '../api/types'

// barSeconds mirrors go-engine's own internal/usecase/tickfeed.go:barSeconds so a bucket boundary
// computed here lands on the same second the ingestor would have used. OKX's casing is
// significant (lowercase 'm' is minutes, capital 'M' is months — CLAUDE.md §9), and getting that
// wrong would silently bucket a 5m chart into 5-month candles rather than failing visibly.
// Returns 0 for anything unparseable, which callers treat as "don't build candles locally".
export function barSeconds(bar: string): number {
  const m = /^(\d+)([mHhDdWw]|M)$/.exec(bar)
  if (!m) return 0
  const n = Number(m[1])
  if (!n) return 0
  switch (m[2]) {
    case 'm':
      return n * 60
    case 'H':
    case 'h':
      return n * 3600
    case 'D':
    case 'd':
      return n * 86400
    case 'W':
    case 'w':
      return n * 604800
    case 'M':
      return n * 2592000
    default:
      return 0
  }
}

// bucketStart floors a millisecond epoch to the start of its bar, the same way a candle's own
// Timestamp is aligned. Bar boundaries are anchored to the Unix epoch, which is what OKX does for
// every timeframe this panel charts (5m/15m/1H all divide the day evenly, so epoch-anchored and
// midnight-anchored agree).
function bucketStart(ms: number, barSecs: number): number {
  return Math.floor(ms / (barSecs * 1000)) * (barSecs * 1000)
}

const iso = (ms: number) => new Date(ms).toISOString()

/**
 * useLiveCandles folds a live last-traded price into a fetched candle series, so a chart behaves
 * the way an exchange's does: the newest candle's close tracks the tick, its high/low stretch to
 * accommodate it, and crossing a bar boundary starts a fresh candle rather than waiting for the
 * next refetch.
 *
 * Why build candles client-side at all, rather than only refetching: the `candles` table holds
 * FINALIZED bars only (CLAUDE.md §7 — PaperTrader persists on confirm=1), and OKX marks a bar
 * confirm=1 only when the NEXT bar closes. Measured on the live stream 2026-09-12: the 12:20 bar
 * was confirmed at 12:25:01 — a full bar interval after it ended. So the newest server row is
 * always ~1 bar behind, and on a 1H chart that is an hour.
 *
 * That lag is why this hook keeps bars it has already completed (`done` below) instead of only the
 * one forming. Holding just the forming bar meant that at every boundary the bar the user had been
 * watching was discarded and would not reappear until the server caught up minutes later — the
 * chart grew no new candles, which is exactly the reported symptom.
 *
 * The locally-built candle is deliberately treated as provisional. `fetched` is the authority:
 * whenever a refetch brings back a bar this hook had been synthesizing, the real row replaces the
 * synthetic one wholesale. So a local candle's open/high/low being approximate (it only ever saw
 * ticks from the moment the chart was opened, not the whole bar) self-corrects on the next poll
 * rather than persisting as a wrong candle.
 */
export function useLiveCandles(fetched: Candle[] | null, bar: string, price: string | undefined): Candle[] | null {
  const barSecs = barSeconds(bar)

  // The series this hook is currently accumulating for. A 5m candle carried onto a 1H chart would
  // render at a timestamp that chart has no bar for, so the forming bar must be discarded when the
  // instrument or timeframe changes.
  const seriesKey = `${bar}:${fetched && fetched.length > 0 ? fetched[0].InstID : ''}`

  // The forming bar, accumulated from ticks. Held in state (not a ref) because the chart has to
  // re-render when it moves. Its own seriesKey is stored ALONGSIDE it rather than compared against
  // a ref during render: a ref would be advanced on the render that still returns the stale candle,
  // leaving a one-frame window where the previous timeframe's bar is drawn on the new chart.
  // Bundling them makes a stale entry unusable by construction — the read below simply ignores it.
  // `candles` holds the bar currently forming as its LAST entry, and any bars this hook completed
  // locally before them — oldest first, so the array is always chronological and can be appended
  // to the fetched series directly.
  const [live, setLive] = useState<{ key: string; candles: Candle[] } | null>(null)
  const localBars = live !== null && live.key === seriesKey ? live.candles : []

  // Newest fetched bar's start, so a tick is never folded into a bar the server has already
  // finalized — doing so would let a late tick reopen a closed candle.
  const newestFetchedMs =
    fetched && fetched.length > 0 ? Date.parse(fetched[fetched.length - 1].Timestamp) : null

  // The bar currently forming, as its own piece of state driven by a CLOCK rather than by ticks.
  // This is what makes rollover explicit: without it the effect below only re-runs when the price
  // changes, so a quiet instrument that does not tick across a boundary would keep extending the
  // previous bar. (It happened to work via the 15s refetch changing `fetched`'s identity — correct
  // by accident, and it would break the moment anyone memoized that array.) A dedicated timer makes
  // the boundary the thing that advances the bar, which is what it actually is.
  const [nowBucket, setNowBucket] = useState(() => (barSecs ? bucketStart(Date.now(), barSecs) : 0))
  useEffect(() => {
    if (!barSecs) return
    // Re-aligns on every fire rather than using a fixed interval: setInterval drifts, and a bar
    // opened a few seconds late would be stamped into the previous bucket.
    let timer: ReturnType<typeof setTimeout>
    const schedule = () => {
      const next = bucketStart(Date.now(), barSecs) + barSecs * 1000
      timer = setTimeout(() => {
        setNowBucket(bucketStart(Date.now(), barSecs))
        schedule()
      }, Math.max(next - Date.now(), 250))
    }
    setNowBucket(bucketStart(Date.now(), barSecs))
    schedule()
    return () => clearTimeout(timer)
  }, [barSecs])

  useEffect(() => {
    if (!barSecs || price === undefined || fetched === null || fetched.length === 0) return
    const p = Number(price)
    if (!Number.isFinite(p) || p <= 0) return

    const instId = fetched[fetched.length - 1].InstID
    const startMs = nowBucket
    // A tick belonging to an already-finalized bar (clock skew, or a bar the server persisted
    // while this tick was in flight) is dropped rather than rewriting that candle.
    if (newestFetchedMs !== null && startMs <= newestFetchedMs) return

    setLive((stored) => {
      // Bars kept from a previous series are unusable; start clean rather than mixing timeframes.
      const kept = stored !== null && stored.key === seriesKey ? stored.candles : []
      // Anything the server has now finalized is dropped here rather than left to the read below:
      // keeping it would grow this array without bound for as long as the chart stays open.
      const prior = kept.filter(
        (c) => newestFetchedMs === null || Date.parse(c.Timestamp) > newestFetchedMs,
      )
      const last = prior.length > 0 ? prior[prior.length - 1] : null

      if (last !== null && Date.parse(last.Timestamp) === startMs) {
        // Same bar: extend it. An unchanged close is skipped so an identical tick can't churn a
        // re-render — usePriceStream already dedupes, this covers a high/low-only no-op.
        const high = Math.max(Number(last.High), p)
        const low = Math.min(Number(last.Low), p)
        if (
          String(high) === last.High &&
          String(low) === last.Low &&
          String(p) === last.Close &&
          prior.length === kept.length
        ) {
          return stored
        }
        return {
          key: seriesKey,
          candles: [
            ...prior.slice(0, -1),
            { ...last, High: String(high), Low: String(low), Close: String(p) },
          ],
        }
      }

      // A new bucket. The bar that was forming is NOT discarded — it is kept as a completed candle,
      // because the server will not supply its real row for another full bar interval (see the
      // comment above this function). Its close becomes the new bar's open, so the two join up the
      // way real candles do instead of leaving a visual gap.
      const open = last !== null ? last.Close : String(p)
      return {
        key: seriesKey,
        candles: [
          ...prior,
          {
            InstID: instId,
            Bar: bar,
            Timestamp: iso(startMs),
            Open: open,
            High: String(Math.max(Number(open), p)),
            Low: String(Math.min(Number(open), p)),
            Close: String(p),
            Volume: '0',
          },
        ],
      }
    })
  }, [price, nowBucket, barSecs, bar, fetched, newestFetchedMs, seriesKey])

  return useMemo(() => {
    if (fetched === null) return null
    // A locally-built bar the server has since finalized is dropped in favour of the real row: the
    // fetched one has the true open and the real volume, and appending both would duplicate a
    // timestamp (which lightweight-charts rejects as unsorted data).
    const extra = localBars.filter(
      (c) => newestFetchedMs === null || Date.parse(c.Timestamp) > newestFetchedMs,
    )
    if (extra.length === 0) return fetched
    return [...fetched, ...extra]
  }, [fetched, localBars, newestFetchedMs])
}
