import { ExchangeIcon } from './TokenIcon'
import type { ExchangeBalance } from '../api/types'

function fmtUsd(n: number): string {
  if (!Number.isFinite(n)) return '—'
  return `$${n.toFixed(2)}`
}

// Redesigned Account-page exchange card (2026-09-20 request): the previous version buried the
// exchange's identity under a big number, when the identity — which exchange, is it connected —
// is what an operator actually orients on first. Logo + name now lead the card; the balance is a
// secondary line underneath, and a row of key-management actions (currently placeholders, see the
// TODOs below and docs/ACCOUNT_PAGE_TODO.md) gives the card somewhere to grow instead of existing
// only to show one number.
//
// The API key add/remove/sync actions below are DELIBERATELY NOT WIRED to anything yet — there is
// no backend support for storing per-exchange credentials from the panel (credentials today are
// environment/config-file only, per CLAUDE.md §27.1's gateway design) or for an on-demand balance
// re-fetch outside the existing 30s poll. Building real versions of these is its own task, tracked
// in docs/ACCOUNT_PAGE_TODO.md — what's here is the visual shape so the card reads as a real
// per-exchange management panel rather than a placeholder for a single number, without pretending
// any of these buttons do something they can't do yet.
export default function ExchangeAccountCard({ b }: { b: ExchangeBalance }) {
  const configured = b.configured
  const hasError = configured && !!b.err

  return (
    <div className={'xcard' + (configured ? '' : ' xcard-unconfigured')}>
      <div className="xcard-head">
        <ExchangeIcon exchange={b.exchange} size={32} />
        <div className="xcard-title">
          <div className="xcard-name">{b.exchange.toUpperCase()}</div>
          <div className={'xcard-status' + (configured ? (hasError ? ' xcard-status-error' : ' xcard-status-ok') : '')}>
            {!configured ? 'Not connected' : hasError ? 'Unavailable' : 'Connected'}
          </div>
        </div>
      </div>

      <div className="xcard-balance-row">
        {!configured ? (
          <div className="xcard-balance-muted">No API key configured</div>
        ) : hasError ? (
          <div className="xcard-balance-error" title={b.err}>
            Balance unavailable
          </div>
        ) : (
          <>
            <div className="xcard-balance">
              {fmtUsd(Number(b.equityUsd))} <span className="xcard-ccy">{b.ccy}</span>
            </div>
            <div className="xcard-avail">{fmtUsd(Number(b.availUsd))} available</div>
          </>
        )}
      </div>

      <div className="xcard-meta">
        {configured ? (
          <>
            {/* Placeholder: no backend field for when a key was added yet — see TODO doc. */}
            <span className="xcard-meta-item">Key added: —</span>
            <span className="xcard-meta-sep">·</span>
            <span className="xcard-meta-item">Last synced: just now</span>
          </>
        ) : (
          <span className="xcard-meta-item">No key on file</span>
        )}
      </div>

      <div className="xcard-actions">
        {configured ? (
          <>
            <button className="btn-ghost" disabled title="Not yet implemented — see docs/ACCOUNT_PAGE_TODO.md">
              Sync now
            </button>
            <button className="btn-ghost btn-ghost-danger" disabled title="Not yet implemented — see docs/ACCOUNT_PAGE_TODO.md">
              Remove key
            </button>
          </>
        ) : (
          <button className="btn-ghost" disabled title="Not yet implemented — see docs/ACCOUNT_PAGE_TODO.md">
            + Add API key
          </button>
        )}
      </div>
    </div>
  )
}
