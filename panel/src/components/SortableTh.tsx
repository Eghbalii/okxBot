// A clickable column header. Clicking sorts by that column; clicking the already-sorted
// column reverses the direction (CLAUDE.md §11.4's sortable-positions requirement).
export default function SortableTh<F extends string>({
  field,
  sortBy,
  sortDesc,
  onSort,
  children,
  className,
  title,
}: {
  field: F
  sortBy: F
  sortDesc: boolean
  onSort: (field: F) => void
  children: React.ReactNode
  className?: string
  /**
   * Explains what the column MEANS, appended to the sort hint rather than replacing it.
   *
   * Added because a computed column's header cannot carry its own definition — "Range" and "Score"
   * on the Home page were both asked about directly, which is the evidence that the page has to
   * answer rather than the reader having to find the code.
   */
  title?: string
}) {
  const active = sortBy === field
  const sortHint = 'Click to sort; click again to reverse'
  return (
    <th
      className={[active ? 'sorted' : '', className ?? ''].filter(Boolean).join(' ') || undefined}
      onClick={() => onSort(field)}
      title={title ? `${title}\n\n${sortHint}` : sortHint}
      aria-sort={active ? (sortDesc ? 'descending' : 'ascending') : 'none'}
    >
      {children}
      {active && <span className="sort-arrow">{sortDesc ? '▼' : '▲'}</span>}
    </th>
  )
}
