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
// 'opening'/'closing' mean a request is in flight with the exchange and its outcome is not yet
// known (2026-09-08). A 'closing' order is still an OPEN position — the close is only recorded once
// the exchange confirms the flatten filled, so a failed close leaves a row visibly stuck in
// 'closing' rather than one that claims to be flat.
export type OrderStatus = 'pending' | 'opening' | 'partial' | 'filled' | 'closing' | 'canceled'
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
  // FeesUSD (trading fee) and FundingUSD (accrued funding cost/credit, positive = cost) are both
  // already subtracted into RealizedPnL (2026-09-06) — kept separate rather than combined so this
  // view can show which one actually moved a trade's PnL. Null for still-open positions and for
  // orders closed before these columns existed.
  FeesUSD: string | null
  FundingUSD: string | null
  Mode: PositionMode
  // ParentOrderID/Variant are what remains of the SL/TP shadow-fork mechanic (CLAUDE.md §15.4),
  // removed 2026-09-02 in favor of the RL agent editing an order's SL/TP in place — every order is
  // now Variant='baseline'/ParentOrderID=null going forward; 'rl_adjusted' only appears on
  // historical rows from before the change.
  ParentOrderID: number | null
  // How many in-place SL/TP edits the model has made on this order — the live replacement for
  // ParentOrderID as the "Updated" signal, since forking was removed (see above).
  AdjustmentCount: number
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
  // Real-trading only. The EXCHANGE's own accounting for the close, preferred over the locally
  // computed RealizedPnL/ClosePx wherever present (2026-09-08) — a local calculation cannot see
  // fees, funding, or the true fill price. null means the exchange did not report it, which is
  // deliberately distinct from a zero.
  // The opening order's own id on OKX. Sent by the backend since real orders existed but never
  // typed here until 2026-09-08, so nothing in the panel could show it.
  ExchangeOrderID: string | null
  ExchangeCloseOrderID: string | null
  ExchangeRealizedPnL: string | null
  ExchangeFee: string | null
  ExchangeClosePx: string | null
  // The most recent exchange failure for this order, surfaced as a popup so a trader can act on a
  // stuck open or close rather than find it in a log later.
  LastError: string | null
  LastErrorAt: string | null
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
  // Operator-chosen tradable slice of the real balance (real mode only); null when no cap is set,
  // meaning the whole balance is tradable. The untraded remainder (AccountBalanceUSD - EquityUSD)
  // is the reserve, derived rather than stored.
  TradingCapUSD: string | null
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

// account_equity_history row (GET /api/account/history) — mirrors go-engine/internal/port.
// EquityPoint's JSON shape (Go default casing, no json tags). One row per balance change, so the
// panel's balance/equity chart can show a drain-and-reset that happened overnight after the fact
// (CLAUDE.md §15.7), not just the current number.
export interface EquityPoint {
  ID: number
  Mode: string
  EquityUSD: string // balance AFTER this change
  DeltaUSD: string // signed: realized PnL for a trade, top-up amount for a reset
  Reason: 'trade' | 'reset' | 'seed'
  OrderID: number | null
  InstID: string
  CreatedAt: string
}

// One row of GET /api/paper-trading/affordability — the Manage Tokens modal's min-size column and
// its auto-disabled tag (2026-09-08). Real mode only: paper has no exchange minimums, so the
// endpoint returns an empty list there.
export interface TokenAffordability {
  instId: string
  // The exchange's smallest acceptable position: contract value x price x min size. This is what
  // makes a token untradeable on a small account.
  minNotionalUsd: string
  // The per-token budget it was judged against (equity / active token count, bounded by
  // max_position_pct) — the same figure sizing uses.
  budgetUsd: string
  // Affordable at the CURRENT budget. A disabled token can read true and still be correctly
  // disabled, because that budget exists only because it is excluded — read autoDisabled for
  // "why is this off".
  affordable: boolean
  // The affordability rule is what turned this token off, as opposed to an operator.
  autoDisabled: boolean
  disabled: boolean
  // The instrument or price could not be read, so no judgement was made. Such a token is never
  // auto-disabled: a transient API failure must not take a tradeable token offline.
  unknown: boolean
}

// GET /api/positions/{id}/exchange-order — OKX's own record for both legs of a real position,
// exactly as the exchange returned it (2026-09-09). Each leg is independent: an open position has
// no close leg, and either lookup can fail on its own without invalidating the other.
export interface ExchangeOrderRaw {
  open: Record<string, unknown> | null
  close: Record<string, unknown> | null
  openError?: string
  closeError?: string
  openOrderId?: string
  closeOrderId?: string
}

// POST /api/system/cleanup — what a disk-cleanup run reclaimed. Build cache only: images and
// volumes are deliberately never touched (the RL model's replay buffer and the database live in
// volumes, and an image with no running container is still needed at the next deploy).
export interface CleanupResult {
  buildCacheBytes: number
  error?: string
}

// GET /api/health — one service's container state. `state` is Docker's own vocabulary
// (running/restarting/exited/missing) rather than a reduced up/down, because "restarting" is the
// crash-loop signal that distinguishes CLAUDE.md §47's outage from §48's self-halt. Those look
// identical from the panel and need opposite responses — a fix and a rebuild versus a reset.
export interface ServiceHealth {
  name: string
  container: string
  state: string
  status: string
  health?: string
  critical: boolean
}

// The real-trading circuit breaker. `halted` blocks NEW positions only — existing ones keep being
// monitored and closed, and their SL/TP rests on the exchange regardless (§35).
export interface HaltStatus {
  halted: boolean
  reason?: string
  // safeToReset is re-derived by cmd/api from the exchange and the database, not read from a flag.
  // The reset button is gated on it so it cannot resume trading against state known to be wrong.
  safeToReset: boolean
  blockers?: string[]
  exchangePositions: number
  localOpenOrders: number
}

export interface HealthResponse {
  services: ServiceHealth[]
  halt?: HaltStatus
  dockerError?: string
}

// --- Home page (2026-09-13) ---

// ExchangeBalance is one exchange's account balance for the Home page's top row.
//
// `configured` is false when the deployment holds no credentials for that exchange. The distinction
// matters and is shown explicitly: a missing key and an empty account mean very different things, and
// rendering the former as a zero balance would be a plausible-looking lie. MEXC's authenticated half
// has never run against a real account, so this is the live case rather than a hypothetical.
export interface ExchangeBalance {
  exchange: string
  configured: boolean
  ccy: string
  equityUsd: string
  availUsd: string
  err?: string
}

// MarketToken is one scanned instrument's 24h snapshot — one row per (exchange, symbol), NOT merged
// across exchanges, because the same token's volume and liquidity genuinely differ per venue and
// merging would average away the property that decides whether the bot can trade it.
export interface MarketToken {
  exchange: string
  symbol: string
  execInstId: string
  lastPx: string
  high24h: string
  low24h: string
  vol24hUsd: string
  change24hPct: string
  range24hPct: string
  score: string
  scannedAt: string
  inRoster: boolean
  enabledPaper: boolean
  enabledReal: boolean
}

// Instrument is one row of the tradeable roster — the tokens actually being collected and traded, as
// opposed to the whole scanned market above. The three flags are independent by design: a discovered
// token collects data and paper-trades immediately while staying off for real money until a person
// enables it.
export interface Instrument {
  id: number
  symbol: string
  exchange: string
  execInstId: string
  instType: string
  enabledIngest: boolean
  enabledPaper: boolean
  enabledReal: boolean
  source: 'manual' | 'scan' | 'seed'
  vol24hUsd: string
  change24hPct: string
  scanScore: string
  updatedAt: string
  // True only when this symbol has a live, enabled strategy assignment for the queried mode
  // (2026-09-17) — NOT the same as enabledPaper/enabledReal, which a token can carry without ever
  // actually trading (e.g. a MEXC row: enabled_paper is set on admission, but paper-trader's
  // roster load is OKX-only, so it never gets an assignment and is never truly active).
  active: boolean
}

export interface ScanResult {
  exchange: string
  scanned: number
  candidates: number
  admitted: number
  err?: string
}
