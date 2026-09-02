export const PAGE_SIZE_OPTIONS = [20, 50, 100, 200]
export const DEFAULT_PAGE_SIZE = 20

// Renders the page controls; the fetch itself is server-side paginated (CLAUDE.md §11.4,
// 2026-09-02 — the positions endpoint takes page/pageSize and returns just that page plus a
// total count, since fetching every row to paginate client-side had become a slow query and a
// multi-MB payload once closed positions numbered in the hundreds). onPageChange/onPageSizeChange
// just update state the caller's fetch depends on.
export default function Pagination({
  page,
  pageSize,
  total,
  onPageChange,
  onPageSizeChange,
}: {
  page: number
  pageSize: number
  total: number
  onPageChange: (page: number) => void
  onPageSizeChange: (size: number) => void
}) {
  const pageCount = Math.max(1, Math.ceil(total / pageSize))
  const first = total === 0 ? 0 : page * pageSize + 1
  const last = Math.min(total, (page + 1) * pageSize)

  return (
    <div className="pagination">
      <span className="text-dim">
        {total === 0 ? 'No rows' : `${first}–${last} of ${total}`}
      </span>
      <span className="spacer" />
      <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
        Per page
        <select
          value={pageSize}
          onChange={(e) => onPageSizeChange(Number(e.target.value))}
        >
          {PAGE_SIZE_OPTIONS.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </label>
      <button onClick={() => onPageChange(0)} disabled={page === 0} title="First page">
        «
      </button>
      <button onClick={() => onPageChange(page - 1)} disabled={page === 0}>
        Prev
      </button>
      <span className="text-dim">
        {page + 1} / {pageCount}
      </span>
      <button onClick={() => onPageChange(page + 1)} disabled={page >= pageCount - 1}>
        Next
      </button>
      <button
        onClick={() => onPageChange(pageCount - 1)}
        disabled={page >= pageCount - 1}
        title="Last page"
      >
        »
      </button>
    </div>
  )
}
