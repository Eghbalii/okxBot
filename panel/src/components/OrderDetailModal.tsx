import { memo, useEffect, useState } from 'react'
import { api } from '../api/client'
import type { ExchangeOrderRaw, PaperOrderAdjustment, Position, PositionMode } from '../api/types'
import { tokenSymbol, trimPrice } from '../utils/format'

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
// Memoized for the same reason as ExchangeRawJSON above — a 5s poll that changed nothing must not
// rebuild this list.
const AdjustmentHistory = memo(function AdjustmentHistory({ orderId, mode }: { orderId: number; mode: PositionMode }) {
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
})

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
// JSON, not a few parameters you picked").
//
// Served from what the engine already captured at each leg's terminal state, not fetched live —
// so opening this modal costs a row read rather than two exchange calls against a rate-limit
// budget shared with real trading, and shows the same bytes every time.
//
// Deliberately unformatted beyond indentation: the value is that nothing was interpreted or
// dropped on the way through, so picking fields out to display would defeat the purpose.
// Memoized on orderId alone: the positions list refetches every 5 seconds and hands this modal a
// brand-new position object each time, even when nothing about the order changed. Without this the
// whole block tears down and rebuilds on every poll — which is visible as the panel flickering, and
// on a long JSON payload also throws away the reader's scroll position mid-read.
const ExchangeRawJSON = memo(function ExchangeRawJSON({ orderId }: { orderId: number }) {
  const [data, setData] = useState<ExchangeOrderRaw | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    api
      .exchangeOrderRaw(orderId)
      .then((d) => {
        if (!cancelled) setData(d)
      })
      .catch((err) => {
        if (!cancelled) setError((err as Error).message)
      })
    return () => {
      cancelled = true
    }
  }, [orderId])

  if (error) return <div className="error-banner">{error}</div>
  if (!data) return <div className="text-dim">Loading…</div>

  return (
    <div className="raw-json-wrap">
      <RawLeg label="Open order" id={data.openOrderId} body={data.open} error={data.openError} />
      <RawLeg label="Close order" id={data.closeOrderId} body={data.close} error={data.closeError} />
    </div>
  )
})

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
        <div className="text-dim">{error}</div>
      ) : (
        <pre className="raw-json mono">{JSON.stringify(body, null, 2)}</pre>
      )}
    </div>
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
        {/* Real orders only: paper trading has no exchange leg, so there is no record to show. */}
        {position.Mode === 'real' && (
          <>
            <h4 className="raw-json-heading">Full exchange record</h4>
            <ExchangeRawJSON orderId={position.ID} />
          </>
        )}

        <h3 style={{ marginTop: '1rem' }}>Adjustment history</h3>
        <AdjustmentHistory orderId={position.ID} mode={position.Mode} />
      </div>
    </div>
  )
}
