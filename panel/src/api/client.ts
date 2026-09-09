import type {
  AccountEquity,
  EquityPoint,
  Candle,
  ModelStatus,
  PaperOrderAdjustment,
  PaperTradingConfig,
  PaperTradingStats,
  ParamChange,
  Position,
  PositionMode,
  StrategyAssignment,
  StrategyConfig,
  StrategyStats,
  CleanupResult,
  ExchangeOrderRaw,
  TokenAffordability,
  TokenStats,
  TesterConfig,
  TesterVersion,
  TesterVersionDetail,
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
  // mode ("paper" or "real") scopes the track record — paper and real trading each have a fully
  // independent one, sourced from paper_orders vs. real_orders respectively.
  strategyStats: (id: number, mode: PositionMode) => request<StrategyStats>(`/strategies/${id}/stats?mode=${mode}`),

  listAssignments: (opts?: { instId?: string; enabledOnly?: boolean; mode?: PositionMode }) => {
    const params = new URLSearchParams()
    if (opts?.instId) params.set('instId', opts.instId)
    if (opts?.enabledOnly) params.set('enabledOnly', 'true')
    if (opts?.mode) params.set('mode', opts.mode)
    return requestList<StrategyAssignment>(`/assignments?${params}`)
  },
  createAssignment: (body: { strategyId: number; instId: string; bar: string; mode?: PositionMode }) =>
    request<{ id: number }>('/assignments', {
      method: 'POST',
      body: JSON.stringify({ StrategyID: body.strategyId, InstID: body.instId, Bar: body.bar, Mode: body.mode }),
    }),
  setAssignmentEnabled: (id: number, enabled: boolean) =>
    request<void>(`/assignments/${id}`, { method: 'PATCH', body: JSON.stringify({ enabled }) }),
  deleteAssignment: (id: number) => request<void>(`/assignments/${id}`, { method: 'DELETE' }),

  // Paged server-side since 2026-09-02: closed positions grew into the hundreds and fetching every
  // row on every 5s poll (then sorting/paginating client-side) had become a genuinely slow query
  // and a multi-MB payload. page is 0-indexed; pageSize defaults server-side if omitted.
  listPositions: (opts?: {
    mode?: PositionMode
    instId?: string
    open?: boolean
    sortBy?: 'opened_at' | 'closed_at' | 'pnl' | 'inst_id'
    sortDesc?: boolean
    page?: number
    pageSize?: number
  }) => {
    const params = new URLSearchParams()
    if (opts?.mode) params.set('mode', opts.mode)
    if (opts?.instId) params.set('instId', opts.instId)
    if (opts?.open !== undefined) params.set('open', String(opts.open))
    if (opts?.sortBy) params.set('sortBy', opts.sortBy)
    if (opts?.sortDesc) params.set('sortDesc', 'true')
    if (opts?.page !== undefined) params.set('page', String(opts.page))
    if (opts?.pageSize !== undefined) params.set('pageSize', String(opts.pageSize))
    return request<{ items: Position[] | null; total: number }>(`/positions?${params}`).then((r) => ({
      items: r.items ?? [],
      total: r.total,
    }))
  },
  // Manual close from the panel (2026-08-31, extended to real positions 2026-09-04): flags the
  // order for the owning engine (PaperTrader or RealTrader) to close on its next tick,
  // close_reason='manual', reported to the model as closed_early. mode is required — paper_orders.id
  // and real_orders.id are independent sequences post-table-split, so it disambiguates which table
  // id addresses.
  closePosition: (id: number, mode: PositionMode) =>
    request<{ ok: boolean }>(`/positions/${id}/close?mode=${mode}`, { method: 'POST' }),

  // Manual SL/TP edit for real positions (CLAUDE.md §27's real-trading plan §3b, 2026-09-03) —
  // real mode only, unlike closePosition above which also supports paper. Each percentage is
  // SIGNED and leverage-adjusted (negative = loss side, positive = profit side, e.g. -5 on a 10x
  // position moves that level to a 0.5% price move from entry) and is applied with NO clamp —
  // an explicit operator/admin action is trusted directly, unlike the model's own automated edits.
  adjustPosition: (id: number, mode: PositionMode, body: { slPct?: number; tpPct?: number }) =>
    request<{ slPx: string | null; tpPx: string | null }>(`/positions/${id}/adjust?mode=${mode}`, {
      method: 'POST',
      body: JSON.stringify(body),
    }),

  // An order's in-trade SL/TP adjustment history (CLAUDE.md §15.4/§15.12 revision, 2026-09-02) —
  // replaces the old baseline-vs-rl_adjusted A/B comparison now that the RL mechanic edits the
  // order in place instead of forking it. mode picks paper_order_adjustments vs.
  // real_order_adjustments.
  paperOrderAdjustments: (orderId: number, mode: PositionMode) =>
    requestList<PaperOrderAdjustment>(`/positions/${orderId}/adjustments?mode=${mode}`),

  // Candles + param-changes back the Strategies page's price-chart marker overlay (CLAUDE.md
  // §16): candles draw the price line, param-changes draw the vertical "params changed here"
  // marker lines on top of it.
  candles: (opts: { instId: string; bar: string; limit?: number }) => {
    const params = new URLSearchParams({ instId: opts.instId, bar: opts.bar })
    if (opts.limit) params.set('limit', String(opts.limit))
    return requestList<Candle>(`/candles?${params}`)
  },
  paramChanges: (strategyId: number, opts: { instId: string; since?: string }) => {
    const params = new URLSearchParams({ instId: opts.instId })
    if (opts.since) params.set('since', opts.since)
    return requestList<ParamChange>(`/strategies/${strategyId}/param-changes?${params}`)
  },

  // Independent strategy-tester service (2026-08-30 request), proxied through cmd/api.
  testerStats: () => requestList<TesterVersion>('/tester/stats'),
  testerVersion: (id: number) => request<TesterVersionDetail>(`/tester/versions/${id}`),
  createTesterVersion: (kind: string, config: Record<string, number>) =>
    request<TesterVersion>('/tester/versions', {
      method: 'POST',
      body: JSON.stringify({ kind, config }),
    }),
  enableTesterVersion: (id: number) =>
    request<{ ok: boolean }>(`/tester/versions/${id}/enable`, { method: 'POST' }),
  // 2026-08-31: lets the operator permanently remove a version they no longer want kept — the
  // automatic optimizer loop otherwise never deletes anything on its own.
  deleteTesterVersion: (id: number) =>
    request<{ ok: boolean }>(`/tester/versions/${id}`, { method: 'DELETE' }),
  testerConfig: () => request<TesterConfig>('/tester/config'),
  saveTesterConfig: (patch: { bar?: string; notionalUsd?: string; leverage?: string; maxOpenDuration?: string }) =>
    request<{ ok: boolean; restartRequired: boolean }>('/tester/config', {
      method: 'PUT',
      body: JSON.stringify(patch),
    }),
  restartTester: () => request<{ status: string }>('/tester/restart', { method: 'POST' }),

  // Paper-trading control box + stats box (2026-09-01 request, extended to real trading
  // 2026-09-04), above the Positions table — mode selects which tab's data this serves.
  paperTradingStats: (mode: PositionMode) => request<PaperTradingStats>(`/paper-trading/stats?mode=${mode}`),
  // "Manage tokens" modal's per-token 24h stats table (2026-09-04 request) — paper-trading only,
  // no mode param (TokenStats24h has no real-trading equivalent yet).
  tokenStats24h: (mode: PositionMode) => requestList<TokenStats>(`/paper-trading/token-stats?mode=${mode}`),
  // Per-token exchange minimums vs. the current per-token budget, for the Manage Tokens modal's
  // min-size column and auto-disabled tag (2026-09-08). Empty in paper mode by design.
  // OKX's own untouched record for both legs of a real position, fetched live rather than served
  // from stored columns — the stored fields are a chosen few, this is everything the exchange knows.
  // Reclaims the Docker build cache. Build cache only — see the endpoint's own doc comment for
  // why images/volumes are deliberately not touched.
  cleanupDisk: () => request<CleanupResult>('/system/cleanup', { method: 'POST' }),
  exchangeOrderRaw: (id: number) =>
    request<ExchangeOrderRaw>(`/positions/${id}/exchange-order`),
  tokenAffordability: (mode: PositionMode) =>
    requestList<TokenAffordability>(`/paper-trading/affordability?mode=${mode}`),
  // Go's null-array-column columns (activeKinds/disabledInstIds/activeBars) marshal as JSON null,
  // not [] — normalized here the same way requestList does for list endpoints, so PaperTradingConfig
  // consumers can always call .length/.map on these fields without a crash (found live: an
  // unnormalized null.length threw and unmounted the whole app to a blank page after the initial
  // paint, since nothing here has an error boundary).
  paperTradingConfig: (mode: PositionMode) =>
    request<PaperTradingConfig>(`/paper-trading/config?mode=${mode}`).then((c) => ({
      ...c,
      activeKinds: c.activeKinds ?? [],
      disabledInstIds: c.disabledInstIds ?? [],
      activeBars: c.activeBars ?? [],
      allInstIds: c.allInstIds ?? [],
    })),
  savePaperTradingConfig: (mode: PositionMode, patch: Partial<Omit<PaperTradingConfig, 'allInstIds'>>) =>
    request<{ ok: boolean; restartRequired: boolean }>('/paper-trading/config', {
      method: 'PUT',
      body: JSON.stringify({ ...patch, mode }),
    }),
  // Paper trading restarts cmd/paper-trader; real trading restarts cmd/trader (its own self-restart
  // handler, added alongside the mode-scoped config work) via cmd/api's mode-aware proxy.
  restartPaperTrader: (mode: PositionMode) =>
    request<{ status: string }>(`/paper-trading/restart?mode=${mode}`, { method: 'POST' }),

  // CLAUDE.md §31.2/§31.3: an explicit deposit/withdrawal bringing both "Total Equity" and the
  // real, continuous "Account Balance" to the same new value together — takes effect immediately,
  // no restart needed, since dynamic sizing reads the live account_equity row on every open rather
  // than a cached value.
  setAccountCap: (newCapUsd: string, mode: PositionMode) =>
    request<AccountEquity>('/account/cap', {
      method: 'POST',
      body: JSON.stringify({ mode, newCapUsd }),
    }),

  // Current account row, for the trading-cap slider's upper bound (the real Account Balance —
  // capping above what the account actually holds isn't a deposit the panel can make).
  account: (mode: PositionMode) => request<AccountEquity>(`/account?mode=${mode}`),

  // Balance/equity timeline for the stats box's chart. `since` is RFC3339; passing it explicitly
  // overrides cmd/api's own default of "since the last reset" (CLAUDE.md §31.2), which is what the
  // day/week/month/year range selector needs — a fixed window, not one that shifts with resets.
  accountHistory: (mode: PositionMode, since: Date, limit = 0) =>
    requestList<EquityPoint>(
      `/account/history?mode=${mode}&since=${encodeURIComponent(since.toISOString())}&limit=${limit}`,
    ),
}

// PaperOrderEvent mirrors Go's usecase.PaperOrderEvent — the lightweight message pushed over
// GET /api/ws whenever cmd/paper-trader opens or closes a paper order (CLAUDE.md §11.4/§12).
export interface PaperOrderEvent {
  type: 'opened' | 'closed'
  orderId: number
  instId: string
}

// PriceUpdate mirrors cmd/api's priceUpdate — the live last-traded price pushed over the same
// socket on every tick (CLAUDE.md §11.4's positions panel), for moment-to-moment PnL client-side.
export interface PriceUpdate {
  type: 'price'
  instId: string
  price: string
}

export type WSEvent = PaperOrderEvent | PriceUpdate

// openEventsSocket connects to cmd/api's WebSocket bridge and calls onEvent for every message
// received (paper order open/close, or a live price tick — discriminate on `type`), reconnecting
// with backoff if the connection drops. Returns a cleanup function that closes the socket and
// stops reconnecting.
export function openEventsSocket(onEvent: (event: WSEvent) => void): () => void {
  let socket: WebSocket | null = null
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null
  let closed = false
  let backoffMs = 1000

  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const url = `${proto}//${window.location.host}${BASE}/ws`

  function connect() {
    if (closed) return
    socket = new WebSocket(url)
    socket.onmessage = (msg) => {
      try {
        onEvent(JSON.parse(msg.data) as WSEvent)
      } catch {
        // ignore malformed frames
      }
    }
    socket.onopen = () => {
      backoffMs = 1000
    }
    socket.onclose = () => {
      if (closed) return
      reconnectTimer = setTimeout(connect, backoffMs)
      backoffMs = Math.min(backoffMs * 2, 30_000)
    }
    socket.onerror = () => {
      socket?.close()
    }
  }
  connect()

  return () => {
    closed = true
    if (reconnectTimer) clearTimeout(reconnectTimer)
    socket?.close()
  }
}

export { ApiError }
