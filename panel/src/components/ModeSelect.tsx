import { useNavigate } from 'react-router-dom'
import type { PositionMode } from '../api/types'

/**
 * Paper/Real selector, in the header rather than above the positions table (2026-09-12 request).
 *
 * A dropdown rather than two tabs: it is a mode the whole panel operates in, not a page-level
 * filter, and one visible value with the alternative a click away reads as "you are HERE" more
 * clearly than two equal-weight buttons — which is the point, given the two modes differ by
 * whether real money moves.
 *
 * Rendered only on the positions routes, since it is the only page whose content is mode-scoped.
 */
export default function ModeSelect({ mode }: { mode: PositionMode }) {
  const navigate = useNavigate()
  return (
    <div className={'mode-select' + (mode === 'real' ? ' is-real' : '')}>
      <select
        value={mode}
        onChange={(e) => navigate(`/positions/${e.target.value}`)}
        aria-label="Trading mode"
        title="Which trading mode the panel is showing"
      >
        <option value="paper">Paper</option>
        <option value="real">Real</option>
      </select>
    </div>
  )
}
