package strategy

import "github.com/shopspring/decimal"

// DualMAATR is a Go port of the core signal logic from "3Commas Bot" ("Bj Bot")
// (pinescript/strategy_3Commas Bot.pine, no stated license). The original supports 8 MA types
// and an HEMA (Heikin-Ashi EMA)/T3/DEMA family for both MAs, plus 3Commas webhook alert wiring,
// an ATR trailing-stop mode, a time-session filter, and a max-drawdown circuit breaker — none of
// that is signal logic and none of it is ported (webhooks/alerts are an execution-integration
// concern, not a Strategy; max-drawdown belongs in internal/risk per CLAUDE.md §5, not per-
// strategy). What's ported: a fast/slow MA crossover entry (EMA/SMA/WMA only, same constraint as
// PMax), with stop/target computed from the swing high/low over SwingLookback bars adjusted by
// ATR*RiskMultiplier, and target placed at RiskReward multiples of that risk — the original's
// "Risk Adjustment"/"Reward to Risk Ratio" mechanism.
type DualMAATR struct {
	MAType         string // "SMA", "EMA", or "WMA"
	FastLength     int
	SlowLength     int
	ATRLength      int
	SwingLookback  int
	RiskMultiplier decimal.Decimal // ATR multiplier added to the swing high/low for the stop
	RiskReward     decimal.Decimal // target = RiskReward * (entry - stop) distance

	prevFast, prevSlow decimal.Decimal
	hasPrev            bool
}

func NewDualMAATR() *DualMAATR {
	return &DualMAATR{
		MAType:         "EMA",
		FastLength:     21,
		SlowLength:     50,
		ATRLength:      14,
		SwingLookback:  5,
		RiskMultiplier: decimal.NewFromInt(1),
		RiskReward:     decimal.NewFromInt(1),
	}
}

func (s *DualMAATR) Name() string { return "dual_ma_atr" }

func (s *DualMAATR) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "fast_length", Default: decimal.NewFromInt(int64(s.FastLength)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(200)},
		{Name: "slow_length", Default: decimal.NewFromInt(int64(s.SlowLength)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(400)},
		{Name: "atr_length", Default: decimal.NewFromInt(int64(s.ATRLength)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "swing_lookback", Default: decimal.NewFromInt(int64(s.SwingLookback)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "risk_multiplier", Default: s.RiskMultiplier, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromInt(10)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromInt(10)},
	}
}

func (s *DualMAATR) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["fast_length"]; ok {
		cp.FastLength = int(ClampParam(specByName["fast_length"], v).IntPart())
	}
	if v, ok := values["slow_length"]; ok {
		cp.SlowLength = int(ClampParam(specByName["slow_length"], v).IntPart())
	}
	if v, ok := values["atr_length"]; ok {
		cp.ATRLength = int(ClampParam(specByName["atr_length"], v).IntPart())
	}
	if v, ok := values["swing_lookback"]; ok {
		cp.SwingLookback = int(ClampParam(specByName["swing_lookback"], v).IntPart())
	}
	if v, ok := values["risk_multiplier"]; ok {
		cp.RiskMultiplier = ClampParam(specByName["risk_multiplier"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	return &cp
}

func (s *DualMAATR) movingAverage(candles []Candle, period int) (decimal.Decimal, error) {
	switch s.MAType {
	case "SMA":
		return SMA(candles, period)
	case "WMA":
		return wma(candles, period)
	default:
		return EMA(candles, period)
	}
}

func (s *DualMAATR) Evaluate(candles []Candle) (Signal, error) {
	need := s.SlowLength
	if s.ATRLength+1 > need {
		need = s.ATRLength + 1
	}
	if s.SwingLookback > need {
		need = s.SwingLookback
	}
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	fastNow, err := s.movingAverage(candles, s.FastLength)
	if err != nil {
		return Signal{}, err
	}
	slowNow, err := s.movingAverage(candles, s.SlowLength)
	if err != nil {
		return Signal{}, err
	}

	if !s.hasPrev {
		s.prevFast, s.prevSlow, s.hasPrev = fastNow, slowNow, true
		return Signal{Side: Hold}, nil
	}
	prevFast, prevSlow := s.prevFast, s.prevSlow
	s.prevFast, s.prevSlow = fastNow, slowNow

	crossedUp := prevFast.LessThanOrEqual(prevSlow) && fastNow.GreaterThan(slowNow)
	crossedDown := prevFast.GreaterThanOrEqual(prevSlow) && fastNow.LessThan(slowNow)
	if !crossedUp && !crossedDown {
		return Signal{Side: Hold}, nil
	}

	atr, err := ATR(candles, s.ATRLength)
	if err != nil {
		return Signal{}, err
	}
	close := candles[len(candles)-1].Close

	if crossedUp {
		lowestLow, err := Lowest(candles, s.SwingLookback)
		if err != nil {
			return Signal{}, err
		}
		stop := lowestLow.Sub(atr.Mul(s.RiskMultiplier))
		risk := close.Sub(stop)
		if risk.IsPositive() {
			slPct := risk.Div(close)
			tpPct := slPct.Mul(s.RiskReward)
			return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.5), SLPct: slPct, TPPct: tpPct}, nil
		}
		return Signal{Side: Hold}, nil
	}

	highestHigh, err := Highest(candles, s.SwingLookback)
	if err != nil {
		return Signal{}, err
	}
	stop := highestHigh.Add(atr.Mul(s.RiskMultiplier))
	risk := stop.Sub(close)
	if risk.IsPositive() {
		slPct := risk.Div(close)
		tpPct := slPct.Mul(s.RiskReward)
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.5), SLPct: slPct, TPPct: tpPct}, nil
	}
	return Signal{Side: Hold}, nil
}
