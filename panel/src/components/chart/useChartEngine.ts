import { useCallback, useEffect, useState } from 'react'
import type { ChartEngine } from './types'

// Runtime engine preference (2026-09-21 follow-up to the build-time switch), so the two engines
// can be compared live in the browser without a rebuild+redeploy cycle per look. Both engines'
// code ships in the bundle when this is active — a real cost relative to the build-time-only
// switch (~10kB gzip for openalgo-charts' core tier), accepted deliberately for the A/B window;
// the build-time env var (chart/index.tsx's own ENGINE constant) stays the source of truth for a
// deployment that has already decided and wants the smaller, single-engine bundle.
//
// localStorage rather than a URL param as the PERSISTED form: a query param is easy to lose on
// the very next navigation (this panel's routes don't carry query strings today), while a
// preference that resets itself every click would defeat the point of comparing them side by
// side. The `?engine=` URL override below still exists for a one-off link/screenshot, and writes
// through to storage so it "sticks" rather than being a one-shot query-string trick.

const STORAGE_KEY = 'okxbot.chartEngine.v1'

function isEngine(v: unknown): v is ChartEngine {
  return v === 'tradingview' || v === 'openalgo'
}

function readStored(): ChartEngine | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    return isEngine(raw) ? raw : null
  } catch {
    // Private window / blocked storage — fall through to the build-time default, same as every
    // other localStorage read in this panel (useTradeDefaults, useFavoriteTokens).
    return null
  }
}

function readUrlOverride(): ChartEngine | null {
  try {
    const v = new URLSearchParams(window.location.search).get('engine')
    return isEngine(v) ? v : null
  } catch {
    return null
  }
}

function writeStored(engine: ChartEngine) {
  try {
    localStorage.setItem(STORAGE_KEY, engine)
  } catch {
    // Same as above: the switch still works for this session, it just won't be remembered.
  }
}

/**
 * Resolves which engine to render THIS session: a `?engine=` URL param (if present, and also
 * persisted so it survives the next page load) wins first, then the stored preference, then the
 * build-time default (chart/index.tsx's ENGINE — the one that ships alone in a build where this
 * hook is never used).
 */
export function useChartEngine(buildDefault: ChartEngine): [ChartEngine, (e: ChartEngine) => void] {
  const [engine, setEngineState] = useState<ChartEngine>(() => readUrlOverride() ?? readStored() ?? buildDefault)

  // The URL override is applied once, on mount, and immediately persisted — it is a way to SET
  // the preference via a link, not a live binding to the address bar (which no page in this panel
  // otherwise reads on every render).
  useEffect(() => {
    const fromUrl = readUrlOverride()
    if (fromUrl) writeStored(fromUrl)
  }, [])

  const setEngine = useCallback((e: ChartEngine) => {
    setEngineState(e)
    writeStored(e)
  }, [])

  return [engine, setEngine]
}
