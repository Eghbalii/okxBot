import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { ExchangeOrderRaw, PaperOrderAdjustment, Position, PositionMode } from '../api/types'
import { formatDateTime, formatUsd, pnlClass, tokenSymbol, trimPrice } from '../utils/format'

// The decision-time observation persisted on the order (CLAUDE.md §15.3) — what the model was
// ASKED. Only the fields this view compares are typed; the rest of the payload is ignored.
interface DecisionSignal {
  kind?: string
  bar?: string
  side?: string
  entry_px?: string
  sl_px?: string
  tp_px?: string
  confidence?: string
  win_rate?: string
  trade_count?: number
  strategy_id?: number
}

interface DecisionObservation {
  category?: string
  signal?: DecisionSignal | null
  schema_version?: number
}

function parseObservation(raw: unknown): DecisionObservation | null {
  if (!raw) return null
  // Go marshals FeaturesJSON as embedded JSON, so it arrives already parsed. A string is still
  // handled in case an older row stored it double-encoded.
  if (typeof raw === 'string') {
    try {
      return JSON.parse(raw) as DecisionObservation
    } catch {
      return null
    }
  }
  if (typeof raw === 'object') return raw as DecisionObservation
  return null
}

function fmt(v: string | number | null | undefined): string {
  if (v === null || v === undefined || v === '') return '—'
  return String(v)
}

// Like fmt, but for price/notional-shaped fields (entry/SL/TP/size) — trimmed for readability the
// same way the Positions table's own prices are (2026-09-04 request), rather than showing raw
// full-precision NUMERIC values.
function fmtPrice(v: string | number | null | undefined): string {
  if (v === null || v === undefined || v === '') return '—'
  return trimPrice(v)
}

// Distance of a SL/TP level from entry, as a signed percent — positive means "in the direction of
// profit for this position's side," negative means "toward loss," regardless of which raw
// direction the price itself moves. A short's stop sits ABOVE entry (a price rise is a loss for a
// short) and its target sits BELOW entry (a price fall is the profit) — the opposite of a long —
// so the raw (level-entry)/entry distance has to be flipped for a sell, or a short's take-profit
// (a genuinely profitable level) reads as negative and its stop-loss reads as positive, exactly
// backwards from what the sign is supposed to communicate.
function pctFromEntry(level: string | number | null | undefined, entryPx: string, side: string): string {
  const entry = Number(entryPx)
  const lvl = Number(level)
  if (!entry || level === null || level === undefined || level === '' || Number.isNaN(lvl)) return ''
  const direction = side === 'sell' ? -1 : 1
  const pct = ((lvl - entry) / entry) * 100 * direction
  return ` (${pct >= 0 ? '+' : ''}${pct.toFixed(2)}%)`
}

// A row where the two sides differ is the interesting case: it means the model actually moved the
// level the strategy proposed. Equal values mean the strategy's own number survived untouched.
function Row({ label, strategy, model }: { label: string; strategy: string; model: string }) {
  const differs = strategy !== '—' && model !== '—' && strategy !== model
  return (
    <tr>
      <td className="text-dim">{label}</td>
      <td className="mono">{strategy}</td>
      <td className="mono" style={differs ? { color: 'var(--accent)' } : undefined}>
        {model}
      </td>
    </tr>
  )
}

// Chronological table of every in-trade SL/TP move made on this order (CLAUDE.md §15.4/§15.12
// revision, 2026-09-02) — replaces the old baseline-vs-rl_adjusted A/B comparison now that the RL
// mechanic edits the order in place instead of forking it.
function AdjustmentHistory({ orderId, mode }: { orderId: number; mode: PositionMode }) {
  const [adjustments, setAdjustments] = useState<PaperOrderAdjustment[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    api
      .paperOrderAdjustments(orderId, mode)
      .then((rows) => {
        if (!cancelled) setAdjustments(rows)
      })
      .catch((err) => {
        if (!cancelled) setError((err as Error).message)
      })
    return () => {
      cancelled = true
    }
  }, [orderId, mode])

  if (error) return <div className="error-banner">Failed to load adjustment history: {error}</div>
  if (adjustments === null) return <div className="text-dim">Loading adjustment history…</div>
  if (adjustments.length === 0) {
    return <div className="text-dim">No in-trade SL/TP adjustments have been made on this order.</div>
  }

  return (
    <table>
      <thead>
        <tr>
          <th>When</th>
          <th>Field</th>
          <th>Old → new</th>
          <th>Source</th>
        </tr>
      </thead>
      <tbody>
        {adjustments.map((a) => (
          <tr key={a.ID}>
            <td className="mono">{new Date(a.CreatedAt).toLocaleString()}</td>
            <td>{a.Field.toUpperCase()}</td>
            <td className="mono">
              {fmtPrice(a.OldValue)} → {fmtPrice(a.NewValue)}
            </td>
            <td>{a.Source}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

export default function OrderDetailModal({
  position,
  onClose,
}: {
  position: Position
  onClose: () => void
}) {
  const obs = parseObservation(position.FeaturesJSON)
  const sig = obs?.signal ?? null

  // Whether the model actually shaped this order. rl_sizing opens at the configured fixed notional
  // and 1x leverage when the model declines or returns an unusable answer (CLAUDE.md §15.4), so a
  // 1x order is the tell that the strategy's own sizing stood.
  const modelSized = position.Leverage !== '1' && position.Leverage !== '1.0'

// OKX's own record for both legs, rendered as raw JSON (2026-09-09 request: "show me the whole
// JSON, not a few parameters you picked"). Fetched on demand rather than with the modal, since it
// costs two live exchange calls against a shared rate-limit budget and most views of an order do
// not need it.
//
// Deliberately unformatted beyond indentation: the value here is that nothing was interpreted or
// dropped on the way through, so picking fields out to display would defeat the purpose.
function ExchangeRawJSON({ orderId }: { orderId: number }) {
  const [data, setData] = useState<ExchangeOrderRaw | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  function load() {
    setLoading(true)
    setError(null)
    api
      .exchangeOrderRaw(orderId)
      .then(setData)
      .catch((err) => setError((err as Error).message))
      .finally(() => setLoading(false))
  }

  if (!data && !loading && !error) {
    return (
      <button onClick={load} className="raw-json-load">
        Load full record from the exchange
      </button>
    )
  }
  if (loading) return <div className="text-dim">Fetching from OKX…</div>
  if (error) return <div className="error-banner">{error}</div>
  if (!data) return null

  return (
    <div className="raw-json-wrap">
      <RawLeg label="Open order" id={data.openOrderId} body={data.open} error={data.openError} />
      <RawLeg label="Close order" id={data.closeOrderId} body={data.close} error={data.closeError} />
      <button onClick={load} className="raw-json-load">
        Refresh
      </button>
    </div>
  )
}

function RawLeg({
  label,
  id,
  body,
  error,
}: {
  label: string
  id?: string
  body: Record<string, unknown> | null
  error?: string
}) {
  // No id recorded at all is the normal case for a still-open position's close leg — say so
  // plainly rather than rendering an empty box that reads as a failure.
  if (!id) {
    return (
      <div className="raw-json-leg">
        <div className="raw-json-label">{label}</div>
        <div className="text-dim">No exchange order id recorded.</div>
      </div>
    )
  }
  return (
    <div className="raw-json-leg">
      <div className="raw-json-label">
        {label} <span className="text-dim mono">{id}</span>
      </div>
      {error ? (
        <div className="error-banner">{error}</div>
      ) : (
        <pre className="raw-json mono">{JSON.stringify(body, null, 2)}</pre>
      )}
    </div>
  )
}

// What the EXCHANGE reported about this order, as distinct from what this system decided or
// computed (2026-09-09 request). Everything above it in the modal is decision-time data — the
// strategy's signal, the model's answer, the levels we chose. This block is the other half: what
// OKX actually did with it.
//
// The distinction matters because the two can legitimately disagree, and only seeing them side by
// side makes that visible. Real order 3's locally computed PnL read +$0.363 while the account
// actually moved +$0.00126 — the local formula treated a USD notional as a position size, ~288x
// off. Any figure OKX reported is therefore shown as authoritative and the locally computed one is
// shown beside it, rather than the panel silently picking one.
//
// Real orders only: paper trading has no exchange leg, so every field here would be empty.
function ExchangeDetails({ position }: { position: Position }) {
  if (position.Mode !== 'real') return null

  const localPnL = position.RealizedPnL !== null ? Number(position.RealizedPnL) : null
  const exPnL = position.ExchangeRealizedPnL !== null ? Number(position.ExchangeRealizedPnL) : null
  // Only worth flagging once both numbers exist and actually differ beyond rounding — a gap here
  // means the local model of fees or fill price is wrong, which is worth seeing rather than hiding.
  const pnlDisagrees =
    localPnL !== null && exPnL !== null && Math.abs(localPnL - exPnL) > 0.000001

  return (
    <>
      <h3 style={{ marginTop: '1rem' }}>Exchange report</h3>

      {position.LastError && (
        <div className="error-banner exchange-error">
          <strong>Exchange error</strong>
          <div className="mono exchange-error-text">{position.LastError}</div>
          <div className="text-dim">{formatDateTime(position.LastErrorAt)}</div>
        </div>
      )}

      <table>
        <thead>
          <tr>
            <th style={{ width: '34%' }}>Field</th>
            <th style={{ width: '33%' }}>Exchange (OKX)</th>
            <th style={{ width: '33%' }}>Computed locally</th>
          </tr>
        </thead>
        <tbody>
          <Row
            label="Fill status"
            strategy={fmt(position.Status)}
            model={position.ClosedAt ? 'closed' : 'open'}
          />
          <Row
            label="Close price"
            strategy={fmtPrice(position.ExchangeClosePx)}
            model={fmtPrice(position.ClosePx)}
          />
          <Row
            label="Realized PnL"
            strategy={exPnL !== null ? formatUsd(exPnL) : '—'}
            model={localPnL !== null ? formatUsd(localPnL) : '—'}
          />
          <Row
            label="Fee"
            strategy={position.ExchangeFee !== null ? formatUsd(Number(position.ExchangeFee)) : '—'}
            model={position.FeesUSD !== null ? formatUsd(-Number(position.FeesUSD)) : '—'}
          />
          <Row label="Close reason" strategy="—" model={fmt(position.CloseReason)} />
        </tbody>
      </table>

      {pnlDisagrees && (
        <div className="text-dim exchange-note">
          The exchange's realized PnL differs from the locally computed figure by{' '}
          <span className={'mono ' + pnlClass(exPnL! - localPnL!)}>
            {formatUsd(exPnL! - localPnL!)}
          </span>
          . The exchange's number is authoritative — it accounts for fees, funding and the true fill
          price, which the local calculation cannot see.
        </div>
      )}

      <h4 className="raw-json-heading">Full exchange record</h4>
      <ExchangeRawJSON orderId={position.ID} />

      <div className="stat-row" style={{ marginTop: '0.75rem' }}>
        <span className="text-dim">Open order id</span>
        <span className="mono exchange-id">{position.ExchangeOrderID || '—'}</span>
      </div>
      <div className="stat-row">
        <span className="text-dim">Close order id</span>
        <span className="mono exchange-id">{position.ExchangeCloseOrderID || '—'}</span>
      </div>
      <div className="stat-row">
        <span className="text-dim">Opened / closed</span>
        <span className="mono">
          {formatDateTime(position.OpenedAt)}
          {position.ClosedAt ? ` → ${formatDateTime(position.ClosedAt)}` : ' → still open'}
        </span>
      </div>
    </>
  )
}

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <h2>
            Order #{position.ID} — <span title={position.InstID}>{tokenSymbol(position.InstID)}</span>{' '}
            <span className="text-dim">
              {position.Bar || '—'} · {sig?.kind || position.StrategyName || '—'}
            </span>
          </h2>
          <button className="modal-close" onClick={onClose} aria-label="Close">
            ✕
          </button>
        </div>

        {!obs && (
          <div className="error-banner">
            No decision-time observation was persisted for this order.
          </div>
        )}

        <table>
          <thead>
            <tr>
              <th style={{ width: '34%' }}>Field</th>
              <th style={{ width: '33%' }}>Strategy signal</th>
              <th style={{ width: '33%' }}>Model / final order</th>
            </tr>
          </thead>
          <tbody>
            <Row label="Side" strategy={fmt(sig?.side)} model={fmt(position.Side)} />
            <Row label="Entry price" strategy={fmtPrice(sig?.entry_px)} model={fmtPrice(position.EntryPx)} />
            <Row
              label="Stop loss"
              strategy={fmtPrice(sig?.sl_px) + (sig?.sl_px ? pctFromEntry(sig.sl_px, sig?.entry_px || position.EntryPx, position.Side) : '')}
              model={fmtPrice(position.SLPx) + (position.SLPx ? pctFromEntry(position.SLPx, position.EntryPx, position.Side) : '')}
            />
            <Row
              label="Take profit"
              strategy={fmtPrice(sig?.tp_px) + (sig?.tp_px ? pctFromEntry(sig.tp_px, sig?.entry_px || position.EntryPx, position.Side) : '')}
              model={fmtPrice(position.TPPx) + (position.TPPx ? pctFromEntry(position.TPPx, position.EntryPx, position.Side) : '')}
            />
            <Row label="Size (USD)" strategy="—" model={fmtPrice(position.Size)} />
            <Row label="Leverage" strategy="—" model={fmt(position.Leverage) + 'x'} />
            <Row label="Confidence" strategy={fmt(sig?.confidence)} model="—" />
            <Row label="Strategy win rate" strategy={fmt(sig?.win_rate)} model="—" />
            <Row label="Strategy trade count" strategy={fmt(sig?.trade_count)} model="—" />
          </tbody>
        </table>

        <div className="stat-row" style={{ marginTop: '0.75rem' }}>
          <span className="text-dim">Lifecycle category asked</span>
          <span className="mono">{fmt(obs?.category)}</span>
        </div>
        <div className="stat-row">
          <span className="text-dim">Model shaped this order</span>
          <span>
            {modelSized ? (
              <span className="badge badge-green">yes</span>
            ) : (
              <span className="badge badge-dim">no — fixed sizing fallback</span>
            )}
          </span>
        </div>
        <ExchangeDetails position={position} />

        <h3 style={{ marginTop: '1rem' }}>Adjustment history</h3>
        <AdjustmentHistory orderId={position.ID} mode={position.Mode} />
      </div>
    </div>
  )
}
