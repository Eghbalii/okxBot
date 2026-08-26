// Package strategy defines the pluggable signal-generator interface (CLAUDE.md §9) and a
// registry for built-in + configured strategies.
package strategy

import (
	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// Side is the suggested trade direction, or "" for no signal.
type Side string

const (
	Hold Side = ""
	Buy  Side = "buy"
	Sell Side = "sell"
)

// Signal is what a Strategy emits for one evaluation of an instrument's candle series.
type Signal struct {
	Side       Side
	Confidence decimal.Decimal // 0..1
	// SLPct/TPPct are suggested stop-loss/take-profit distance from entry, as a fraction of price.
	SLPct decimal.Decimal
	TPPct decimal.Decimal
}

// Candle is the OHLCV shape strategies evaluate over, oldest-first.
type Candle = domain.Candle

// ParamSpec describes one RL/panel-tunable numeric parameter of a Strategy: its name, default,
// and valid range. Registry/RL callers use this to discover what's tunable without hardcoding
// per-strategy knowledge, and to clamp any proposed value before it reaches WithParams.
type ParamSpec struct {
	Name    string
	Default decimal.Decimal
	Min     decimal.Decimal
	Max     decimal.Decimal
}

// Strategy evaluates a candle series for one instrument and returns a trade signal.
type Strategy interface {
	Name() string
	// Params declares this strategy's tunable numeric parameters (periods, thresholds, SL/TP %,
	// etc.), so callers (RL service, panel) can discover and propose values for them.
	Params() []ParamSpec
	// WithParams returns a copy of the strategy configured with the given param values, keyed by
	// ParamSpec.Name. Values for unrecognized or missing keys keep their current setting.
	WithParams(values map[string]decimal.Decimal) Strategy
	Evaluate(candles []Candle) (Signal, error)
}

// ClampParam clamps a proposed value to a ParamSpec's [Min, Max] range. Strategy.WithParams
// implementations should call this so the RL agent's proposals can never configure a strategy
// outside its declared valid range.
func ClampParam(spec ParamSpec, value decimal.Decimal) decimal.Decimal {
	if value.LessThan(spec.Min) {
		return spec.Min
	}
	if value.GreaterThan(spec.Max) {
		return spec.Max
	}
	return value
}

// paramsByName indexes a ParamSpec slice by name, for WithParams implementations that need to
// clamp several named values.
func paramsByName(specs []ParamSpec) map[string]ParamSpec {
	out := make(map[string]ParamSpec, len(specs))
	for _, spec := range specs {
		out[spec.Name] = spec
	}
	return out
}

// Registry holds the strategies currently enabled for a given instrument.
type Registry struct {
	byInstID map[string][]Strategy
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byInstID: make(map[string][]Strategy)}
}

// Register assigns a strategy to an instrument.
func (r *Registry) Register(instID string, s Strategy) {
	r.byInstID[instID] = append(r.byInstID[instID], s)
}

// For returns the strategies assigned to an instrument.
func (r *Registry) For(instID string) []Strategy {
	return r.byInstID[instID]
}
