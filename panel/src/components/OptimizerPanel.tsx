import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import ParamChangeModal from './ParamChangeModal'
import { formatUsd, pnlClass, tokenSymbol, winRate } from '../utils/format'
import type { OptimizerBacktestCapital, OptimizerCandidate, OptimizerValidationConfig, PositionMode, StrategyStats } from '../api/types'

const PAGE_SIZE = 25

// Panel-editable promotion thresholds for one risk profile.
function ValidationConfigForm({ riskProfile }: { riskProfile: string }) {
  const [cfg, setCfg] = useState<OptimizerValidationConfig | null>(null)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    api
      .optimizerConfig(riskProfile)
      .then(setCfg)
      .catch((err) => setError((err as Error).message))
  }, [riskProfile])

  if (error) return <div className="error-banner">{error}</div>
  if (!cfg) return <div className="text-dim">Loading…</div>

  function set<K extends keyof OptimizerValidationConfig>(key: K, value: OptimizerValidationConfig[K]) {
    setCfg((prev) => (prev ? { ...prev, [key]: value } : prev))
    setSaved(false)
  }

  async function save() {
    if (!cfg) return
    setSaving(true)
    try {
      await api.saveOptimizerConfig(cfg)
      setSaved(true)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="config-grid" style={{ marginTop: '0.5rem' }}>
      <label className="config-tile">
        Min trades
        <input type="number" value={cfg.minTrades} onChange={(e) => set('minTrades', Number(e.target.value))} />
      </label>
      <label className="config-tile">
        Min win rate %
        <input type="text" value={cfg.minWinRatePct} onChange={(e) => set('minWinRatePct', e.target.value)} />
      </label>
      <label className="config-tile">
        Min realized PnL ($)
        <input type="text" value={cfg.minRealizedPnL} onChange={(e) => set('minRealizedPnL', e.target.value)} />
      </label>
      <label className="config-tile">
        Min significance |t|
        <input type="text" value={cfg.minSignificanceT} onChange={(e) => set('minSignificanceT', e.target.value)} />
      </label>
      <label className="config-tile">
        Max account resets
        <input type="number" value={cfg.maxResets} onChange={(e) => set('maxResets', Number(e.target.value))} />
      </label>
      <label className="config-tile">
        Backtest lookback
        <input
          type="text"
          placeholder="30d"
          value={cfg.backtestLookback}
          onChange={(e) => set('backtestLookback', e.target.value)}
        />
      </label>
      <div style={{ display: 'flex', alignItems: 'flex-end', gap: '0.5rem' }}>
        <button className="btn-primary" onClick={save} disabled={saving}>
          {saving ? 'Saving…' : 'Save'}
        </button>
        {saved && <span className="text-dim">Saved</span>}
      </div>
    </div>
  )
}

type Tab = 'active' | 'rejected'

// The Active tab has two independent stat blocks per row (the backtest that got it promoted, and
// its live track record since) — each sortable on its own, since "best at promotion time" and
// "best right now" can disagree once a candidate has actually been trading for a while.
type ActiveSortKey = 'backtestWinRate' | 'backtestPnl' | 'liveWinRate' | 'livePnl' | 'liveSignals'

function activeSortValue(row: { candidate: OptimizerCandidate; liveStats?: StrategyStats }, key: ActiveSortKey): number {
  const { candidate, liveStats } = row
  switch (key) {
    case 'backtestWinRate':
      return candidate.backtestWinRatePct ? Number(candidate.backtestWinRatePct) : -Infinity
    case 'backtestPnl':
      return candidate.backtestRealizedPnl ? Number(candidate.backtestRealizedPnl) : -Infinity
    case 'liveWinRate': {
      const wins = liveStats?.Wins ?? 0
      const losses = liveStats?.Losses ?? 0
      return wins + losses > 0 ? (wins / (wins + losses)) * 100 : -Infinity
    }
    case 'livePnl':
      return liveStats ? Number(liveStats.RealizedPnL) : -Infinity
    case 'liveSignals':
      return liveStats?.SignalCount ?? -Infinity
  }
}

// Candidates that actually traded in the backtest and were later superseded by a better one
// (status='paper_replaced') — a strictly more useful "rejected" view than one dominated by
// candidates that never even opened a trade (operator's explicit request, 2026-09-28): this is
// the only "rejected" signal that reflects a real, executed track record.
type RejectedSortKey = 'winRate' | 'pnl' | 'trades'

function rejectedSortValue(c: OptimizerCandidate, key: RejectedSortKey): number {
  switch (key) {
    case 'winRate':
      return c.backtestWinRatePct ? Number(c.backtestWinRatePct) : -Infinity
    case 'pnl':
      return c.backtestRealizedPnl ? Number(c.backtestRealizedPnl) : -Infinity
    case 'trades':
      return c.backtestTradeCount ?? -Infinity
  }
}

// Days spanned by the historical candles the backtest actually replayed — "over how many days was
// this PnL earned", not when the backtest itself ran (operator's explicit request, 2026-09-28).
function tradingPeriod(from: string | undefined, to: string | undefined): string {
  if (!from || !to) return '—'
  const days = (new Date(to).getTime() - new Date(from).getTime()) / 86_400_000
  if (!Number.isFinite(days) || days < 0) return '—'
  return days < 1 ? '<1d' : `${Math.round(days)}d`
}

// A % return next to a $ PnL figure, against the capital that specific backtest actually used —
// answers "with how much capital was this profit made" (operator's explicit question), since a
// bare dollar figure means nothing without the size of the position it came from.
function PnLCell({ pnl, capitalUsd, className = '' }: { pnl: string | undefined; capitalUsd: number | null; className?: string }) {
  if (!pnl) return <td className={'mono ' + className}>—</td>
  const n = Number(pnl)
  const pct = capitalUsd && capitalUsd > 0 ? (n / capitalUsd) * 100 : null
  return (
    <td className={'mono ' + className + ' ' + pnlClass(n)}>
      {formatUsd(n)}
      {pct !== null && <span className="text-dim"> ({pct >= 0 ? '+' : ''}{pct.toFixed(1)}%)</span>}
    </td>
  )
}

function SortHeader<K extends string>({
  label,
  sortKey,
  active,
  dir,
  onClick,
  className = '',
}: {
  label: string
  sortKey: K
  active: K
  dir: 'asc' | 'desc'
  onClick: (key: K) => void
  className?: string
}) {
  const isActive = active === sortKey
  return (
    <th className={'th-static ' + className} style={{ cursor: 'pointer', userSelect: 'none' }} onClick={() => onClick(sortKey)}>
      {label}
      {isActive ? (dir === 'desc' ? ' ▼' : ' ▲') : ''}
    </th>
  )
}

function Pagination({ page, totalPages, onChange }: { page: number; totalPages: number; onChange: (p: number) => void }) {
  if (totalPages <= 1) return null
  return (
    <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center', marginTop: '0.6rem' }}>
      <button onClick={() => onChange(0)} disabled={page === 0}>
        «
      </button>
      <button onClick={() => onChange(page - 1)} disabled={page === 0}>
        ‹
      </button>
      <span className="text-dim">
        Page {page + 1} / {totalPages}
      </span>
      <button onClick={() => onChange(page + 1)} disabled={page >= totalPages - 1}>
        ›
      </button>
      <button onClick={() => onChange(totalPages - 1)} disabled={page >= totalPages - 1}>
        »
      </button>
    </div>
  )
}

// A strategy's own displayName, clickable to open its parameter-change history (old tuned values
// vs. new, CLAUDE.md §16.7's timeline reused rather than rebuilt — see ParamChangeModal).
function StrategyNameLink({ candidate, onOpen }: { candidate: OptimizerCandidate; onOpen: () => void }) {
  if (!candidate.strategyId) return <>{candidate.displayName}</>
  return (
    <button
      onClick={onOpen}
      style={{ background: 'none', border: 'none', padding: 0, color: 'var(--accent)', cursor: 'pointer', textDecoration: 'underline', font: 'inherit' }}
    >
      {candidate.displayName}
    </button>
  )
}

// Auto-refresh interval — data-changing background work here is a slow, continuous crawl across
// thousands of lineages (not an instant recompute), so a fast poll would just be wasted requests;
// 30s is enough that the view stops looking frozen without hammering the API.
const REFRESH_MS = 30_000

// Both lists below are inherently bounded by the lineage count (~5,394 total, most producing at
// most one live candidate at a time) — unlike backtest_passed/backtest_rejected, which can run
// into the tens of thousands and were never meant to be paginated whole into the panel.
const CANDIDATE_LIST_LIMIT = 2000

// Shows what the backtest/optimize pipeline is actually doing, in one place (no separate tab):
//   - Active: candidates currently live in paper trading (status='paper_active'). Promotion is
//     automatic the moment a candidate clears validation, so this tab is the actual live roster.
//   - Rejected: candidates that DID trade in their backtest and were later superseded by a better
//     one (status='paper_replaced') — deliberately not the raw backtest_rejected pool, most of
//     which never opened a single trade and told the operator nothing useful (2026-09-28).
// Active is the default tab. Sortable, paginated at 25 rows/page, auto-refreshes every 30s.
export default function OptimizerPanel({ mode }: { mode: PositionMode }) {
  const [tab, setTab] = useState<Tab>('active')
  const [active, setActive] = useState<{ candidate: OptimizerCandidate; liveStats?: StrategyStats }[]>([])
  const [rejected, setRejected] = useState<OptimizerCandidate[]>([])
  const [capital, setCapital] = useState<OptimizerBacktestCapital | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [running, setRunning] = useState(false)
  const [showConfig, setShowConfig] = useState(false)
  const [configProfile, setConfigProfile] = useState<'low' | 'high'>('low')
  const [activeSortKey, setActiveSortKey] = useState<ActiveSortKey>('livePnl')
  const [activeSortDir, setActiveSortDir] = useState<'asc' | 'desc'>('desc')
  const [rejectedSortKey, setRejectedSortKey] = useState<RejectedSortKey>('pnl')
  const [rejectedSortDir, setRejectedSortDir] = useState<'asc' | 'desc'>('desc')
  const [page, setPage] = useState(0)
  const [lastChecked, setLastChecked] = useState<Date | null>(null)
  const [paramModal, setParamModal] = useState<OptimizerCandidate | null>(null)

  async function reload(showSpinner: boolean) {
    if (showSpinner) setLoading(true)
    try {
      const [activeCandidates, rejectedCandidates, cap] = await Promise.all([
        api.optimizerCandidatesByStatus('paper_active', CANDIDATE_LIST_LIMIT),
        api.optimizerCandidatesByStatus('paper_replaced', CANDIDATE_LIST_LIMIT),
        api.optimizerBacktestCapital().catch(() => null),
      ])

      const activeRows = await Promise.all(
        activeCandidates.map(async (candidate) => ({
          candidate,
          liveStats: candidate.strategyId ? await api.strategyStats(candidate.strategyId, mode) : undefined,
        })),
      )

      setActive(activeRows)
      setRejected(rejectedCandidates)
      setCapital(cap)
      setError(null)
      setLastChecked(new Date())
    } catch (err) {
      setError((err as Error).message)
    } finally {
      if (showSpinner) setLoading(false)
    }
  }

  useEffect(() => {
    reload(true)
    const id = setInterval(() => reload(false), REFRESH_MS)
    return () => clearInterval(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode])

  useEffect(() => {
    setPage(0)
  }, [tab, activeSortKey, activeSortDir, rejectedSortKey, rejectedSortDir])

  async function runNow() {
    setRunning(true)
    try {
      await api.runOptimizerNow()
      await reload(true)
    } finally {
      setRunning(false)
    }
  }

  function toggleActiveSort(key: ActiveSortKey) {
    if (activeSortKey === key) {
      setActiveSortDir((d) => (d === 'desc' ? 'asc' : 'desc'))
    } else {
      setActiveSortKey(key)
      setActiveSortDir('desc')
    }
  }

  function toggleRejectedSort(key: RejectedSortKey) {
    if (rejectedSortKey === key) {
      setRejectedSortDir((d) => (d === 'desc' ? 'asc' : 'desc'))
    } else {
      setRejectedSortKey(key)
      setRejectedSortDir('desc')
    }
  }

  const capitalUsd = capital ? Number(capital.positionCapitalUsd) : null

  const sortedActive = useMemo(() => {
    const copy = [...active]
    copy.sort((a, b) =>
      activeSortDir === 'desc'
        ? activeSortValue(b, activeSortKey) - activeSortValue(a, activeSortKey)
        : activeSortValue(a, activeSortKey) - activeSortValue(b, activeSortKey),
    )
    return copy
  }, [active, activeSortKey, activeSortDir])

  const sortedRejected = useMemo(() => {
    const copy = [...rejected]
    copy.sort((a, b) =>
      rejectedSortDir === 'desc'
        ? rejectedSortValue(b, rejectedSortKey) - rejectedSortValue(a, rejectedSortKey)
        : rejectedSortValue(a, rejectedSortKey) - rejectedSortValue(b, rejectedSortKey),
    )
    return copy
  }, [rejected, rejectedSortKey, rejectedSortDir])

  const pageCount = (n: number) => Math.max(1, Math.ceil(n / PAGE_SIZE))
  const slice = <T,>(rows: T[]) => rows.slice(page * PAGE_SIZE, page * PAGE_SIZE + PAGE_SIZE)

  if (error) {
    return (
      <div className="card">
        <h2>Strategy optimizer</h2>
        <div className="error-banner">{error}</div>
      </div>
    )
  }

  return (
    <div className="card">
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <h2>Strategy optimizer</h2>
        <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
          {lastChecked && (
            <span className="text-dim" style={{ fontSize: '0.75rem' }}>
              checked {lastChecked.toLocaleTimeString()}
            </span>
          )}
          <button onClick={() => setShowConfig((v) => !v)}>
            {showConfig ? 'Hide validation config' : 'Validation config'}
          </button>
          <button onClick={runNow} disabled={running}>
            {running ? 'Running…' : 'Run now'}
          </button>
        </div>
      </div>

      {capital && (
        <div className="text-dim" style={{ marginTop: '0.4rem', fontSize: '0.8rem' }}>
          Every backtest below sizes one position at {formatUsd(Number(capital.positionCapitalUsd))} (
          {(Number(capital.maxPositionPct) * 100).toFixed(0)}% of the {formatUsd(Number(capital.initialUsd))} account) — PnL % is against that figure.
        </div>
      )}

      {showConfig && (
        <div style={{ marginTop: '0.5rem' }}>
          <div className="mode-tabs" style={{ marginBottom: '0.4rem' }}>
            {(['low', 'high'] as const).map((p) => (
              <button
                key={p}
                className={'mode-tab' + (configProfile === p ? ' active' : '')}
                onClick={() => setConfigProfile(p)}
              >
                {p === 'low' ? 'Low risk (OKX)' : 'High risk (MEXC)'}
              </button>
            ))}
          </div>
          <ValidationConfigForm riskProfile={configProfile} />
        </div>
      )}

      <div className="mode-tabs" style={{ marginTop: '0.75rem', marginBottom: '0.5rem' }}>
        <button className={'mode-tab' + (tab === 'active' ? ' active' : '')} onClick={() => setTab('active')}>
          Active in paper ({active.length})
        </button>
        <button className={'mode-tab' + (tab === 'rejected' ? ' active' : '')} onClick={() => setTab('rejected')}>
          Rejected, was trading ({rejected.length})
        </button>
      </div>

      {loading && active.length === 0 && rejected.length === 0 && <div className="text-dim">Loading…</div>}

      {tab === 'active' && (
        <>
          {active.length === 0 && !loading && (
            <div className="text-dim">No candidate is currently live in paper trading.</div>
          )}
          {active.length > 0 && (
            <>
              <table>
                <thead>
                  <tr>
                    <th className="th-static">Name</th>
                    <th className="th-static">Token / bar</th>
                    <th className="th-static">Leverage</th>
                    <th className="th-static">Period</th>
                    <th className="th-static col-group-backtest" colSpan={3}>
                      Backtest (at promotion)
                    </th>
                    <th className="th-static col-group-live" colSpan={3}>
                      Live since promotion
                    </th>
                  </tr>
                  <tr>
                    <th className="th-static"></th>
                    <th className="th-static"></th>
                    <th className="th-static"></th>
                    <th className="th-static"></th>
                    <th className="th-static col-cell-backtest">Trades</th>
                    <SortHeader label="Win rate" sortKey="backtestWinRate" active={activeSortKey} dir={activeSortDir} onClick={toggleActiveSort} className="col-cell-backtest" />
                    <SortHeader label="PnL" sortKey="backtestPnl" active={activeSortKey} dir={activeSortDir} onClick={toggleActiveSort} className="col-cell-backtest" />
                    <SortHeader label="Signals" sortKey="liveSignals" active={activeSortKey} dir={activeSortDir} onClick={toggleActiveSort} className="col-cell-live" />
                    <SortHeader label="Win rate" sortKey="liveWinRate" active={activeSortKey} dir={activeSortDir} onClick={toggleActiveSort} className="col-cell-live" />
                    <SortHeader label="PnL" sortKey="livePnl" active={activeSortKey} dir={activeSortDir} onClick={toggleActiveSort} className="col-cell-live" />
                  </tr>
                </thead>
                <tbody>
                  {slice(sortedActive).map(({ candidate, liveStats }) => (
                    <tr key={candidate.id}>
                      <td>
                        <StrategyNameLink candidate={candidate} onOpen={() => setParamModal(candidate)} />
                      </td>
                      <td>
                        {tokenSymbol(candidate.instId)} / {candidate.bar}
                      </td>
                      <td className="mono">{candidate.leverage}x</td>
                      <td className="mono text-dim">{tradingPeriod(candidate.backtestFrom, candidate.backtestTo)}</td>
                      <td className="mono col-cell-backtest">{candidate.backtestTradeCount ?? '—'}</td>
                      <td className="col-cell-backtest">{candidate.backtestWinRatePct ? `${Number(candidate.backtestWinRatePct).toFixed(1)}%` : '—'}</td>
                      <PnLCell pnl={candidate.backtestRealizedPnl} capitalUsd={capitalUsd} className="col-cell-backtest" />
                      <td className="col-cell-live">{liveStats?.SignalCount ?? '—'}</td>
                      <td className="col-cell-live">{winRate(liveStats?.Wins, liveStats?.Losses)}</td>
                      <td className={'mono col-cell-live ' + pnlClass(liveStats ? Number(liveStats.RealizedPnL) : null)}>
                        {liveStats ? formatUsd(Number(liveStats.RealizedPnL)) : '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <Pagination page={page} totalPages={pageCount(active.length)} onChange={setPage} />
            </>
          )}
        </>
      )}

      {tab === 'rejected' && (
        <>
          {rejected.length === 0 && !loading && (
            <div className="text-dim">No candidate that actually traded has been superseded yet.</div>
          )}
          {rejected.length > 0 && (
            <>
              <table>
                <thead>
                  <tr>
                    <th className="th-static">Name</th>
                    <th className="th-static">Token / bar</th>
                    <th className="th-static">Leverage</th>
                    <th className="th-static">Period</th>
                    <SortHeader label="Trades" sortKey="trades" active={rejectedSortKey} dir={rejectedSortDir} onClick={toggleRejectedSort} />
                    <SortHeader label="Win rate" sortKey="winRate" active={rejectedSortKey} dir={rejectedSortDir} onClick={toggleRejectedSort} />
                    <SortHeader label="PnL" sortKey="pnl" active={rejectedSortKey} dir={rejectedSortDir} onClick={toggleRejectedSort} />
                  </tr>
                </thead>
                <tbody>
                  {slice(sortedRejected).map((c) => (
                    <tr key={c.id}>
                      <td>
                        <StrategyNameLink candidate={c} onOpen={() => setParamModal(c)} />
                      </td>
                      <td>
                        {tokenSymbol(c.instId)} / {c.bar}
                      </td>
                      <td className="mono">{c.leverage}x</td>
                      <td className="mono text-dim">{tradingPeriod(c.backtestFrom, c.backtestTo)}</td>
                      <td>{c.backtestTradeCount ?? '—'}</td>
                      <td>{c.backtestWinRatePct ? `${Number(c.backtestWinRatePct).toFixed(1)}%` : '—'}</td>
                      <PnLCell pnl={c.backtestRealizedPnl} capitalUsd={capitalUsd} />
                    </tr>
                  ))}
                </tbody>
              </table>
              <Pagination page={page} totalPages={pageCount(rejected.length)} onChange={setPage} />
            </>
          )}
        </>
      )}

      {paramModal && paramModal.strategyId && (
        <ParamChangeModal
          strategyId={paramModal.strategyId}
          instId={paramModal.instId}
          displayName={paramModal.displayName}
          onClose={() => setParamModal(null)}
        />
      )}
    </div>
  )
}
