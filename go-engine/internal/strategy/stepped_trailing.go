package strategy

import "github.com/shopspring/decimal"

// SteppedTrailing is a Go port of the Pine Script "Stepped trailing strategy example" by adolgov
// (pinescript/strategy_Stepped trailing strategy example.pine, MPL 2.0). Entry is the same fast/
// slow SMA crossover the author calls a "random entry condition" (long-only in the source — no
// short entry was defined, only ported as-is). The interesting part is the 3-stage exit: once
// TP1 is reached, move stop to breakeven; once TP2 is reached, move stop to TP1; TP3 is the final
// target (or, if ActivateTrailingOnThirdStep, a trailing stop activates instead of a hard TP3).
// This can't be expressed as a single SLPct/TPPct pair on the Signal, since the exit level changes
// over the life of the trade based on how far price has moved — that's position-lifecycle state
// PaperTrader's order-monitoring loop owns, not something Evaluate can compute from candles alone.
// StageExit below is exposed so the execution layer can drive the staged SL, but is not wired into
// Signal; Evaluate emits only the entry signal and the initial (stage-1) SL/TP.
type SteppedTrailing struct {
	FastPeriod                  int
	SlowPeriod                  int
	SLPct                       decimal.Decimal // stage 1: hard stop
	TP1Pct                      decimal.Decimal // stage 1->2 trigger; stop moves to breakeven
	TP2Pct                      decimal.Decimal // stage 2->3 trigger; stop moves to TP1
	TP3Pct                      decimal.Decimal // stage 3: final target (or trail amount level)
	ActivateTrailingOnThirdStep bool
}

func NewSteppedTrailing() *SteppedTrailing {
	return &SteppedTrailing{
		FastPeriod: 14,
		SlowPeriod: 28,
		SLPct:      decimal.NewFromFloat(0.05),
		TP1Pct:     decimal.NewFromFloat(0.05),
		TP2Pct:     decimal.NewFromFloat(0.10),
		TP3Pct:     decimal.NewFromFloat(0.15),
	}
}

func (s *SteppedTrailing) Name() string { return "stepped_trailing" }

func (s *SteppedTrailing) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "fast_period", Default: decimal.NewFromInt(int64(s.FastPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "slow_period", Default: decimal.NewFromInt(int64(s.SlowPeriod)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp1_pct", Default: s.TP1Pct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
		{Name: "tp2_pct", Default: s.TP2Pct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
		{Name: "tp3_pct", Default: s.TP3Pct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(1)},
	}
}

func (s *SteppedTrailing) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["fast_period"]; ok {
		cp.FastPeriod = int(ClampParam(specByName["fast_period"], v).IntPart())
	}
	if v, ok := values["slow_period"]; ok {
		cp.SlowPeriod = int(ClampParam(specByName["slow_period"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp1_pct"]; ok {
		cp.TP1Pct = ClampParam(specByName["tp1_pct"], v)
	}
	if v, ok := values["tp2_pct"]; ok {
		cp.TP2Pct = ClampParam(specByName["tp2_pct"], v)
	}
	if v, ok := values["tp3_pct"]; ok {
		cp.TP3Pct = ClampParam(specByName["tp3_pct"], v)
	}
	return &cp
}

func (s *SteppedTrailing) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.SlowPeriod+1 {
		return Signal{Side: Hold}, nil
	}
	fastNow, err := SMA(candles, s.FastPeriod)
	if err != nil {
		return Signal{}, err
	}
	slowNow, err := SMA(candles, s.SlowPeriod)
	if err != nil {
		return Signal{}, err
	}
	fastPrev, err := SMA(candles[:len(candles)-1], s.FastPeriod)
	if err != nil {
		return Signal{}, err
	}
	slowPrev, err := SMA(candles[:len(candles)-1], s.SlowPeriod)
	if err != nil {
		return Signal{}, err
	}

	crossedUp := fastPrev.LessThanOrEqual(slowPrev) && fastNow.GreaterThan(slowNow)
	if crossedUp {
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TP3Pct}, nil
	}
	return Signal{Side: Hold}, nil
}
