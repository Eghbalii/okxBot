package strategy

import "github.com/shopspring/decimal"

// RSISMAFuzzy is a fuzzy-logic variant of RSISMA (CLAUDE.md §16.5): instead of a hard threshold
// crossing (RSI <= OversoldMax is Buy, RSI > OversoldMax is Hold — a cliff at the boundary where
// RSI=29.99 and RSI=30.01 produce completely different decisions), membership in "oversold"/
// "overbought" is a smooth trapezoidal degree between 0 and 1. This directly addresses "nothing in
// real markets is a hard 0/1 boolean" — the transition zone (OversoldMax..OversoldFullMax) is where
// membership ramps from 1 down to 0, rather than the signal cutting off entirely at one point.
// A separate strategy.Strategy kind from RSISMA, not a modification to it — origins stay locked
// and independently comparable (CLAUDE.md §11.3).
type RSISMAFuzzy struct {
	RSIPeriod int
	SMAPeriod int

	// OversoldMin/OversoldMax bound the "fully oversold" plateau (membership = 1 at/below
	// OversoldMin, ramping down to 0 at OversoldMax). OversoldMin must stay below OversoldMax —
	// WithParams enforces this by clamping OversoldMin no higher than OversoldMax-1.
	OversoldMin decimal.Decimal
	OversoldMax decimal.Decimal
	// OverboughtMin/OverboughtMax mirror the above for the overbought side: membership ramps from
	// 0 at OverboughtMin up to 1 at/above OverboughtMax.
	OverboughtMin decimal.Decimal
	OverboughtMax decimal.Decimal

	// MinMembership is the fuzzy-membership floor below which no signal is emitted at all (Hold) —
	// without this, the strategy would emit a near-zero-confidence signal for every single candle,
	// which is not useful (a "signal" with 0.01 confidence is noise, not a trade opportunity).
	MinMembership decimal.Decimal

	SLPct decimal.Decimal
	TPPct decimal.Decimal
}

// NewRSISMAFuzzy creates an RSISMAFuzzy with a transition zone straddling the classic 30/70
// oversold/overbought thresholds.
func NewRSISMAFuzzy() *RSISMAFuzzy {
	return &RSISMAFuzzy{
		RSIPeriod:     14,
		SMAPeriod:     50,
		OversoldMin:   decimal.NewFromInt(20),
		OversoldMax:   decimal.NewFromInt(35),
		OverboughtMin: decimal.NewFromInt(65),
		OverboughtMax: decimal.NewFromInt(80),
		MinMembership: decimal.NewFromFloat(0.1),
		SLPct:         decimal.NewFromFloat(0.01),
		TPPct:         decimal.NewFromFloat(0.02),
	}
}

func (s *RSISMAFuzzy) Name() string { return "rsi_sma_fuzzy" }

func (s *RSISMAFuzzy) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "rsi_period", Default: decimal.NewFromInt(int64(s.RSIPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "sma_period", Default: decimal.NewFromInt(int64(s.SMAPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(300)},
		{Name: "oversold_min", Default: s.OversoldMin, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(48)},
		{Name: "oversold_max", Default: s.OversoldMax, Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(49)},
		{Name: "overbought_min", Default: s.OverboughtMin, Min: decimal.NewFromInt(51), Max: decimal.NewFromInt(98)},
		{Name: "overbought_max", Default: s.OverboughtMax, Min: decimal.NewFromInt(52), Max: decimal.NewFromInt(99)},
		{Name: "min_membership", Default: s.MinMembership, Min: decimal.Zero, Max: decimal.NewFromFloat(0.9)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *RSISMAFuzzy) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["rsi_period"]; ok {
		cp.RSIPeriod = int(ClampParam(specByName["rsi_period"], v).IntPart())
	}
	if v, ok := values["sma_period"]; ok {
		cp.SMAPeriod = int(ClampParam(specByName["sma_period"], v).IntPart())
	}
	if v, ok := values["oversold_min"]; ok {
		cp.OversoldMin = ClampParam(specByName["oversold_min"], v)
	}
	if v, ok := values["oversold_max"]; ok {
		cp.OversoldMax = ClampParam(specByName["oversold_max"], v)
	}
	if v, ok := values["overbought_min"]; ok {
		cp.OverboughtMin = ClampParam(specByName["overbought_min"], v)
	}
	if v, ok := values["overbought_max"]; ok {
		cp.OverboughtMax = ClampParam(specByName["overbought_max"], v)
	}
	if v, ok := values["min_membership"]; ok {
		cp.MinMembership = ClampParam(specByName["min_membership"], v)
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	// Keep OversoldMin < OversoldMax and OverboughtMin < OverboughtMax regardless of which
	// individual param changed — an inverted or degenerate zone would make membership() divide by
	// a non-positive range.
	if !cp.OversoldMin.LessThan(cp.OversoldMax) {
		cp.OversoldMin = cp.OversoldMax.Sub(decimal.NewFromInt(1))
	}
	if !cp.OverboughtMin.LessThan(cp.OverboughtMax) {
		cp.OverboughtMax = cp.OverboughtMin.Add(decimal.NewFromInt(1))
	}
	return &cp
}

// oversoldMembership returns the fuzzy degree (0..1) to which rsi belongs to "oversold": 1 at or
// below OversoldMin, linearly ramping to 0 at OversoldMax, 0 above it. A standard trapezoidal
// membership function — no hard cliff anywhere in the transition zone.
func (s *RSISMAFuzzy) oversoldMembership(rsi decimal.Decimal) decimal.Decimal {
	if rsi.LessThanOrEqual(s.OversoldMin) {
		return decimal.NewFromInt(1)
	}
	if rsi.GreaterThanOrEqual(s.OversoldMax) {
		return decimal.Zero
	}
	span := s.OversoldMax.Sub(s.OversoldMin)
	return s.OversoldMax.Sub(rsi).Div(span)
}

// overboughtMembership mirrors oversoldMembership for the overbought side.
func (s *RSISMAFuzzy) overboughtMembership(rsi decimal.Decimal) decimal.Decimal {
	if rsi.GreaterThanOrEqual(s.OverboughtMax) {
		return decimal.NewFromInt(1)
	}
	if rsi.LessThanOrEqual(s.OverboughtMin) {
		return decimal.Zero
	}
	span := s.OverboughtMax.Sub(s.OverboughtMin)
	return rsi.Sub(s.OverboughtMin).Div(span)
}

func (s *RSISMAFuzzy) Evaluate(candles []Candle) (Signal, error) {
	rsi, err := RSI(candles, s.RSIPeriod)
	if err != nil {
		return Signal{}, err
	}
	sma, err := SMA(candles, s.SMAPeriod)
	if err != nil {
		return Signal{}, err
	}
	price := candles[len(candles)-1].Close

	oversold := s.oversoldMembership(rsi)
	overbought := s.overboughtMembership(rsi)

	switch {
	case oversold.GreaterThanOrEqual(s.MinMembership) && price.GreaterThan(sma):
		return Signal{Side: Buy, Confidence: oversold, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case overbought.GreaterThanOrEqual(s.MinMembership) && price.LessThan(sma):
		return Signal{Side: Sell, Confidence: overbought, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
