import { useState } from 'react'
import OptimizerPanel from '../components/OptimizerPanel'
import type { PositionMode } from '../api/types'

// Strategy lifecycle here is owned entirely by the backtest/optimize pipeline (OptimizerPanel) —
// this page no longer shows the old flat "all 63 origin strategies" table (2026-09-28, explicit
// operator instruction: that trade-count/win-rate view on origins was clutter with no useful
// data, since origins themselves never trade — only pipeline-promoted candidates do) nor the
// price-chart "Parameter change history" panel that used to sit below it (2026-09-28, explicit
// operator instruction: it wasn't giving useful data). Parameter-change history is still
// reachable — click a strategy's name in OptimizerPanel's Active/Rejected tables to open
// ParamChangeModal, the old/new-value table this page used to show as a chart.
export default function StrategiesPage() {
  // Paper and bot trading each have a fully independent track record and assignment set
  // (CLAUDE.md real-trading readiness plan, 2026-09-04) — this tab picks which one the optimizer
  // data below reflects, same pattern as the Positions page's own tabs.
  const [mode, setMode] = useState<PositionMode>('paper')

  return (
    <div>
      <div className="mode-tabs" style={{ marginBottom: '0.9rem' }}>
        {(['paper', 'bot'] as PositionMode[]).map((m) => (
          <button
            key={m}
            className={'mode-tab' + (mode === m ? ' active' : '')}
            onClick={() => setMode(m)}
          >
            {m[0].toUpperCase() + m.slice(1)}
          </button>
        ))}
      </div>

      <OptimizerPanel mode={mode} />
    </div>
  )
}
