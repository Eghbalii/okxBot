// Package risk enforces hard trading limits that are independent of, and cannot be overridden
// by, the RL model's output. This is the last line of defense against a bad model action.
package risk

import "fmt"

// Limits holds the configured hard risk limits.
type Limits struct {
	MaxLeverage             float64
	MaxPositionNotionalUSD  float64
	MaxDailyDrawdownPct     float64
	MinLiquidationBufferPct float64
}

// Manager evaluates proposed trading actions against hard limits and tracks daily PnL for the
// drawdown circuit breaker.
type Manager struct {
	limits         Limits
	dayStartEquity float64
	halted         bool
	haltReason     string
}

// NewManager creates a risk Manager. dayStartEquity is the account equity at the start of the
// current trading day, used as the baseline for the drawdown circuit breaker.
func NewManager(limits Limits, dayStartEquity float64) *Manager {
	return &Manager{limits: limits, dayStartEquity: dayStartEquity}
}

// ProposedAction is the RL-suggested (or any) trading action awaiting risk approval.
type ProposedAction struct {
	Leverage            float64
	PositionNotionalUSD float64
	// LiquidationBufferPct is the distance between mark price and estimated liquidation price,
	// expressed as a percentage of mark price, if this action were taken.
	LiquidationBufferPct float64
}

// Halted reports whether the circuit breaker has tripped and trading should stop until reset.
func (m *Manager) Halted() (bool, string) {
	return m.halted, m.haltReason
}

// Reset clears the halted state (e.g. after manual review or at the start of a new trading day).
func (m *Manager) Reset(dayStartEquity float64) {
	m.halted = false
	m.haltReason = ""
	m.dayStartEquity = dayStartEquity
}

// CheckDrawdown updates the circuit breaker based on current equity. Call this on every
// account-state refresh, independent of whether a new action is being evaluated.
func (m *Manager) CheckDrawdown(currentEquity float64) {
	if m.dayStartEquity <= 0 {
		return
	}
	drawdownPct := (m.dayStartEquity - currentEquity) / m.dayStartEquity * 100
	if drawdownPct >= m.limits.MaxDailyDrawdownPct {
		m.halted = true
		m.haltReason = fmt.Sprintf("daily drawdown %.2f%% >= limit %.2f%%", drawdownPct, m.limits.MaxDailyDrawdownPct)
	}
}

// Approve clamps/validates a proposed action against hard limits. It returns the (possibly
// clamped) action and an error if the action must be rejected outright rather than clamped.
func (m *Manager) Approve(action ProposedAction) (ProposedAction, error) {
	if halted, reason := m.Halted(); halted {
		return action, fmt.Errorf("trading halted: %s", reason)
	}

	if action.Leverage > m.limits.MaxLeverage {
		action.Leverage = m.limits.MaxLeverage
	}
	if action.PositionNotionalUSD > m.limits.MaxPositionNotionalUSD {
		action.PositionNotionalUSD = m.limits.MaxPositionNotionalUSD
	}
	if action.LiquidationBufferPct < m.limits.MinLiquidationBufferPct {
		return action, fmt.Errorf(
			"rejected: liquidation buffer %.2f%% below minimum %.2f%%",
			action.LiquidationBufferPct, m.limits.MinLiquidationBufferPct,
		)
	}
	return action, nil
}
