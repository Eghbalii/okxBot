// Package strategy defines the pluggable signal-generator interface (CLAUDE.md §9) and a
// registry for built-in + configured strategies.
package strategy

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
	Confidence float64 // 0..1
	// SLPct/TPPct are suggested stop-loss/take-profit distance from entry, as a fraction of price.
	SLPct float64
	TPPct float64
}

// Candle is the minimal OHLCV shape strategies evaluate over, oldest-first.
type Candle struct {
	Open, High, Low, Close, Volume float64
}

// Strategy evaluates a candle series for one instrument and returns a trade signal.
type Strategy interface {
	Name() string
	Evaluate(candles []Candle) (Signal, error)
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
