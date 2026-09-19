import { useState } from 'react'
import { usePolling } from '../hooks/usePolling'
import { api } from '../api/client'
import type { HealthResponse, ServiceHealth } from '../api/types'

// Service health, built after two outages that looked identical from this panel and had opposite
// fixes (CLAUDE.md §47, §48):
//
//   §47 the container was CRASH-LOOPING  -> needs a code fix and a rebuild
//   §48 the container was UP and HALTED  -> needs a reset
//
// Both presented as "bot trading isn't working". So this shows the distinction directly instead of
// leaving it to be inferred from a failed request.

// Colour and label per Docker state. "restarting" is called out explicitly rather than folded into
// a generic error colour — it is the crash-loop signal and the single most useful thing on the page
// when something is wrong.
function stateLook(state: string): { cls: string; label: string } {
  switch (state) {
    case 'running':
      return { cls: 'ok', label: 'running' }
    case 'restarting':
      return { cls: 'warn', label: 'restarting (crash loop)' }
    case 'exited':
      return { cls: 'bad', label: 'stopped' }
    case 'missing':
      return { cls: 'bad', label: 'not found' }
    case '':
      return { cls: 'unknown', label: 'unknown' }
    default:
      return { cls: 'warn', label: state }
  }
}

function ServiceRow({ s }: { s: ServiceHealth }) {
  const look = stateLook(s.state)
  return (
    <tr>
      <td>
        {s.name}
        {s.critical && <span className="badge badge-dim" style={{ marginLeft: 6 }}>critical</span>}
      </td>
      <td>
        <span className={`state-dot state-${look.cls}`} /> {look.label}
      </td>
      <td className="mono dim">{s.status || '—'}</td>
    </tr>
  )
}

// HaltPanel is the part that answers "is there a problem, why, and is it safe to reset".
//
// The reset button is disabled unless the backend says the condition has actually cleared — the
// operator's own requirement (2026-09-13): understand first, reset only if safe. A confirmation
// dialog would not achieve that, since it asks the operator to guess at exactly the moment they
// have least information.
function HaltPanel({ health, onChanged }: { health: HealthResponse; onChanged: () => void }) {
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<string | null>(null)
  const halt = health.halt

  if (!halt) return null

  async function reset() {
    setBusy(true)
    setMsg(null)
    try {
      await api.resetHalt()
      setMsg('Trader is restarting — its halt clears on startup.')
      onChanged()
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  // The evidence is shown whether or not anything is wrong: "exchange 0 / local 0" is how an
  // operator confirms the system agrees with reality, which is worth seeing before trusting it.
  const evidence = `exchange ${halt.exchangePositions} position(s) · local ${halt.localOpenOrders} open row(s)`

  return (
    <div className="halt-panel">
      <div className="halt-head">
        <strong>Bot Trader</strong>
        {halt.safeToReset && halt.exchangePositions === halt.localOpenOrders ? (
          <span className="badge badge-green">state agrees with exchange</span>
        ) : (
          <span className="badge badge-red">drift detected</span>
        )}
      </div>

      <div className="dim" style={{ marginTop: 4 }}>{evidence}</div>

      {halt.blockers && halt.blockers.length > 0 && (
        <ul className="halt-blockers">
          {halt.blockers.map((b, i) => (
            <li key={i}>{b}</li>
          ))}
        </ul>
      )}

      <div style={{ marginTop: 10 }}>
        <button className="btn-danger" onClick={reset} disabled={busy || !halt.safeToReset}>
          {busy ? 'Restarting…' : 'Clear halt & restart trader'}
        </button>
        {!halt.safeToReset && (
          <span className="dim" style={{ marginLeft: 8 }}>
            Resolve the issue above before resetting.
          </span>
        )}
      </div>

      {msg && <div className="dim" style={{ marginTop: 8 }}>{msg}</div>}
    </div>
  )
}

export default function ServiceHealthBox() {
  // refreshSignal forces an immediate refetch after a reset, rather than waiting out the poll —
  // usePolling has no imperative refetch, it re-runs when this value changes.
  const [refresh, setRefresh] = useState(0)
  const { data } = usePolling<HealthResponse>(() => api.health(), 10000, [], refresh)

  if (!data) return <div className="config-box"><div className="dim">Loading service health…</div></div>

  const critical = data.services.filter((s) => s.critical)
  const other = data.services.filter((s) => !s.critical)
  const unhealthy = critical.filter((s) => s.state !== 'running')

  return (
    <div className="config-box">
      <div className="config-box-header">
        <strong>Service health</strong>
        {unhealthy.length === 0 ? (
          <span className="badge badge-green">all critical services running</span>
        ) : (
          <span className="badge badge-red">
            {unhealthy.length} critical service{unhealthy.length > 1 ? 's' : ''} not running
          </span>
        )}
      </div>

      {data.dockerError && (
        <div className="halt-blockers" style={{ marginTop: 8 }}>
          Cannot read container states: {data.dockerError}
        </div>
      )}

      <HaltPanel health={data} onChanged={() => setRefresh((n) => n + 1)} />

      <table className="health-table">
        <tbody>
          {critical.map((s) => (
            <ServiceRow key={s.container} s={s} />
          ))}
          {other.map((s) => (
            <ServiceRow key={s.container} s={s} />
          ))}
        </tbody>
      </table>
    </div>
  )
}
