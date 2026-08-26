import type {
  ModelStatus,
  Position,
  PositionMode,
  SLTPAdjustmentPair,
  StrategyAssignment,
  StrategyConfig,
  StrategyStats,
  VariantStats,
} from './types'

// Same-origin in production (the panel is served from behind the OpenVPN-only cmd/api host, per
// CLAUDE.md §11); proxied to cmd/api by vite.config.ts during local dev.
const BASE = '/api'

class ApiError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }))
    throw new ApiError(res.status, body.error ?? res.statusText)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

// Go's `ListX` repository methods return a nil slice (JSON `null`, not `[]`) when there are no
// rows — e.g. GET /api/assignments on a fresh install. Every list endpoint goes through this so
// pages can .map()/.filter() the result without a null check at every call site.
async function requestList<T>(path: string, init?: RequestInit): Promise<T[]> {
  const data = await request<T[] | null>(path, init)
  return data ?? []
}

export const api = {
  resources: () => request<{ grafanaUrl: string }>('/resources'),

  modelStatus: () => request<ModelStatus>('/model/status'),
  modelLogs: (unit: string, opts?: { lines?: number; errorsOnly?: boolean }) => {
    const params = new URLSearchParams({ unit: unit })
    if (opts?.lines) params.set('lines', String(opts.lines))
    if (opts?.errorsOnly) params.set('errors', 'true')
    return request<{ unit: string; logs: string }>(`/model/logs?${params}`)
  },

  listStrategies: (opts?: { instId?: string; enabledOnly?: boolean }) => {
    const params = new URLSearchParams()
    if (opts?.instId) params.set('instId', opts.instId)
    if (opts?.enabledOnly) params.set('enabledOnly', 'true')
    return requestList<StrategyConfig>(`/strategies?${params}`)
  },
  getStrategy: (id: number) => request<StrategyConfig>(`/strategies/${id}`),
  createStrategy: (body: {
    name: string
    clonedFrom: number
    instIds: string[]
    config?: Record<string, number>
  }) =>
    request<{ id: number }>('/strategies', {
      method: 'POST',
      body: JSON.stringify({
        name: body.name,
        clonedFrom: body.clonedFrom,
        instIds: body.instIds,
        config: body.config,
      }),
    }),
  updateStrategy: (id: number, body: { config: Record<string, number>; enabled: boolean }) =>
    request<void>(`/strategies/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  deleteStrategy: (id: number) => request<void>(`/strategies/${id}`, { method: 'DELETE' }),
  resetStrategy: (id: number) => request<void>(`/strategies/${id}/reset`, { method: 'POST' }),
  strategyStats: (id: number) => request<StrategyStats>(`/strategies/${id}/stats`),

  listAssignments: (opts?: { instId?: string; enabledOnly?: boolean }) => {
    const params = new URLSearchParams()
    if (opts?.instId) params.set('instId', opts.instId)
    if (opts?.enabledOnly) params.set('enabledOnly', 'true')
    return requestList<StrategyAssignment>(`/assignments?${params}`)
  },
  createAssignment: (body: { strategyId: number; instId: string; bar: string }) =>
    request<{ id: number }>('/assignments', {
      method: 'POST',
      body: JSON.stringify({ StrategyID: body.strategyId, InstID: body.instId, Bar: body.bar }),
    }),
  setAssignmentEnabled: (id: number, enabled: boolean) =>
    request<void>(`/assignments/${id}`, { method: 'PATCH', body: JSON.stringify({ enabled }) }),
  deleteAssignment: (id: number) => request<void>(`/assignments/${id}`, { method: 'DELETE' }),

  listPositions: (opts?: {
    mode?: PositionMode
    instId?: string
    open?: boolean
    sortBy?: 'opened_at' | 'closed_at' | 'pnl' | 'inst_id'
    sortDesc?: boolean
  }) => {
    const params = new URLSearchParams()
    if (opts?.mode) params.set('mode', opts.mode)
    if (opts?.instId) params.set('instId', opts.instId)
    if (opts?.open !== undefined) params.set('open', String(opts.open))
    if (opts?.sortBy) params.set('sortBy', opts.sortBy)
    if (opts?.sortDesc) params.set('sortDesc', 'true')
    return requestList<Position>(`/positions?${params}`)
  },

  // Baseline-vs-rl_adjusted A/B comparison (CLAUDE.md §15.4). `since` is an RFC3339 timestamp
  // (e.g. `new Date(Date.now() - 7*86400e3).toISOString()` for "the last week").
  sltpAdjustmentStats: (opts?: { instId?: string; since?: string }) => {
    const params = new URLSearchParams()
    if (opts?.instId) params.set('instId', opts.instId)
    if (opts?.since) params.set('since', opts.since)
    return requestList<VariantStats>(`/sltp-adjustments/stats?${params}`)
  },
  sltpAdjustmentPairs: (opts?: { instId?: string }) => {
    const params = new URLSearchParams()
    if (opts?.instId) params.set('instId', opts.instId)
    return requestList<SLTPAdjustmentPair>(`/sltp-adjustments/pairs?${params}`)
  },
}

export { ApiError }
