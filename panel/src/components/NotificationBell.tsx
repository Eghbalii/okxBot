import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../api/client'
import type { Position } from '../api/types'
import { formatDateTime, tokenSymbol } from '../utils/format'

const SEEN_KEY = 'okxbot.seenOrderErrors'
const PREVIEW_LIMIT = 5
const PAGE_SIZE = 10
const POLL_MS = 15000

// One notification per (order, failure time). The timestamp is part of the identity on purpose: a
// NEW failure on an order whose previous error was already read is genuinely new and must count
// again, which an id-only key could not express.
const keyOf = (p: Position) => `${p.Mode}:${p.ID}:${p.LastErrorAt ?? ''}`

function loadSeen(): Set<string> {
  try {
    const raw = localStorage.getItem(SEEN_KEY)
    return new Set<string>(raw ? (JSON.parse(raw) as string[]) : [])
  } catch {
    // A corrupt or unavailable store must not break the header — an empty set just means
    // everything shows as unread, which is the safe direction for an unresolved exchange error.
    return new Set()
  }
}

function saveSeen(seen: Set<string>): void {
  try {
    // Bounded so the key cannot grow without limit over the life of the deployment.
    localStorage.setItem(SEEN_KEY, JSON.stringify([...seen].slice(-500)))
  } catch {
    /* storage full or blocked — the UI still works, it just re-shows on reload */
  }
}

/**
 * Exchange-failure notifications, as a header bell with an unread count.
 *
 * Replaces the modal that used to interrupt every page load (2026-09-11). Its dismissal lived in
 * component state, so a reload re-raised every error that had ever been dismissed — correct as a
 * "never lose an unresolved failure" default, but in practice it meant the same 13 errors blocking
 * the panel on every visit. Read-state is now persisted, so seen means seen.
 *
 * Fetches its own data rather than taking it from the positions table: the bell lives in the app
 * header and must report the same count on every page, and the table only ever holds one mode's
 * current page.
 */
export default function NotificationBell() {
  const [errors, setErrors] = useState<Position[]>([])
  const [seen, setSeen] = useState<Set<string>>(loadSeen)
  const [openPanel, setOpenPanel] = useState(false)
  const [showAll, setShowAll] = useState(false)
  const [page, setPage] = useState(0)
  const wrapRef = useRef<HTMLDivElement>(null)

  const load = useCallback(async () => {
    try {
      // Both modes, newest failure first. Closed orders are included deliberately: a close that
      // failed is exactly the case worth seeing, and it is closed by then.
      // Real mode ONLY, and not as an optimisation: paper_orders has no last_error column at
      // all. Exchange failures are structural to real trading — paper trading never contacts an
      // exchange — so the paper request could never have returned a single notification. It was
      // costing a 6.9MB download (those rows carry FeaturesJSON, the whole decision-time
      // observation) that blocked the badge for seconds behind data with nothing to show.
      const r = await api.listPositions({
        mode: 'real',
        pageSize: 100,
        sortBy: 'opened_at',
        sortDesc: true,
      })
      const withErrors = r.items.filter((p) => p.LastError)
      withErrors.sort((a, b) => (b.LastErrorAt ?? '').localeCompare(a.LastErrorAt ?? ''))
      setErrors(withErrors)
    } catch (e) {
      // Leave the previous list in place: a transient API blip should not clear the bell. Logged
      // rather than swallowed — a silently empty bell is indistinguishable from "no errors", which
      // is the worst possible failure mode for this particular component.
      console.error('notification bell: could not load exchange errors', e)
    }
  }, [])

  useEffect(() => {
    // StrictMode mounts, unmounts, and remounts once. The first mount's in-flight load resolved
    // into the discarded instance, so the bell sat empty until the 15s poll — measured: badge 0 at
    // t=3s and t=8s, then correct 13 at t=20s. Retrying shortly after mount closes that window
    // without shortening the poll, which would only add load to cover a startup case.
    let alive = true
    const run = () => {
      if (alive) void load()
    }
    run()
    const kick = setTimeout(run, 1200)
    const t = setInterval(run, POLL_MS)
    return () => {
      alive = false
      clearTimeout(kick)
      clearInterval(t)
    }
  }, [load])

  // Close the dropdown on an outside click, the usual expectation for this control.
  useEffect(() => {
    if (!openPanel) return
    const onDown = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpenPanel(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [openPanel])

  const unread = useMemo(() => errors.filter((p) => !seen.has(keyOf(p))), [errors, seen])

  const markSeen = useCallback((items: Position[]) => {
    if (items.length === 0) return
    setSeen((prev) => {
      const next = new Set(prev)
      for (const p of items) next.add(keyOf(p))
      saveSeen(next)
      return next
    })
  }, [])

  // Opening the dropdown marks exactly what it shows — not the whole backlog, since the rest has
  // not actually been looked at yet.
  const toggle = () => {
    const next = !openPanel
    setOpenPanel(next)
    if (next) markSeen(errors.slice(0, PREVIEW_LIMIT))
  }

  const openAll = () => {
    setShowAll(true)
    setOpenPanel(false)
    setPage(0)
  }

  const pageCount = Math.max(1, Math.ceil(errors.length / PAGE_SIZE))
  const pageItems = errors.slice(page * PAGE_SIZE, page * PAGE_SIZE + PAGE_SIZE)

  // Keyed on the page's CONTENTS, not the array: pageItems is rebuilt on every render, so
  // depending on it re-ran this effect continuously — and since markSeen sets state, that loop
  // marked the whole backlog read the moment the component mounted. The badge showed 0 while
  // "More (8 older)" proved 13 errors were loaded, which is what gave the bug away.
  const pageKey = pageItems.map(keyOf).join(',')
  useEffect(() => {
    if (!showAll) return
    markSeen(pageItems)
    // pageItems is intentionally absent: pageKey is its stable identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [showAll, pageKey, markSeen])

  return (
    <div className="notif-wrap" ref={wrapRef}>
      <button
        className={'notif-bell' + (unread.length > 0 ? ' has-unread' : '')}
        onClick={toggle}
        title={
          errors.length === 0
            ? 'No exchange errors'
            : `${errors.length} exchange error${errors.length === 1 ? '' : 's'}, ${unread.length} unread`
        }
        aria-label="Exchange error notifications"
      >
        <BellIcon />
        {unread.length > 0 && (
          <span className="notif-badge">{unread.length > 99 ? '99+' : unread.length}</span>
        )}
      </button>

      {openPanel && (
        <div className="notif-panel">
          <div className="notif-panel-head">
            <strong>Exchange errors</strong>
            <span className="text-dim">{errors.length} total</span>
          </div>

          {errors.length === 0 ? (
            <div className="notif-empty">Nothing to report.</div>
          ) : (
            <>
              <ul className="notif-list">
                {errors.slice(0, PREVIEW_LIMIT).map((p) => (
                  <NotifRow key={keyOf(p)} p={p} unread={!seen.has(keyOf(p))} />
                ))}
              </ul>
              {errors.length > PREVIEW_LIMIT && (
                <button className="notif-more" onClick={openAll}>
                  More ({errors.length - PREVIEW_LIMIT} older)
                </button>
              )}
            </>
          )}
        </div>
      )}

      {showAll && (
        <div className="modal-backdrop" onClick={() => setShowAll(false)}>
          <div className="modal" style={{ maxWidth: 780 }} onClick={(e) => e.stopPropagation()}>
            <div className="modal-header">
              <h2>Exchange errors ({errors.length})</h2>
            </div>
            <ul className="notif-list notif-list-full">
              {pageItems.map((p) => (
                <NotifRow key={keyOf(p)} p={p} unread={!seen.has(keyOf(p))} full />
              ))}
            </ul>
            {pageCount > 1 && (
              <div className="notif-pager">
                <button className="btn" disabled={page === 0} onClick={() => setPage((n) => n - 1)}>
                  Prev
                </button>
                <span className="text-dim">
                  Page {page + 1} of {pageCount}
                </span>
                <button
                  className="btn"
                  disabled={page >= pageCount - 1}
                  onClick={() => setPage((n) => n + 1)}
                >
                  Next
                </button>
              </div>
            )}
            <div className="adjust-actions">
              <button className="btn-primary" onClick={() => setShowAll(false)}>
                Close
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function NotifRow({ p, unread, full }: { p: Position; unread: boolean; full?: boolean }) {
  return (
    <li className={'notif-item' + (unread ? ' unread' : '')}>
      <div className="notif-item-head">
        <span className={'badge ' + (p.Mode === 'real' ? 'badge-red' : 'badge-dim')}>{p.Mode}</span>
        <strong>
          #{p.ID} {tokenSymbol(p.InstID)}
        </strong>
        {p.Status === 'closing' && <span className="badge badge-red">close failed</span>}
        <span className="text-dim notif-time">{formatDateTime(p.LastErrorAt)}</span>
      </div>
      <div className={'notif-item-msg mono' + (full ? '' : ' clamp')}>{p.LastError}</div>
      {full && p.Status === 'closing' && (
        <div className="text-dim notif-item-hint">
          This position may still be open on the exchange — check OKX directly.
        </div>
      )}
    </li>
  )
}

function BellIcon() {
  return (
    <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
      <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M13.7 21a2 2 0 0 1-3.4 0" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}
