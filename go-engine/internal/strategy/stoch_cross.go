package strategy

import "github.com/shopspring/decimal"

// StochCross is a Go port of the Pine Script "How to use Leverage and Margin in PineScript" by
// Peter_O (pinescript/strategy_How to use Leverage and Margin in PineScript.pine, MPL 2.0). The
// title undersells it — the actual signal logic is a Stochastic %K/%D crossover strategy; the
// leverage/margin settings in the original strategy() header are backtester-account config, not
// signal logic, and aren't ported. Take-profit is a fixed tick distance in the original; here it's
// expressed as TPPct of entry price instead, since the domain has no "ticks" concept. No stop
// loss was specified in the source (exit is TP-only, position held otherwise).
type StochCross struct {
	PeriodK    int // %K lookback
	PeriodD    int // %D smoothing of %K
	SmoothK    int // %K smoothing
	TPPct      decimal.Decimal
	Overbought decimal.Decimal
	Oversold   decimal.Decimal
}

func NewStochCross() *StochCross {
	return &StochCross{
		PeriodK:    13,
		PeriodD:    3,
		SmoothK:    4,
		TPPct:      decimal.NewFromFloat(0.01),
		Overbought: decimal.NewFromInt(80),
		Oversold:   decimal.NewFromInt(20),
	}
}

func (s *StochCross) Name() string { return "stoch_cross" }

func (s *StochCross) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "period_k", Default: decimal.NewFromInt(int64(s.PeriodK)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "period_d", Default: decimal.NewFromInt(int64(s.PeriodD)), Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(50)},
		{Name: "smooth_k", Default: decimal.NewFromInt(int64(s.SmoothK)), Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(50)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
		{Name: "overbought", Default: s.Overbought, Min: decimal.NewFromInt(51), Max: decimal.NewFromInt(99)},
		{Name: "oversold", Default: s.Oversold, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(49)},
	}
}

func (s *StochCross) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["period_k"]; ok {
		cp.PeriodK = int(ClampParam(specByName["period_k"], v).IntPart())
	}
	if v, ok := values["period_d"]; ok {
		cp.PeriodD = int(ClampParam(specByName["period_d"], v).IntPart())
	}
	if v, ok := values["smooth_k"]; ok {
		cp.SmoothK = int(ClampParam(specByName["smooth_k"], v).IntPart())
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	if v, ok := values["overbought"]; ok {
		cp.Overbought = ClampParam(specByName["overbought"], v)
	}
	if v, ok := values["oversold"]; ok {
		cp.Oversold = ClampParam(specByName["oversold"], v)
	}
	return &cp
}

// rawStochK returns the raw %K (0..100) for every candle from index period-1 onward: where that
// candle's close sits within the trailing `period`-window high/low range.
func rawStochK(candles []Candle, period int) []decimal.Decimal {
	out := make([]decimal.Decimal, len(candles))
	for i := period - 1; i < len(candles); i++ {
		window := candles[i-period+1 : i+1]
		highest, lowest := window[0].High, window[0].Low
		for _, c := range window {
			if c.High.GreaterThan(highest) {
				highest = c.High
			}
			if c.Low.LessThan(lowest) {
				lowest = c.Low
			}
		}
		rng := highest.Sub(lowest)
		if rng.IsZero() {
			out[i] = decimal.NewFromInt(50)
			continue
		}
		out[i] = window[len(window)-1].Close.Sub(lowest).Div(rng).Mul(hundred)
	}
	return out
}

// smaSeries returns the trailing SMA of `series[:period-1]` as zero, then real values onward —
// same "valid from index period-1" convention as rawStochK, so the two compose directly.
func smaSeries(series []decimal.Decimal, period int) []decimal.Decimal {
	out := make([]decimal.Decimal, len(series))
	periodDec := decimal.NewFromInt(int64(period))
	for i := period - 1; i < len(series); i++ {
		sum := decimal.Zero
		for _, v := range series[i-period+1 : i+1] {
			sum = sum.Add(v)
		}
		out[i] = sum.Div(periodDec)
	}
	return out
}

func (s *StochCross) Evaluate(candles []Candle) (Signal, error) {
	need := s.PeriodK + s.SmoothK + s.PeriodD
	if len(candles) < need+1 {
		return Signal{Side: Hold}, nil
	}

	rawK := rawStochK(candles, s.PeriodK)
	kLine := smaSeries(rawK, s.SmoothK)
	dLine := smaSeries(kLine, s.PeriodD)

	last := len(candles) - 1
	kNow, kPrev := kLine[last], kLine[last-1]
	dNow, dPrev := dLine[last], dLine[last-1]

	crossedUp := kPrev.LessThanOrEqual(dPrev) && kNow.GreaterThan(dNow)
	crossedDown := kPrev.GreaterThanOrEqual(dPrev) && kNow.LessThan(dNow)

	switch {
	case crossedUp && kNow.LessThan(s.Overbought):
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.6), TPPct: s.TPPct}, nil
	case crossedDown && kNow.GreaterThan(s.Oversold):
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.6), TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
