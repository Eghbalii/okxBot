import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { PaperTradingConfig, TradingState } from '../api/types'
import StrategyKindModal from './StrategyKindModal'
import TokenModal from './TokenModal'

// The three decision timeframes paper-trading strategies can evaluate on (CLAUDE.md §9) — the
// fixed set, not every timeframe the ingestor collects (which also includes 4H/1D for context
// only, never a decision bar).
const DECISION_BARS = ['5m', '15m', '1H']

// Control box above the Positions table (2026-09-01 request): pause/stop trading, disable one
// signal direction, and manage active strategies/tokens/timeframes for paper trading. Every
// control here is "edit + restart" (CLAUDE.md, matching cmd/strategy-tester's own config pattern)
// — no live-reload, so a save always shows a "restart required" banner rather than pretending the
// change is already in effect.
export default function PaperTradingConfigBox() {
  const [cfg, setCfg] = useState<PaperTradingConfig | null>(null)
  const [tradingState, setTradingState] = useState<TradingState>('running')
  const [disableLong, setDisableLong] = useState(false)
  const [disableShort, setDisableShort] = useState(false)
  const [activeBars, setActiveBars] = useState<Set<string>>(new Set())
  const [dirty, setDirty] = useState(false)
  const [saving, setSaving] = useState(false)
  const [restarting, setRestarting] = useState(false)
  const [message, setMessage] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [showStrategyModal, setShowStrategyModal] = useState(false)
  const [showTokenModal, setShowTokenModal] = useState(false)

  function load() {
    api
      .paperTradingConfig()
      .then((c) => {
        setCfg(c)
        setTradingState(c.tradingState)
        setDisableLong(c.disableLong)
        setDisableShort(c.disableShort)
        setActiveBars(new Set(c.activeBars))
        setDirty(false)
      })
      .catch((err) => setError((err as Error).message))
  }

  useEffect(load, [])

  function toggleBar(bar: string) {
    setActiveBars((prev) => {
      const next = new Set(prev)
      if (next.has(bar)) next.delete(bar)
      else next.add(bar)
      return next
    })
    setDirty(true)
  }

  async function save() {
    if (tradingState === 'stopped') {
      if (!confirm('Setting state to Stopped will close every open paper position now. Continue?')) return
    }
    setSaving(true)
    setError(null)
    try {
      await api.savePaperTradingConfig({
        tradingState,
        disableLong,
        disableShort,
        activeBars: [...activeBars],
      })
      setDirty(false)
      setMessage('Saved. Restart paper-trader for the changes to take effect.')
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setSaving(false)
    }
  }

  async function restart() {
    if (!confirm('Restart the paper-trader service now? No other service is affected.')) return
    setRestarting(true)
    try {
      await api.restartPaperTrader()
      setMessage('Restart requested — the service will be back within a few seconds.')
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setRestarting(false)
    }
  }

  async function saveActiveKinds(kinds: string[]) {
    await api.savePaperTradingConfig({ activeKinds: kinds })
    setMessage('Saved. Restart paper-trader for the changes to take effect.')
    load()
  }

  async function saveDisabledInstIds(instIds: string[]) {
    await api.savePaperTradingConfig({ disabledInstIds: instIds })
    setMessage('Saved. Restart paper-trader for the changes to take effect.')
    load()
  }

  return (
    <div className="card">
      <h2>Paper trading controls</h2>
      {error && <div className="error-banner">{error}</div>}
      {!cfg && !error && <div className="text-dim">Loading…</div>}
      {cfg && (
        <>
          <div className="toolbar">
            <label className="text-dim" style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
              State
              <select
                value={tradingState}
                onChange={(e) => {
                  setTradingState(e.target.value as TradingState)
                  setDirty(true)
                }}
              >
                <option value="running">Running</option>
                <option value="paused">Paused (no new opens)</option>
                <option value="stopped">Stopped (closes everything)</option>
              </select>
            </label>
            <label style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
              <input
                type="checkbox"
                checked={disableLong}
                onChange={(e) => {
                  setDisableLong(e.target.checked)
                  setDirty(true)
                }}
              />
              <span className="text-dim">Disable long signals</span>
            </label>
            <label style={{ display: 'flex', alignItems: 'center', gap: '0.35rem' }}>
              <input
                type="checkbox"
                checked={disableShort}
                onChange={(e) => {
                  setDisableShort(e.target.checked)
                  setDirty(true)
                }}
              />
              <span className="text-dim">Disable short signals</span>
            </label>
          </div>

          <div className="toolbar" style={{ marginTop: '0.5rem' }}>
            <span className="text-dim">Active timeframes:</span>
            {DECISION_BARS.map((bar) => (
              <label key={bar} style={{ display: 'flex', alignItems: 'center', gap: '0.3rem' }}>
                <input type="checkbox" checked={activeBars.has(bar)} onChange={() => toggleBar(bar)} />
                <span className="mono">{bar}</span>
              </label>
            ))}
            <span className="text-dim" style={{ fontSize: '0.8rem' }}>
              (none checked = use config.yaml's paper_trading.bars as-is)
            </span>
          </div>

          <div className="toolbar" style={{ marginTop: '0.5rem' }}>
            <button onClick={() => setShowStrategyModal(true)}>Manage strategies…</button>
            <button onClick={() => setShowTokenModal(true)}>Manage tokens…</button>
          </div>

          <div className="toolbar" style={{ marginTop: '0.75rem' }}>
            <button onClick={save} disabled={!dirty || saving}>
              {saving ? 'Saving…' : 'Save'}
            </button>
            <button onClick={restart} disabled={restarting}>
              {restarting ? 'Restarting…' : 'Restart service'}
            </button>
          </div>

          {message && (
            <div className="text-dim" style={{ marginTop: '0.5rem' }}>
              {message}
            </div>
          )}
        </>
      )}

      {showStrategyModal && (
        <StrategyKindModal
          activeKinds={cfg?.activeKinds ?? []}
          onClose={() => setShowStrategyModal(false)}
          onSave={saveActiveKinds}
        />
      )}
      {showTokenModal && (
        <TokenModal
          allInstIds={cfg?.allInstIds ?? []}
          disabledInstIds={cfg?.disabledInstIds ?? []}
          onClose={() => setShowTokenModal(false)}
          onSave={saveDisabledInstIds}
        />
      )}
    </div>
  )
}
