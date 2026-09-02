import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { PaperOrderAdjustment, Position } from '../api/types'
import { tokenSymbol } from '../utils/format'

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

// Distance of a SL/TP level from entry, as a signed percent (negative = below entry). Shown next
// to the raw price so a level can be read without mentally computing the distance each time.
function pctFromEntry(level: string | number | null | undefined, entryPx: string): string {
  const entry = Number(entryPx)
  const lvl = Number(level)
  if (!entry || level === null || level === undefined || level === '' || Number.isNaN(lvl)) return ''
  const pct = ((lvl - entry) / entry) * 100
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
function AdjustmentHistory({ orderId }: { orderId: number }) {
  const [adjustments, setAdjustments] = useState<PaperOrderAdjustment[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    api
      .paperOrderAdjustments(orderId)
      .then((rows) => {
        if (!cancelled) setAdjustments(rows)
      })
      .catch((err) => {
        if (!cancelled) setError((err as Error).message)
      })
    return () => {
      cancelled = true
    }
  }, [orderId])

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
              {fmt(a.OldValue)} → {fmt(a.NewValue)}
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
            <Row label="Entry price" strategy={fmt(sig?.entry_px)} model={fmt(position.EntryPx)} />
            <Row
              label="Stop loss"
              strategy={fmt(sig?.sl_px) + (sig?.sl_px ? pctFromEntry(sig.sl_px, sig?.entry_px || position.EntryPx) : '')}
              model={fmt(position.SLPx) + (position.SLPx ? pctFromEntry(position.SLPx, position.EntryPx) : '')}
            />
            <Row
              label="Take profit"
              strategy={fmt(sig?.tp_px) + (sig?.tp_px ? pctFromEntry(sig.tp_px, sig?.entry_px || position.EntryPx) : '')}
              model={fmt(position.TPPx) + (position.TPPx ? pctFromEntry(position.TPPx, position.EntryPx) : '')}
            />
            <Row label="Size (USD)" strategy="—" model={fmt(position.Size)} />
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
        <h3 style={{ marginTop: '1rem' }}>Adjustment history</h3>
        <AdjustmentHistory orderId={position.ID} />
      </div>
    </div>
  )
}
