import { Navigate, useParams } from 'react-router-dom'
import PositionsTable from '../components/PositionsTable'
import type { PositionMode } from '../api/types'

/**
 * Thin URL-parsing wrapper around PositionsTable for the /positions/:mode routes.
 *
 * 'manual' moved out of here entirely (2026-09-20 request): manually-opened positions are now
 * shown as their own PositionsTable instance at the bottom of the Trade page instead of as a third
 * mode tab here — orders are opened FROM Trade, so reviewing them there is where they belong, and
 * this page's own mode selector (paper/bot) never needs a manual entry.
 */
export default function PositionsPage() {
  const { mode: rawMode } = useParams<{ mode: string }>()
  // Only 'paper'/'bot' are valid route segments now — an unrecognized value (a stale bookmark to
  // the old /positions/manual, a typo) redirects to paper rather than silently misinterpreting it.
  if (rawMode !== 'paper' && rawMode !== 'bot') {
    return <Navigate to="/positions/paper" replace />
  }
  const mode: PositionMode = rawMode

  return <PositionsTable mode={mode} />
}
