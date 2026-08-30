// A clickable column header. Clicking sorts by that column; clicking the already-sorted
// column reverses the direction (CLAUDE.md §11.4's sortable-positions requirement).
export default function SortableTh<F extends string>({
  field,
  sortBy,
  sortDesc,
  onSort,
  children,
  className,
}: {
  field: F
  sortBy: F
  sortDesc: boolean
  onSort: (field: F) => void
  children: React.ReactNode
  className?: string
}) {
  const active = sortBy === field
  return (
    <th
      className={[active ? 'sorted' : '', className ?? ''].filter(Boolean).join(' ') || undefined}
      onClick={() => onSort(field)}
      title="Sort by this column; click again to reverse"
      aria-sort={active ? (sortDesc ? 'descending' : 'ascending') : 'none'}
    >
      {children}
      {active && <span className="sort-arrow">{sortDesc ? '▼' : '▲'}</span>}
    </th>
  )
}
