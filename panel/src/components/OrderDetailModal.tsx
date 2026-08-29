import type { Position } from '../api/types'

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
            Order #{position.ID} — {position.InstID}{' '}
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
            <Row label="Stop loss" strategy={fmt(sig?.sl_px)} model={fmt(position.SLPx)} />
            <Row label="Take profit" strategy={fmt(sig?.tp_px)} model={fmt(position.TPPx)} />
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
        <div className="stat-row">
          <span className="text-dim">Variant</span>
          <span className="mono">{position.Variant || 'baseline'}</span>
        </div>
      </div>
    </div>
  )
}
