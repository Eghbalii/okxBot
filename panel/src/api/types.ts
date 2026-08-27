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

export type PositionMode = 'paper' | 'demo' | 'real'
export type CloseReason = 'sl' | 'tp' | 'manual' | 'timeout'

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
  // ParentOrderID/Variant implement the SL/TP shadow-fork mechanic (CLAUDE.md §15.4): a
  // 'rl_adjusted' row is a linked copy of its 'baseline' parent carrying an RL-proposed SL/TP
  // adjustment, tracked to completion for later comparison rather than editing the parent in place.
  ParentOrderID: number | null
  Variant: 'baseline' | 'rl_adjusted' | ''
}

// VariantStats/SLTPAdjustmentPair back the baseline-vs-rl_adjusted A/B comparison (CLAUDE.md §15.4).
export interface VariantStats {
  Variant: 'baseline' | 'rl_adjusted'
  ClosedCount: number
  Wins: number
  Losses: number
  RealizedPnL: string
}

export interface SLTPAdjustmentPair {
  InstID: string
  BaselineOrder: Position
  RLAdjustedOrder: Position
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
