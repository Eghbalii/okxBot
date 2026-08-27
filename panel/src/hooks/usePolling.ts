import { useEffect, useRef, useState } from 'react'

interface PollingState<T> {
  data: T | null
  error: string | null
  loading: boolean
}

// Polls fetcher on an interval, starting immediately. Deliberately simple (no cache/dedup layer)
// — the panel's data volumes are small and this is the "foundations first" pass (CLAUDE.md §11).
//
// refreshSignal is an optional extra value (e.g. a counter bumped by usePositionEvents on a
// WebSocket push, CLAUDE.md §11.4/§12) that triggers an immediate fetch when it changes, without
// resetting the interval timer — lets a caller get a real-time-pushed update without abandoning
// the periodic poll as a fallback/consistency check.
export function usePolling<T>(
  fetcher: () => Promise<T>,
  intervalMs: number,
  deps: unknown[] = [],
  refreshSignal?: unknown,
) {
  const [state, setState] = useState<PollingState<T>>({ data: null, error: null, loading: true })
  const fetcherRef = useRef(fetcher)
  fetcherRef.current = fetcher

  useEffect(() => {
    let cancelled = false

    async function tick() {
      try {
        const data = await fetcherRef.current()
        if (!cancelled) setState({ data, error: null, loading: false })
      } catch (err) {
        if (!cancelled) {
          setState((prev) => ({ ...prev, error: (err as Error).message, loading: false }))
        }
      }
    }

    tick()
    const id = setInterval(tick, intervalMs)
    return () => {
      cancelled = true
      clearInterval(id)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)

  useEffect(() => {
    if (refreshSignal === undefined) return
    fetcherRef.current().then(
      (data) => setState({ data, error: null, loading: false }),
      (err) => setState((prev) => ({ ...prev, error: (err as Error).message, loading: false })),
    )
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refreshSignal])

  return state
}
