package strategy

import "github.com/shopspring/decimal"

// BBBreakout is a Go port of the widely-known TradingView "Bollinger Bands Breakout Strategy"
// family (raw PineScript source not extractable via automated fetch; implemented from the
// standard, well-documented Bollinger Bands construction).
//
// Standard/default parameters: BB(20, 2) — John Bollinger's own published defaults (20-period SMA
// basis, 2 standard deviation bands).
//
// Signal logic: buy when a candle CLOSES above the upper band (a genuine breakout, not merely a
// touch); sell when a candle closes below the lower band. Stop at the band basis (SMA) — the
// classic "if price falls back inside the bands the breakout failed" invalidation level — target at
// RiskReward multiples of that risk.
type BBBreakout struct {
	Period     int
	Mult       decimal.Decimal
	RiskReward decimal.Decimal
}

func NewBBBreakout() *BBBreakout {
	return &BBBreakout{
		Period:     20,
		Mult:       decimal.NewFromInt(2),
		RiskReward: decimal.NewFromFloat(1.5),
	}
}

func (s *BBBreakout) Name() string { return "bb_breakout" }

func (s *BBBreakout) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "period", Default: decimal.NewFromInt(int64(s.Period)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(200)},
		{Name: "mult", Default: s.Mult, Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromInt(5)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *BBBreakout) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["period"]; ok {
		cp.Period = int(ClampParam(spec["period"], v).IntPart())
	}
	if v, ok := values["mult"]; ok {
		cp.Mult = ClampParam(spec["mult"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

func (s *BBBreakout) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.Period+1 {
		return Signal{Side: Hold}, nil
	}
	basis, upper, lower, err := BollingerBands(candles, s.Period, s.Mult)
	if err != nil {
		return Signal{}, err
	}
	last := candles[len(candles)-1]

	switch {
	case last.Close.GreaterThan(upper):
		risk := last.Close.Sub(basis)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       basis,
			TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	case last.Close.LessThan(lower):
		risk := basis.Sub(last.Close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       basis,
			TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
