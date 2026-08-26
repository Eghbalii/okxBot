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
