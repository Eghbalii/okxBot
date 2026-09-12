import { useEffect, useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import { formatUsd, pnlClass } from '../utils/format'
import type { PositionMode } from '../api/types'
import BalanceChart from './BalanceChart'

// A PnL figure: the percentage leads because it is the comparable number across windows, with the
// dollar amount beneath in a quieter weight rather than parenthesised on the same line — at three
// tiles per row the old single line had to shrink to fit and read as one run-on value.
function PnLTile({ label, usd, pct }: { label: string; usd: string; pct: string }) {
  const usdNum = Number(usd)
  const pctNum = Number(pct)
  return (
    <div className="stat-tile">
      <div className="stat-label">{label}</div>
      <div className={'stat-value mono ' + pnlClass(usdNum)}>
        {Number.isFinite(pctNum) ? `${pctNum >= 0 ? '+' : ''}${pctNum.toFixed(2)}%` : '—'}
      </div>
      <div className="stat-sub mono">{formatUsd(usdNum)}</div>
    </div>
  )
}

// A plain money figure. Same shape as PnLTile so the two rows align on a common baseline rather
// than each tile sizing itself to its own content.
function StatTile({
  label,
  value,
  title,
  dim,
}: {
  label: string
  value: string
  title?: string
  dim?: boolean
}) {
  return (
    <div className="stat-tile">
      <div className="stat-label" title={title}>
        {label}
      </div>
      <div className={'stat-value mono' + (dim ? ' text-dim' : '')}>{value}</div>
    </div>
  )
}

// Trading-cap control, inline in its own tile alongside the other stat tiles: "I've decided to
// trade with $X from now on." What that means depends on the mode, because the two modes disagree
// about what Account Balance IS:
//
//   paper — no exchange exists, so Account Balance is bookkeeping this system owns and a cap moves
//           BOTH numbers to the chosen value together (CLAUDE.md §31.2/§31.3).
//   real  — Account Balance mirrors the exchange's own reported total and must never be
//           overwritten with a chosen number (it is what realized PnL is reconciled against). The
//           cap names the tradable SLICE of it instead; the remainder becomes the Reserve tile,
//           and profit/loss accrues to the slice while the reserve stays put.
//
// The slider's upper bound is the account's REAL Account Balance, not an arbitrary ceiling: a cap
// above what the account actually holds would be claiming a deposit the panel cannot make. The
// direct number input is deliberately kept and left UNBOUNDED — an operator who really has
// deposited more on the exchange needs to be able to say so, and the backend is the authority on
// what is valid, not this control. The slider is the convenience, the input is the escape hatch.
function TradingCapTile({
  mode,
  maxAvailable,
  current,
  onSaved,
}: {
  mode: PositionMode
  maxAvailable: number
  current: number
  onSaved: () => void
}) {
  const [value, setValue] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Seed the control from the account's own current balance so the slider starts somewhere
  // meaningful rather than at zero, and re-seed if the account moves while the field is untouched.
  useEffect(() => {
    if (value === '' && current > 0) setValue(current.toFixed(2))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [current])

  const num = Number(value)
  // The slider can only express [0, maxAvailable]; a typed value above that is still valid (see
  // the note above), it just pins the thumb to the far end rather than rescaling the track.
  const sliderMax = maxAvailable > 0 ? maxAvailable : 100
  const sliderValue = Number.isFinite(num) ? Math.min(Math.max(num, 0), sliderMax) : 0
  const step = sliderMax >= 100 ? 1 : sliderMax >= 10 ? 0.1 : 0.01

  async function save() {
    if (!Number.isFinite(num) || num <= 0) {
      setError('enter a positive number')
      return
    }
    setSaving(true)
    setError(null)
    try {
      await api.setAccountCap(String(num), mode)
      onSaved()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="stat-tile cap-tile">
      <div className="stat-label">Set Trading Cap</div>
      <div className="cap-row">
        <input
          type="range"
          min={0}
          max={sliderMax}
          step={step}
          value={sliderValue}
          onChange={(e) => setValue(e.target.value)}
          className="cap-slider"
          title={`Max available: $${sliderMax.toFixed(2)}`}
        />
        <input
          type="number"
          placeholder="e.g. 40"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          className="cap-input"
        />
        <button className="btn-primary" onClick={save} disabled={saving}>
          {saving ? '…' : 'Set'}
        </button>
      </div>
      <div className="text-dim cap-hint">
        {mode === 'real' ? `of $${sliderMax.toFixed(2)} on the exchange` : `max $${sliderMax.toFixed(2)}`}
      </div>
      {error && <div className="cap-error">{error}</div>}
    </div>
  )
}

// Stats box above the Positions table: open order count, Account Balance, Total Equity, and
// 24h/1w/1month realized PnL, plus the Set Trading Cap control. Polls at a slower interval than
// the position table's own 5s poll — this data doesn't need that freshness.
export default function PaperTradingStatsBox({ mode }: { mode: PositionMode }) {
  const [refreshSignal, setRefreshSignal] = useState(0)
  const { data, error } = usePolling(() => api.paperTradingStats(mode), 15_000, [mode], refreshSignal)

  return (
    <div className="card">
      {error && <div className="error-banner">{error}</div>}
      {!data && !error && <div className="text-dim">Loading…</div>}
      {data && (
        <div className="stats-layout">
          {/* Stats on the left, chart on the right (2026-09-12 redesign). The chart was previously
              full-width beneath the tiles, which made it the largest thing on the page while
              carrying the least-consulted information. Three rows of stats, grouped by what they
              answer: how am I doing, what do I hold, what am I risking. */}
          <div className="stats-col">
            {/* Row 1 — performance. */}
            <div className="stats-row stats-row-3">
              <PnLTile label="24h" usd={data.pnl24hUsd} pct={data.pnl24hPct} />
              <PnLTile label="1W" usd={data.pnl7dUsd} pct={data.pnl7dPct} />
              <PnLTile label="1M" usd={data.pnl30dUsd} pct={data.pnl30dPct} />
            </div>

            {/* Row 2 — capital. Reserve is real-mode only (paper has no exchange balance to hold
                back), so the row is 2-up there and 3-up on real. */}
            <div className={'stats-row ' + (mode === 'real' ? 'stats-row-3' : 'stats-row-2')}>
              <StatTile
                label="Balance"
                title="The real, continuous running total — never reset by a trading-cap change"
                value={formatUsd(Number(data.accountBalanceUsd)).replace('+', '')}
              />
              <StatTile
                label="Equity"
                title={
                  mode === 'real'
                    ? 'The slice of the exchange balance this engine trades with — profit and loss accrue here'
                    : 'The balance since the last chosen trading cap — what new positions size against'
                }
                value={formatUsd(Number(data.totalEquityUsd)).replace('+', '')}
              />
              {mode === 'real' && (
                <StatTile
                  label="Reserve"
                  dim
                  title="Account Balance minus Total Equity — capital held back from trading. Moves only when you change the cap."
                  value={formatUsd(
                    Number(data.accountBalanceUsd) - Number(data.totalEquityUsd),
                  ).replace('+', '')}
                />
              )}
            </div>

            {/* Row 3 — the one control, given its own full-width row so its slider has room. */}
            <div className="stats-row">
              <TradingCapTile
                mode={mode}
                maxAvailable={Number(data.accountBalanceUsd)}
                current={Number(data.totalEquityUsd)}
                onSaved={() => setRefreshSignal((n) => n + 1)}
              />
            </div>
          </div>

          <div className="stats-chart">
            <BalanceChart
              mode={mode}
              currentEquity={Number(data.totalEquityUsd)}
              currentBalance={Number(data.accountBalanceUsd)}
              refreshSignal={refreshSignal}
            />
          </div>
        </div>
      )}
    </div>
  )
}
