import { useCallback, useEffect, useRef, useState } from 'react'

// A small stale-while-revalidate cache for the panel's chart data.
//
// The problem it solves (2026-09-13 request): switching between open positions in the chart, or
// between the balance chart's day/week/month ranges, threw away data that was already loaded and
// re-fetched from scratch — blanking the view to a loading state each time. The backend is not the
// bottleneck (measured: /api/candles 10ms, /api/account/history 19ms); the cost is discarding a
// result the browser already had, then re-parsing and re-rendering it.
//
// So this is deliberately a CLIENT cache rather than a backend one. A server-side cache would not
// have helped: the data was already fast to produce and the waste was entirely in the round trip
// and the blank re-render.
//
// Behaviour, and the reason for each part:
//
//   - A cached value is returned SYNCHRONOUSLY on the first render for a key. Switching back to a
//     position looked at a moment ago is instant, with no loading state at all.
//   - Stale data is shown WHILE revalidating rather than being hidden. A chart that is 30 seconds
//     out of date is far more useful than an empty box, and the fresh data replaces it in place.
//   - `loading` is true only when there is genuinely nothing to show. Components use it to decide
//     between "Loading…" and rendering what they have, so a background refresh never blanks a view.

// Entry is one cached value plus when it was stored. `value` is absent for a key's first-ever
// load, between the fetch being issued and resolving — deliberately not defaulted to `undefined
// as T`, which used to happen and let StrictMode's double-invoked mount effect read that
// placeholder back and call `setData(undefined)`, a real, different state from "nothing loaded
// yet" (null) that broke a downstream `=== null` check (useLiveCandles crashed on it).
interface Entry<T> {
  value?: T
  storedAt: number
  // inFlight de-duplicates concurrent requests for the same key: two components mounting at once
  // (the chart and its position panel, say) share one network call rather than racing.
  inFlight?: Promise<T>
}

// The cache is module-level so it survives component unmounts — which is the whole point. A cache
// living in component state would be discarded exactly when the modal closes, i.e. right before the
// user reopens it.
const store = new Map<string, Entry<unknown>>()

// Bounded so a long session cannot grow it without limit. Candles for 10 instruments across 3 bars
// is 30 entries; 60 leaves room for balance ranges and a second exchange without ever evicting
// something still on screen. Eviction is oldest-first by insertion order, which Map preserves.
const MAX_ENTRIES = 60

function put<T>(key: string, entry: Entry<T>) {
  if (!store.has(key) && store.size >= MAX_ENTRIES) {
    const oldest = store.keys().next().value
    if (oldest !== undefined) store.delete(oldest)
  }
  store.set(key, entry as Entry<unknown>)
}

/** invalidate drops one key, or everything when called with no key. */
export function invalidateCache(key?: string) {
  if (key === undefined) store.clear()
  else store.delete(key)
}

export interface CachedResource<T> {
  data: T | null
  /** True only when there is nothing cached to show — never during a background revalidation. */
  loading: boolean
  error: string | null
  /** True while a request is in flight over data that is already on screen. */
  revalidating: boolean
  /** Forces a refetch, bypassing the freshness window. */
  refresh: () => void
}

/**
 * useCachedResource fetches `key`'s value, serving any cached copy immediately.
 *
 * `maxAgeMs` is how long a cached value is considered fresh enough to skip revalidation entirely.
 * Past it the value is still shown, and a refresh happens in the background.
 *
 * `refetchMs`, when set, revalidates on an interval while mounted — used by the candle chart so a
 * bar closing while the modal is open shows up as a real candle.
 */
export function useCachedResource<T>(
  key: string,
  fetcher: () => Promise<T>,
  opts: { maxAgeMs?: number; refetchMs?: number } = {},
): CachedResource<T> {
  const { maxAgeMs = 30_000, refetchMs } = opts

  // Read the cache during render so a cached value is on screen from the very first frame. Doing
  // this in an effect instead would paint one empty frame first, which is the flicker this exists
  // to remove.
  const cached = store.get(key) as Entry<T> | undefined
  const [data, setData] = useState<T | null>(cached?.value ?? null)
  const [error, setError] = useState<string | null>(null)
  const [revalidating, setRevalidating] = useState(false)

  // Keep the latest fetcher without making it a dependency: callers pass an inline closure, which
  // would otherwise change identity every render and refetch in a loop.
  const fetcherRef = useRef(fetcher)
  fetcherRef.current = fetcher

  const load = useCallback(
    async (force: boolean) => {
      const existing = store.get(key) as Entry<T> | undefined

      // Fresh enough, and not forced: show it and do nothing. existing.value can only be absent
      // here while its own fetch is still in flight, which the branch below already handles via
      // existing.inFlight — so reaching this branch with no value would mean a stale timestamp on
      // an empty entry, which "not fresh enough" correctly falls through on its own.
      if (!force && existing?.value !== undefined && Date.now() - existing.storedAt < maxAgeMs) {
        setData(existing.value)
        setError(null)
        return
      }

      // Someone is already fetching this key — join them instead of issuing a second request.
      if (existing?.inFlight) {
        setRevalidating(true)
        try {
          setData(await existing.inFlight)
          setError(null)
        } catch (e) {
          setError(e instanceof Error ? e.message : String(e))
        } finally {
          setRevalidating(false)
        }
        return
      }

      setRevalidating(true)
      const promise = fetcherRef.current()
      // Record the in-flight promise against whatever value already exists (undefined for a
      // key's first-ever load), so a concurrent caller — including React StrictMode's
      // double-invoked mount effect — reads back a genuinely-still-loading entry instead of a
      // `value: undefined as T` placeholder that used to be indistinguishable from a real value.
      put(key, { value: existing?.value, storedAt: existing?.storedAt ?? 0, inFlight: promise })

      try {
        const value = await promise
        put(key, { value, storedAt: Date.now() })
        setData(value)
        setError(null)
      } catch (e) {
        // Keep whatever was already cached on failure. A transient error must not blank a chart the
        // user is looking at — it reports the error alongside the stale data instead.
        if (existing) put(key, { value: existing.value, storedAt: existing.storedAt })
        else store.delete(key)
        setError(e instanceof Error ? e.message : String(e))
      } finally {
        setRevalidating(false)
      }
    },
    [key, maxAgeMs],
  )

  useEffect(() => {
    let cancelled = false

    // Adopt this key's cached value synchronously on a key change, so switching to an
    // already-loaded position shows its data in the same frame rather than after a round trip.
    // `hit.value` can be absent while a fetch for this key is still in flight (see load() above)
    // — that is exactly "nothing to show yet", i.e. null, never `undefined` itself.
    const hit = store.get(key) as Entry<T> | undefined
    setData(hit?.value ?? null)
    setError(null)

    void load(false)

    if (!refetchMs) return () => { cancelled = true }
    const id = setInterval(() => {
      if (!cancelled) void load(true)
    }, refetchMs)
    return () => {
      cancelled = true
      clearInterval(id)
    }
  }, [key, load, refetchMs])

  const refresh = useCallback(() => void load(true), [load])

  return { data, loading: data === null && error === null, error, revalidating, refresh }
}
