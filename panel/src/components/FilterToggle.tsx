/**
 * All / Active filter for the manage-strategies and manage-tokens modals (2026-09-12 request).
 *
 * Filters what is DISPLAYED, never what is selected: a row hidden by this toggle keeps its checked
 * state and is still saved. Filtering the selection itself would mean switching to "Active" and
 * pressing Save silently dropped everything that scrolled out of view — a destructive surprise from
 * what reads as a view control.
 *
 * Shared by both modals so the two cannot drift into behaving differently; the counts are passed in
 * because each modal counts a different thing.
 */
export default function FilterToggle({
  activeOnly,
  onChange,
  activeCount,
  totalCount,
  noun,
}: {
  activeOnly: boolean
  onChange: (v: boolean) => void
  activeCount: number
  totalCount: number
  noun: string
}) {
  return (
    <div className="filter-toggle" role="group" aria-label={`Filter ${noun}`}>
      <button
        type="button"
        className={'filter-btn' + (activeOnly ? '' : ' active')}
        onClick={() => onChange(false)}
      >
        All <span className="filter-count">{totalCount}</span>
      </button>
      <button
        type="button"
        className={'filter-btn' + (activeOnly ? ' active' : '')}
        onClick={() => onChange(true)}
      >
        Active <span className="filter-count">{activeCount}</span>
      </button>
    </div>
  )
}
