import { lazy, Suspense } from 'react'
import { useChartEngine } from './useChartEngine'
import type { CandleChartProps, ChartEngine } from './types'

// Build-time default (2026-09-21 request): "keep the TradingView code, don't remove it — a future
// open-source user should be able to pick their own charting engine." VITE_CHART_ENGINE is read at
// build time (Vite inlines import.meta.env.* statically, so an unset/unrecognized value here falls
// back to 'tradingview', preserving every existing deployment's behavior).
const BUILD_DEFAULT: ChartEngine = import.meta.env.VITE_CHART_ENGINE === 'openalgo' ? 'openalgo' : 'tradingview'

// Lazy rather than a static import of both (2026-09-21 follow-up, same day): the runtime toggle
// below means EITHER engine can be picked at any time, and a static `import { CandleChart } from
// '../CandleChart'` alongside a static import of OpenAlgoCandleChart put both engines' full code
// in the main bundle unconditionally — measured at +67kB gzip on every page load, including pages
// that never open a chart, just to make an engine switch instant. lazy() turns each into its own
// chunk, fetched only once its engine is actually selected; the OTHER engine's chunk is never
// requested until (if ever) the toggle is used. A build that never renders ChartEngineToggle (the
// common case today) still only ever fetches the one chunk BUILD_DEFAULT resolves to.
const TradingViewCandleChart = lazy(() =>
  import('../CandleChart').then((m) => ({ default: m.CandleChart })),
)
const OpenAlgoCandleChartLazy = lazy(() =>
  import('./OpenAlgoCandleChart').then((m) => ({ default: m.OpenAlgoCandleChart })),
)

export function CandleChart(props: CandleChartProps) {
  const [engine] = useChartEngine(BUILD_DEFAULT)
  // A one-line fallback rather than nothing: without it, switching engines mid-session (the whole
  // point of the toggle) blanks the chart to a bare white gap for the one frame the new chunk
  // takes to fetch, which reads as broken rather than as a deliberate reload.
  return (
    <Suspense fallback={<p className="text-dim">Loading chart…</p>}>
      {engine === 'openalgo' ? <OpenAlgoCandleChartLazy {...props} /> : <TradingViewCandleChart {...props} />}
    </Suspense>
  )
}

/**
 * Which engine is ACTIVE right now, for a caller that overlays its own chrome on top of the chart
 * (TokenChartModal/TradePage's timeframe tabs) and needs to know whether to clear the OpenAlgo
 * rail's reserved left column (2026-09-21 fix — the overlay used to sit at a fixed top-left
 * corner regardless of engine, which is correct for TV's rail-less chart but lands directly on
 * OpenAlgo's drawing-tool column).
 */
export function useActiveChartEngine(): ChartEngine {
  const [engine] = useChartEngine(BUILD_DEFAULT)
  return engine
}

/**
 * Small engine picker (2026-09-21) so the two can be compared live without a rebuild. Deliberately
 * NOT rendered by CandleChart itself — it is one control for the whole session/page, not one per
 * chart instance, so callers place it once near their chart (TokenChartModal's header, TradePage's
 * chart toolbar) rather than it appearing next to every chart on a page that shows several.
 */
export function ChartEngineToggle({ className }: { className?: string }) {
  const [engine, setEngine] = useChartEngine(BUILD_DEFAULT)
  return (
    <div className={'chart-engine-toggle' + (className ? ' ' + className : '')} role="group" aria-label="Chart engine">
      {(['tradingview', 'openalgo'] as const).map((e) => (
        <button
          key={e}
          type="button"
          className={'chart-engine-btn' + (engine === e ? ' active' : '')}
          onClick={() => setEngine(e)}
          title={e === 'tradingview' ? 'TradingView Lightweight Charts (current default)' : 'OpenAlgo Charts (indicators/drawing-tools engine, in evaluation)'}
        >
          {e === 'tradingview' ? 'TradingView' : 'OpenAlgo'}
        </button>
      ))}
    </div>
  )
}

export type { ChartEngine } from './types'
