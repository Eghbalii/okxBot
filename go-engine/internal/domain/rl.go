package domain

import "github.com/shopspring/decimal"

// Observation is the feature vector sent to the RL service for one inference step.
// Field names/JSON tags match rl_service's observation construction — keep both sides in sync.
type Observation struct {
	InstID           string            `json:"inst_id"`
	MidPrice         decimal.Decimal   `json:"mid_price"`
	Position         decimal.Decimal   `json:"position"` // signed current position size
	CurrentLeverage  decimal.Decimal   `json:"current_leverage"`
	UnrealizedPnLPct decimal.Decimal   `json:"unrealized_pnl_pct"`
	EquityUSD        decimal.Decimal   `json:"equity_usd"`
	Features         []decimal.Decimal `json:"features"` // rolling window of engineered features
}

// Action is the RL agent's decision returned by the inference service.
type Action struct {
	TargetExposure decimal.Decimal `json:"target_exposure"` // in [-1, 1]
	LeverageFrac   decimal.Decimal `json:"leverage_frac"`   // in [0, 1], mapped to [1x, max_leverage]
	Confidence     decimal.Decimal `json:"confidence"`
}
