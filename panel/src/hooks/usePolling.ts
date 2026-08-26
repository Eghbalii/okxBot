import { useEffect, useRef, useState } from 'react'

interface PollingState<T> {
  data: T | null
  error: string | null
  loading: boolean
}

// Polls fetcher on an interval, starting immediately. Deliberately simple (no cache/dedup layer)
// — the panel's data volumes are small and this is the "foundations first" pass (CLAUDE.md §11).
export function usePolling<T>(fetcher: () => Promise<T>, intervalMs: number, deps: unknown[] = []) {
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

  return state
}
