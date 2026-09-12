import type { Position } from '../api/types'

/**
 * Unrealized PnL for an open position, computed client-side from entry/size/leverage against the
 * live streamed price (CLAUDE.md §11.4) — mirrors usecase.unrealizedPnLPct Go-side, so the panel
 * doesn't need a REST round-trip for something it can derive from data it already holds.
 *
 * pct is PnL as a fraction of MARGIN (the price move scaled by leverage), matching OKX's own
 * uplRatio convention and usd's own leverage multiplication. Before 2026-08-31 pct omitted leverage
 * entirely (§19.1), so a 1% price move at 20x displayed as 1% instead of the correct 20% — which is
 * why this lives in one shared place rather than being re-derived per call site.
 */
export function unrealizedPnL(
  p: Position,
  lastPrice: string | undefined,
): { pct: number; usd: number } | null {
  if (p.ClosedAt) return null // realized, not unrealized — RealizedPnL covers this case
  if (!lastPrice) return null
  const entry = Number(p.EntryPx)
  const last = Number(lastPrice)
  const size = Number(p.Size)
  const leverage = Number(p.Leverage) || 1
  if (!entry || !Number.isFinite(last)) return null
  const direction = p.Side === 'buy' ? 1 : -1
  const pct = (direction * (last - entry) * 100 * leverage) / entry
  const usd = ((direction * (last - entry)) / entry) * size * leverage
  return { pct, usd }
}
