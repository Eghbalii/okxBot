import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import { useCachedResource } from '../hooks/useCachedResource'
import type { PaperTradingConfig, PositionMode } from '../api/types'
import StrategyKindModal from './StrategyKindModal'
import TokenModal from './TokenModal'

// The three decision timeframes paper-trading strategies can evaluate on (CLAUDE.md §9) — the
// fixed set, not every timeframe the ingestor collects (which also includes 4H/1D for context
// only, never a decision bar).
const DECISION_BARS = ['5m', '15m', '1H']

const STATE_META: Record<string, { label: string; badge: string; dot: string }> = {
  running: { label: 'Running', badge: 'badge-green', dot: 'var(--green)' },
  paused: { label: 'Paused', badge: 'badge-yellow', dot: 'var(--yellow)' },
  stopped: { label: 'Stopped', badge: 'badge-red', dot: 'var(--red)' },
}

// Trading controls for one mode (Paper or Real) — the page-level tab (PositionsPage) now owns mode
// selection, so this component just renders whichever mode it's given (CLAUDE.md real-trading
// readiness plan, 2026-09-04: both modes are wired up identically, reading/writing the same
// mode-scoped Postgres rows via cmd/api's now mode-aware endpoints).
export default function PaperTradingConfigBox({ mode }: { mode: PositionMode }) {
  return (
    <div className="card config-box">
      <div className="config-box-header">
        <h2>Trading controls</h2>
      </div>
      <TradingControls mode={mode} />
    </div>
  )
}

// TradingHealthBanner surfaces, on the Positions page, anything that stops trading REGARDLESS of the
// configured state beside it (2026-09-13 request).
//
// The gap it closes: the badge next to it renders `tradingState` from the database — the operator's
// configured INTENT. It read a green "Running" throughout §47 (the trader crash-looping for eight
// hours) and §48 (a self-halt that lasted two hours), because neither of those changes that column.
// The one page most likely to be open while wondering "why are no positions opening" was the one
// page that could not say.
//
// It reads the same GET /api/health the Resources page does, so the two can never disagree — a
// second source of truth here would just be a new way for them to drift. Silent when everything is
// fine: a banner shown always is a banner nobody reads.
function TradingHealthBanner({ mode }: { mode: PositionMode }) {
  // Cached and shared with the Resources page's own poll, so opening Positions costs no extra
  // request and shows a known-good answer immediately.
  const { data } = useCachedResource('health', () => api.health(), {
    maxAgeMs: 10_000,
    refetchMs: 15_000,
  })
  if (!data) return null

  const downCritical = data.services.filter((s) => s.critical && s.state !== 'running')
  // A halt applies to real trading only, so it is not reported on the Paper tab where it would be
  // a distraction the operator cannot act on.
  const halt = mode === 'real' ? data.halt : undefined
  const blocked = halt && !halt.safeToReset && (halt.blockers?.length ?? 0) > 0

  if (downCritical.length === 0 && !blocked) return null

  // "restarting" is named explicitly rather than folded into "not running": it means a crash loop,
  // which needs a fix and a rebuild, where a stopped container usually just needs starting.
  const worst = downCritical[0]
  const serviceMsg = worst
    ? `${worst.name} is ${worst.state === 'restarting' ? 'crash-looping' : worst.state}` +
      (downCritical.length > 1 ? ` (and ${downCritical.length - 1} more)` : '')
    : null

  return (
    <div className="trading-health-banner">
      <div>
        <strong>Trading may be blocked.</strong>{' '}
        {serviceMsg && <span>{serviceMsg}. </span>}
        {blocked && <span>{halt?.blockers?.[0]} </span>}
      </div>
      <Link to="/resources" className="banner-link">
        Open Resources →
      </Link>
    </div>
  )
}

function TradingControls({ mode }: { mode: PositionMode }) {
  const [cfg, setCfg] = useState<PaperTradingConfig | null>(null)
  const [disableLong, setDisableLong] = useState(false)
  const [disableShort, setDisableShort] = useState(false)
  const [activeBars, setActiveBars] = useState<Set<string>>(new Set())
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)
  const [stateChanging, setStateChanging] = useState(false)
  const [message, setMessage] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [showStrategyModal, setShowStrategyModal] = useState(false)
  const [showTokenModal, setShowTokenModal] = useState(false)

  function load() {
    api
      .paperTradingConfig(mode)
      .then((c) => {
        setCfg(c)
        setDisableLong(c.disableLong)
        setDisableShort(c.disableShort)
        setActiveBars(new Set(c.activeBars))
        setDirty(false)
      })
      .catch((err) => setError((err as Error).message))
  }

  useEffect(load, [mode])

  function toggleBar(bar: string) {
    setActiveBars((prev) => {
      const next = new Set(prev)
      if (next.has(bar)) next.delete(bar)
      else next.add(bar)
      return next
    })
    setDirty(true)
  }

  // Pause/Stop/Resume are independent of the config form below (per explicit request: they're not
  // config, they take effect immediately) — each is its own request + restart, not bundled into the
  // Save button.
  async function setTradingState(next: 'running' | 'paused' | 'stopped') {
    if (next === 'stopped' && !confirm('Stop trading? This closes every open position now. New positions stay off until you resume.')) {
      return
    }
    setStateChanging(true)
    setError(null)
    try {
      await api.savePaperTradingConfig(mode, { tradingState: next })
      await api.restartPaperTrader(mode)
      setMessage(`${STATE_META[next].label} — applying now, back within a few seconds.`)
      setTimeout(load, 4000)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setStateChanging(false)
    }
  }

  // One combined action: persist the form, then restart immediately so it actually takes effect —
  // there is no live-reload path, so a "Save" that doesn't also restart would silently do nothing
  // until a separate manual step. Long/short/timeframe edits live here since they're config choices
  // reviewed together, unlike the state buttons above which are one-click, no-review actions.
  async function saveAndApply() {
    setSaving(true)
    setError(null)
    try {
      await api.savePaperTradingConfig(mode, {
        disableLong,
        disableShort,
        activeBars: [...activeBars],
      })
      await api.restartPaperTrader(mode)
      setDirty(false)
      setMessage('Saved — applying now, back within a few seconds.')
      setTimeout(load, 4000)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setSaving(false)
    }
  }

  async function saveActiveKinds(kinds: string[]) {
    await api.savePaperTradingConfig(mode, { activeKinds: kinds })
    await api.restartPaperTrader(mode)
    setMessage('Saved — applying now, back within a few seconds.')
    setTimeout(load, 4000)
  }

  async function saveDisabledInstIds(instIds: string[]) {
    await api.savePaperTradingConfig(mode, { disabledInstIds: instIds })
    await api.restartPaperTrader(mode)
    setMessage('Saved — applying now, back within a few seconds.')
    setTimeout(load, 4000)
  }

  if (error && !cfg) return <div className="error-banner">{error}</div>
  if (!cfg) return <div className="text-dim">Loading…</div>

  const state = cfg.tradingState
  const meta = STATE_META[state] ?? STATE_META.running
  const activeKindCount = cfg.activeKinds.length === 0 ? 'all' : cfg.activeKinds.length
  const disabledTokenCount = cfg.disabledInstIds.length

  return (
    <>
      {error && <div className="error-banner">{error}</div>}

      {/* Configured intent and actual health are DIFFERENT facts and are shown as such.
          tradingState is what the operator asked for; it says nothing about whether the service can
          act on it. Before this, the badge read "Running" in green while the trader was
          crash-looping (§47) or had halted itself (§48) — the state most worth noticing was the one
          the page hid. Conflating them into one badge would lose that distinction again, so the
          badge keeps its own meaning and the problem is reported beside it. */}
      <TradingHealthBanner mode={mode} />

      <div className="state-row">
        <span className={'badge ' + meta.badge}>
          <span className="state-dot" style={{ background: meta.dot }} />
          {meta.label}
        </span>
        <div className="state-actions">
          <button
            className={state === 'running' ? '' : 'btn-primary'}
            onClick={() => setTradingState('running')}
            disabled={stateChanging || state === 'running'}
            title="Resume opening new positions"
          >
            Resume
          </button>
          <button
            onClick={() => setTradingState('paused')}
            disabled={stateChanging || state === 'paused' || state === 'stopped'}
            title="No new positions; existing ones keep running"
          >
            Pause
          </button>
          <button
            className="btn-danger"
            onClick={() => setTradingState('stopped')}
            disabled={stateChanging || state === 'stopped'}
            title="Close every open position now, and stop opening new ones"
          >
            Stop
          </button>
        </div>
      </div>

      <div className="config-grid">
        <div className="config-tile">
          <div className="config-tile-label">Signal direction</div>
          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={disableLong}
              onChange={(e) => {
                setDisableLong(e.target.checked)
                setDirty(true)
              }}
            />
            Disable long signals
          </label>
          <label className="checkbox-row">
            <input
              type="checkbox"
              checked={disableShort}
              onChange={(e) => {
                setDisableShort(e.target.checked)
                setDirty(true)
              }}
            />
            Disable short signals
          </label>
        </div>

        <div className="config-tile">
          <div className="config-tile-label">Active timeframes</div>
          <div className="checkbox-row-inline">
            {DECISION_BARS.map((bar) => (
              <label key={bar} className="checkbox-row">
                <input type="checkbox" checked={activeBars.has(bar)} onChange={() => toggleBar(bar)} />
                <span className="mono">{bar}</span>
              </label>
            ))}
          </div>
          <div className="text-dim" style={{ fontSize: '0.75rem' }}>
            None checked = use config.yaml's default bars
          </div>
        </div>

        <div className="config-tile">
          <div className="config-tile-label">Strategies</div>
          <div className="config-tile-value">{activeKindCount} active</div>
          <button onClick={() => setShowStrategyModal(true)}>Manage strategies…</button>
        </div>

        <div className="config-tile">
          <div className="config-tile-label">Tokens</div>
          <div className="config-tile-value">
            {disabledTokenCount === 0 ? 'all active' : `${disabledTokenCount} disabled`}
          </div>
          <button onClick={() => setShowTokenModal(true)}>Manage tokens…</button>
        </div>
      </div>

      <div className="toolbar" style={{ marginTop: '0.9rem' }}>
        <button className="btn-primary" onClick={saveAndApply} disabled={!dirty || saving}>
          {saving ? 'Applying…' : 'Save & Apply'}
        </button>
        {message && <span className="text-dim">{message}</span>}
      </div>

      {showStrategyModal && (
        <StrategyKindModal
          mode={mode}
          activeKinds={cfg.activeKinds}
          onClose={() => setShowStrategyModal(false)}
          onSave={saveActiveKinds}
        />
      )}
      {showTokenModal && (
        <TokenModal
          mode={mode}
          disabledInstIds={cfg.disabledInstIds}
          onClose={() => setShowTokenModal(false)}
          onSave={saveDisabledInstIds}
        />
      )}
    </>
  )
}
