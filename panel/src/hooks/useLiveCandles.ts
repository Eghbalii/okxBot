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
 * Why build the forming candle client-side at all, rather than only refetching: the `candles`
 * table holds FINALIZED bars only (CLAUDE.md §7 — PaperTrader persists a bar when it closes), so
 * there is no server-side row for the bar currently forming. Without this the newest candle on a
 * 1H chart could sit up to an hour stale while the price ticked visibly in the table beside it.
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
  const [live, setLive] = useState<{ key: string; candle: Candle } | null>(null)
  const current = live !== null && live.key === seriesKey ? live.candle : null

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
      const prev = stored !== null && stored.key === seriesKey ? stored.candle : null
      if (prev !== null && Date.parse(prev.Timestamp) === startMs) {
        // Same bar: extend it. An unchanged close is skipped so an identical tick can't churn a
        // re-render — usePriceStream already dedupes, this covers a high/low-only no-op.
        const high = Math.max(Number(prev.High), p)
        const low = Math.min(Number(prev.Low), p)
        if (String(high) === prev.High && String(low) === prev.Low && String(p) === prev.Close) {
          return stored
        }
        return {
          key: seriesKey,
          candle: { ...prev, High: String(high), Low: String(low), Close: String(p) },
        }
      }
      // New bar (or the first tick since the chart opened). Opens at this tick: the true open is
      // whatever traded at the boundary, which a client that just connected never saw. The bar is
      // replaced by the server's own row once it finalizes, so the approximation is temporary.
      return {
        key: seriesKey,
        candle: {
          InstID: instId,
          Bar: bar,
          Timestamp: iso(startMs),
          Open: String(p),
          High: String(p),
          Low: String(p),
          Close: String(p),
          Volume: '0',
        },
      }
    })
  }, [price, nowBucket, barSecs, bar, fetched, newestFetchedMs, seriesKey])

  return useMemo(() => {
    if (fetched === null) return null
    if (current === null) return fetched
    // Guard against the server having finalized the bar this hook is synthesizing: the fetched
    // row wins, and the synthetic one is dropped rather than appended as a duplicate timestamp
    // (which lightweight-charts rejects as unsorted data).
    const liveMs = Date.parse(current.Timestamp)
    if (newestFetchedMs !== null && liveMs <= newestFetchedMs) return fetched
    return [...fetched, current]
  }, [fetched, current, newestFetchedMs])
}
