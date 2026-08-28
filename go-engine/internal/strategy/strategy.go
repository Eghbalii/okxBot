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
//
// SL/TP can be expressed two ways, and both are supported on purpose (CLAUDE.md §15.11):
//
//   - **Price levels** (EntryPx/SLPx/TPPx) — what a strategy reading chart structure actually
//     produces: a stop below a swing low, a target at a fair-value gap. This is the richer form,
//     since a level carries information a percentage cannot.
//   - **Percentages** (SLPct/TPPct) — a configured distance from entry, which is what most of the
//     built-in strategies currently use.
//
// A strategy sets whichever it genuinely computes; ResolveLevels fills in the other from the live
// price so downstream code always has prices. Converting a real level *into* a percentage would
// discard the structure that produced it, which is why the price fields are primary and the
// percentages are the fallback rather than the reverse.
type Signal struct {
	Side       Side
	Confidence decimal.Decimal // 0..1

	// SLPct/TPPct are suggested stop-loss/take-profit distance from entry, as a fraction of price.
	SLPct decimal.Decimal
	TPPct decimal.Decimal

	// EntryPx/SLPx/TPPx are explicit price levels. Zero means "not specified" — ResolveLevels then
	// derives them from the percentages above. Auditing which strategies can populate these
	// directly is tracked as its own task (CLAUDE.md §14).
	EntryPx decimal.Decimal
	SLPx    decimal.Decimal
	TPPx    decimal.Decimal
}

// ResolveLevels returns a copy of s with EntryPx/SLPx/TPPx populated, deriving any that the
// strategy left unset from its percentage fields against price. Levels the strategy set explicitly
// are never overwritten — a strategy that read a real level off the chart keeps it.
//
// Direction matters: a long's stop sits below entry and its target above, and a short's is
// mirrored, so the sign is taken from the signal's own side rather than assumed.
func (s Signal) ResolveLevels(price decimal.Decimal) Signal {
	if !price.IsPositive() || s.Side == Hold {
		return s
	}

	if !s.EntryPx.IsPositive() {
		s.EntryPx = price
	}

	direction := decimal.NewFromInt(1)
	if s.Side == Sell {
		direction = decimal.NewFromInt(-1)
	}

	if !s.SLPx.IsPositive() && s.SLPct.IsPositive() {
		s.SLPx = s.EntryPx.Sub(direction.Mul(s.SLPct).Mul(s.EntryPx))
	}
	if !s.TPPx.IsPositive() && s.TPPct.IsPositive() {
		s.TPPx = s.EntryPx.Add(direction.Mul(s.TPPct).Mul(s.EntryPx))
	}
	return s
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
	// Evaluate reads the candle window of the timeframe this strategy is assigned to. A strategy
	// that also wants higher-timeframe context implements MultiTimeframeStrategy below instead of
	// changing this signature.
	Evaluate(candles []Candle) (Signal, error)
}

// MarketView is the multi-timeframe read handed to a MultiTimeframeStrategy (CLAUDE.md §9): the
// bar the strategy is assigned to (its decision cadence), that bar's own window, and every other
// timeframe the ingestor is collecting for this instrument.
//
// The point is that trading a 5m signal while confirming trend on 1h/4h is normal discretionary
// practice, and a strategy restricted to one window structurally cannot express it. Which
// timeframes a strategy *decides* on stays separate from which it may *look at*.
type MarketView struct {
	// Bar is the timeframe whose candle just closed — the strategy's decision cadence.
	Bar string
	// Candles is Bars[Bar]: the assigned timeframe's window, identical to what Evaluate receives.
	Candles []Candle
	// Bars holds every timeframe currently maintained for this instrument, keyed by bar
	// ("5m", "15m", "1H", ...). Read-only: the map and its slices are a snapshot the caller owns.
	// A requested bar may be absent or too short to compute an indicator over — Higher() returns
	// ok=false rather than a partial window, and a strategy must degrade gracefully when it does
	// (the engine keeps running through a warm-up period where longer timeframes are still filling).
	Bars map[string][]Candle
}

// Higher returns another timeframe's window, reporting ok=false when that bar isn't being
// collected or hasn't accumulated at least minLen candles yet. Callers should treat !ok as "no
// higher-timeframe opinion available right now" and fall back to their single-timeframe logic,
// never as an error — during warm-up this is the normal case, not a failure.
func (v MarketView) Higher(bar string, minLen int) ([]Candle, bool) {
	c, ok := v.Bars[bar]
	if !ok || len(c) < minLen {
		return nil, false
	}
	return c, true
}

// MultiTimeframeStrategy is the OPTIONAL capability a Strategy implements when it wants context
// from timeframes other than the one it's assigned to. The engine type-asserts for it and calls
// EvaluateView instead of Evaluate; a strategy that doesn't implement it is unaffected, which is
// why this is a separate interface rather than a change to Strategy's own signature (14 built-in
// strategies would otherwise all need editing to gain a parameter most of them ignore).
type MultiTimeframeStrategy interface {
	Strategy
	EvaluateView(view MarketView) (Signal, error)
}

// EvaluateWith runs s against view, using its multi-timeframe path when it has one and falling
// back to plain Evaluate otherwise. Every engine call site goes through this, so adding
// higher-timeframe awareness to a strategy needs no change at the call sites.
func EvaluateWith(s Strategy, view MarketView) (Signal, error) {
	if mt, ok := s.(MultiTimeframeStrategy); ok {
		return mt.EvaluateView(view)
	}
	return s.Evaluate(view.Candles)
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
