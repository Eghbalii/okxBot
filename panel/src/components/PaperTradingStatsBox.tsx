import { useEffect, useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import { formatUsd, pnlClass } from '../utils/format'
import type { PositionMode } from '../api/types'
import BalanceChart from './BalanceChart'

function PnLTile({ label, usd, pct }: { label: string; usd: string; pct: string }) {
  const usdNum = Number(usd)
  const pctNum = Number(pct)
  return (
    <div className="config-tile">
      <div className="config-tile-label">{label}</div>
      <div className={'config-tile-value mono ' + pnlClass(usdNum)}>
        {Number.isFinite(pctNum) ? `${pctNum >= 0 ? '+' : ''}${pctNum.toFixed(2)}%` : '—'}{' '}
        <span className="text-dim" style={{ fontWeight: 400, fontSize: '0.8rem' }}>
          ({formatUsd(usdNum)})
        </span>
      </div>
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
    <div className="config-tile">
      <div className="config-tile-label">Set Trading Cap</div>
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
        <div className="config-grid">
          <div className="config-tile">
            <div className="config-tile-label">Open Orders</div>
            <div className="config-tile-value mono">{data.openCount}</div>
          </div>
          <div className="config-tile">
            <div className="config-tile-label" title="The real, continuous running total — never reset by a trading-cap change">
              Account Balance
            </div>
            <div className="config-tile-value mono">
              {formatUsd(Number(data.accountBalanceUsd)).replace('+', '')}
            </div>
          </div>
          <div className="config-tile">
            <div
              className="config-tile-label"
              title={
                mode === 'real'
                  ? 'The slice of the exchange balance this engine trades with — profit and loss accrue here'
                  : 'The balance since the last chosen trading cap — what new positions size against'
              }
            >
              Total Equity
            </div>
            <div className="config-tile-value mono">
              {formatUsd(Number(data.totalEquityUsd)).replace('+', '')}
            </div>
          </div>
          {/* Real mode only: the untraded remainder, shown so the split between "what the exchange
              holds" and "what I'm risking" is visible at a glance rather than mental arithmetic.
              Paper has no exchange balance to split, so the tile would always read $0.00. */}
          {mode === 'real' && (
            <div className="config-tile">
              <div
                className="config-tile-label"
                title="Account Balance minus Total Equity — capital held back from trading. Moves only when you change the cap."
              >
                Reserve
              </div>
              <div className="config-tile-value mono text-dim">
                {formatUsd(Number(data.accountBalanceUsd) - Number(data.totalEquityUsd)).replace('+', '')}
              </div>
            </div>
          )}
          <PnLTile label="24h PnL" usd={data.pnl24hUsd} pct={data.pnl24hPct} />
          <PnLTile label="1W PnL" usd={data.pnl7dUsd} pct={data.pnl7dPct} />
          <PnLTile label="1M PnL" usd={data.pnl30dUsd} pct={data.pnl30dPct} />
          <TradingCapTile
            mode={mode}
            maxAvailable={Number(data.accountBalanceUsd)}
            current={Number(data.totalEquityUsd)}
            onSaved={() => setRefreshSignal((n) => n + 1)}
          />
        </div>
      )}
      {data && (
        <BalanceChart
          mode={mode}
          currentEquity={Number(data.totalEquityUsd)}
          currentBalance={Number(data.accountBalanceUsd)}
          refreshSignal={refreshSignal}
        />
      )}
    </div>
  )
}
