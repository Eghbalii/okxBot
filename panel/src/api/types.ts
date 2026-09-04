// Mirrors go-engine/internal/port types exposed by cmd/api (internal/api/server.go). Keep in
// sync with that package's JSON shapes — there is no shared schema generation yet (CLAUDE.md §11).

export interface StrategyConfig {
  ID: number
  Name: string
  InstIDs: string[]
  Kind: string
  Config: Record<string, number> | null
  Enabled: boolean
  IsOrigin: boolean
  ClonedFrom: number | null
}

export interface StrategyAssignment {
  ID: number
  StrategyID: number
  InstID: string
  Bar: string
  Enabled: boolean
}

export interface StrategyStats {
  StrategyID: number
  SignalCount: number
  Wins: number
  Losses: number
  OpenCount: number
  RealizedPnL: string // decimal.Decimal marshals as a JSON string
  FirstOpened: string | null
  LastActivity: string | null
}

// TokenStats is one row of the "Manage tokens" modal's 24h stats table (2026-09-04 request) —
// positions CLOSED in the last 24h, per token.
export interface TokenStats {
  instId: string
  positionCount: number
  pnlUsd: string // decimal.Decimal marshals as a JSON string
  pnlPct: string
}

// 'demo' was dropped 2026-09-04 (real-trading readiness plan) — the project never built a demo
// controller and decided not to pursue one; only paper and real trading exist going forward.
export type PositionMode = 'paper' | 'real'
// Fill-lifecycle status for a real order (real_orders.status) — null for paper/demo rows, which
// have no fill lifecycle (a paper order is always instantly and fully filled).
export type OrderStatus = 'pending' | 'partial' | 'filled' | 'canceled'
// 'rl_early' is the model choosing to close a position before either SL or TP was touched
// (CLAUDE.md §15.12's early-close action, gated behind paper_trading.rl_early_close).
export type CloseReason = 'sl' | 'tp' | 'manual' | 'timeout' | 'rl_early'

export interface Position {
  ID: number
  InstID: string
  StrategyID: number | null
  Side: 'buy' | 'sell'
  EntryPx: string
  SLPx: string | null
  TPPx: string | null
  Size: string
  Leverage: string
  OpenedAt: string
  ClosedAt: string | null
  CloseReason: CloseReason | null
  ClosePx: string | null
  RealizedPnL: string | null
  Mode: PositionMode
  // ParentOrderID/Variant are what remains of the SL/TP shadow-fork mechanic (CLAUDE.md §15.4),
  // removed 2026-09-02 in favor of the RL agent editing an order's SL/TP in place — every order is
  // now Variant='baseline'/ParentOrderID=null going forward; 'rl_adjusted' only appears on
  // historical rows from before the change.
  ParentOrderID: number | null
  Variant: 'baseline' | 'rl_adjusted' | ''
  // Bar is the decision timeframe the signal that opened this order fired on (e.g. "5m", "1H").
  // Empty for orders opened before this field existed.
  Bar: string
  // StrategyName is joined in by ListPositions for display — empty when StrategyID is null.
  StrategyName: string
  // PnLMaxPct/PnLMinPct are the peak and trough unrealized PnL this position has reached while
  // open (fraction of margin, same scale as the live PnL% shown in the table) — CLAUDE.md §15.11.
  // Zero-valued (not null) for a position that never updated them.
  PnLMaxPct: string
  PnLMinPct: string
  // FeaturesJSON is the decision-time observation actually sent to the model (CLAUDE.md §15.3),
  // stored as embedded JSON. Shape is the rl_service Observation; the order-detail view reads the
  // strategy signal out of it to compare against what the order ended up with.
  FeaturesJSON: unknown
  // Status is the fill lifecycle for a real order (real_orders.status) — null for paper/demo rows.
  Status: OrderStatus | null
}

// PaperOrderAdjustment is one row of an order's in-trade SL/TP adjustment history (CLAUDE.md
// §15.4/§15.12 revision, 2026-09-02) — the audit trail that replaced the shadow-fork mechanic's
// implicit fork-vs-baseline comparison; the order-detail modal renders these as a change table.
export interface PaperOrderAdjustment {
  ID: number
  OrderID: number
  Field: 'sl' | 'tp'
  OldValue: string | null
  NewValue: string | null
  Source: 'model' | 'optimizer' | 'manual'
  CreatedAt: string
}

export interface RLHealth {
  reachable: boolean
  status?: string
  modelLoaded: boolean
  error?: string
}

export interface UnitStatus {
  unit: string
  active: boolean
  state: string
  subState: string
  startedAt?: string
  uptimeSec?: number
  restartCount: number
  error?: string
}

export interface ModelStatus {
  health: RLHealth
  units: UnitStatus[]
}

// Candle/ParamChange back the Strategies page's price-chart + parameter-change marker overlay
// (CLAUDE.md §16, the strategy parameter optimizer's chart requirement).
export interface Candle {
  InstID: string
  Bar: string
  Timestamp: string
  Open: string
  High: string
  Low: string
  Close: string
  Volume: string
}

export interface ParamChange {
  ID: number
  StrategyID: number
  InstID: string
  OldConfig: Record<string, number> | null
  NewConfig: Record<string, number>
  Source: 'optimizer' | 'manual'
  CreatedAt: string
}

// Independent strategy-tester service (2026-08-30 request) — proxied through cmd/api's
// /api/tester/* routes. Its own JSON uses camelCase (unlike the rest of this file, which mirrors
// Go's PascalCase field names directly) because cmd/strategy-tester's response is forwarded
// byte-for-byte rather than re-decoded/re-encoded by cmd/api.
export interface TesterVersionStats {
  signalCount: number
  wins: number
  losses: number
  tpCloses: number
  slCloses: number
  openCount: number
  winRatePct: string
  realizedPnl: string
}

export interface TesterVersion {
  id: number
  kind: string
  version: number
  displayName: string
  config: Record<string, number> | null
  // Every tunable param's actual running value (overrides merged onto factory defaults) — use
  // this for a compare/diff view, not `config`, which is empty whenever a version overrides
  // nothing and would otherwise make every param look like it changed from nothing.
  effectiveConfig: Record<string, number> | null
  parentVersionId: number | null
  enabled: boolean
  // "origin" | "manual" | "optimizer" — distinguishes the seeded default, an operator's panel
  // edit, and the automatic optimizer loop's own proposal (2026-08-31).
  source: string
  stats: TesterVersionStats
}

export interface TesterVersionDetail {
  version: TesterVersion
  parent?: TesterVersion
}

export interface TesterConfig {
  bar: string
  instIds: string[]
  notionalUsd: string
  leverage: string
  maxOpenDuration: string // Go duration string, e.g. "6h"
}

// Paper-trading control box + stats box (2026-09-01 request), proxied/read through cmd/api's
// /api/paper-trading/* routes.
export interface PaperTradingStats {
  openCount: number
  // "Total Equity": the balance SINCE the operator last chose a baseline via POST /api/account/cap
  // (or the last automatic drain-to-zero reset) — what new position sizing is computed from.
  totalEquityUsd: string
  // "Account Balance": the real, continuous running total, never reset by a baseline change
  // (CLAUDE.md §31.2). Equal to totalEquityUsd until the account's first-ever reset, then diverges.
  accountBalanceUsd: string
  pnl24hUsd: string
  pnl24hPct: string
  pnl7dUsd: string
  pnl7dPct: string
  pnl30dUsd: string
  pnl30dPct: string
}

// account_equity row (GET /api/account) — mirrors go-engine/internal/port.AccountEquity's JSON
// shape (Go's default encoding/json field-name casing, i.e. capitalized, since that struct has no
// json tags).
export interface AccountEquity {
  Mode: string
  InitialUSD: string
  EquityUSD: string
  AccountBalanceUSD: string
  ResetCount: number
  LastResetAt: string | null
  UpdatedAt: string
}

export type TradingState = 'running' | 'paused' | 'stopped'

export interface PaperTradingConfig {
  tradingState: TradingState
  disableLong: boolean
  disableShort: boolean
  // Empty = no per-kind restriction (every currently-enabled assignment applies as-is).
  activeKinds: string[]
  disabledInstIds: string[]
  // Empty = use paper_trading.bars from config.yaml as-is.
  activeBars: string[]
  // Not itself part of the saved config — the full configured trading.inst_ids roster, so the
  // token-manage modal knows what tokens exist to toggle.
  allInstIds: string[]
}
