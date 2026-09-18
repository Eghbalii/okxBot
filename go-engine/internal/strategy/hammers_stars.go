package strategy

import "github.com/shopspring/decimal"

// HammersStars is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/hammers_stars.pine, @version=6 "Hammers & Stars Strategy [v1.2]" by
// ZenAndTheArtOfTrading), replacing the first pass's from-description implementation (2026-09-16),
// which used a body/range ratio plus a wick/body ratio and an always-on EMA trend filter — the real
// source instead uses a "Fibonacci level of the candle" threshold, an ATR-based stop derived from
// the LOW/HIGH structure (not the pattern candle's own extreme with no ATR involvement), and its EMA
// filter is off by default.
//
// Real parameters, read directly from the source:
//   - ATR(14) size filter: `>= 0.0` / `<= 3.0` x ATR (both effectively disabled at these defaults —
//     0 min means "no minimum", 3.0 max rarely binds), so the candle-size gate is a no-op by
//     default; not modeled here since the default leaves it inert.
//   - `fibLevel` = 0.333: `bullFib = (low-high)*fibLevel + high`, `bearFib = (high-low)*fibLevel +
//     low` — the level 33.3% of the way down from the high (bullish) / up from the low (bearish).
//   - `emaFilter` = 0 (disabled by default — "set to zero to disable").
//   - `stopMultiplier` = 1.0 (x ATR), `rr` = 1.0.
//
// Signal logic, exactly the source's own conditions: a HAMMER requires the candle's lower body edge
// (`lowestBody = min(open, close)`) to sit AT OR ABOVE `bullFib` (i.e. the body closed in the upper
// ~66.7% of the candle's range) AND `close != open`. A STAR (shooting star) requires the upper body
// edge (`highestBody = max(open, close)`) to sit AT OR BELOW `bearFib` (body closed in the lower
// ~66.7% of the range). Stop: `longStopPrice = low < low[1] ? low - ATR : low[1] - ATR` (the lower
// of the current/prior low, minus one ATR) for a hammer; mirrored with highs for a star. Target =
// entry +/- stopDistance * rr. The source has no trend-context requirement of its own beyond the
// (default-disabled) EMA filter — a bare hammer/star pattern is tradeable on its own, unlike the
// first pass's invented "must follow an opposite trend" gate.
type HammersStars struct {
	ATRPeriod    int
	FibLevel     decimal.Decimal
	StopMult     decimal.Decimal
	RiskReward   decimal.Decimal
}

func NewHammersStars() *HammersStars {
	return &HammersStars{
		ATRPeriod:  14,
		FibLevel:   decimal.NewFromFloat(0.333),
		StopMult:   decimal.NewFromInt(1),
		RiskReward: decimal.NewFromInt(1),
	}
}

func (s *HammersStars) Name() string { return "hammers_stars" }

func (s *HammersStars) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "fib_level", Default: s.FibLevel, Min: decimal.NewFromFloat(0.05), Max: decimal.NewFromFloat(0.9)},
		{Name: "stop_mult", Default: s.StopMult, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromInt(10)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *HammersStars) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["fib_level"]; ok {
		cp.FibLevel = ClampParam(spec["fib_level"], v)
	}
	if v, ok := values["stop_mult"]; ok {
		cp.StopMult = ClampParam(spec["stop_mult"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

func (s *HammersStars) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.ATRPeriod+2 {
		return Signal{Side: Hold}, nil
	}
	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	last := candles[len(candles)-1]
	prev := candles[len(candles)-2]

	if last.Close.Equal(last.Open) {
		return Signal{Side: Hold}, nil
	}

	bullFib := last.Low.Sub(last.High).Mul(s.FibLevel).Add(last.High)
	bearFib := last.High.Sub(last.Low).Mul(s.FibLevel).Add(last.Low)

	lowestBody := decimal.Min(last.Open, last.Close)
	highestBody := decimal.Max(last.Open, last.Close)

	validHammer := lowestBody.GreaterThanOrEqual(bullFib)
	validStar := highestBody.LessThanOrEqual(bearFib)

	stopSize := atr.Mul(s.StopMult)

	switch {
	case validHammer:
		longStopPrice := prev.Low.Sub(stopSize)
		if last.Low.LessThan(prev.Low) {
			longStopPrice = last.Low.Sub(stopSize)
		}
		risk := last.Close.Sub(longStopPrice)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       longStopPrice,
			TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	case validStar:
		shortStopPrice := prev.High.Add(stopSize)
		if last.High.GreaterThan(prev.High) {
			shortStopPrice = last.High.Add(stopSize)
		}
		risk := shortStopPrice.Sub(last.Close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       shortStopPrice,
			TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
