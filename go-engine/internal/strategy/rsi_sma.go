package strategy

import "github.com/shopspring/decimal"

var (
	hundred = decimal.NewFromInt(100)
)

// RSISMA is a built-in strategy: buy when RSI is oversold and price is above the trend SMA
// (pullback-in-uptrend), sell when RSI is overbought and price is below the trend SMA.
type RSISMA struct {
	RSIPeriod     int
	SMAPeriod     int
	OversoldMax   decimal.Decimal
	OverboughtMin decimal.Decimal
	SLPct         decimal.Decimal
	TPPct         decimal.Decimal
}

// NewRSISMA creates an RSISMA strategy with sensible defaults for the given periods.
func NewRSISMA(rsiPeriod, smaPeriod int) *RSISMA {
	return &RSISMA{
		RSIPeriod:     rsiPeriod,
		SMAPeriod:     smaPeriod,
		OversoldMax:   decimal.NewFromInt(30),
		OverboughtMin: decimal.NewFromInt(70),
		SLPct:         decimal.NewFromFloat(0.01),
		TPPct:         decimal.NewFromFloat(0.02),
	}
}

func (s *RSISMA) Name() string { return "rsi_sma" }

func (s *RSISMA) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "rsi_period", Default: decimal.NewFromInt(int64(s.RSIPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "sma_period", Default: decimal.NewFromInt(int64(s.SMAPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(300)},
		{Name: "oversold_max", Default: s.OversoldMax, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(49)},
		{Name: "overbought_min", Default: s.OverboughtMin, Min: decimal.NewFromInt(51), Max: decimal.NewFromInt(99)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *RSISMA) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["rsi_period"]; ok {
		cp.RSIPeriod = int(ClampParam(specByName["rsi_period"], v).IntPart())
	}
	if v, ok := values["sma_period"]; ok {
		cp.SMAPeriod = int(ClampParam(specByName["sma_period"], v).IntPart())
	}
	if v, ok := values["oversold_max"]; ok {
		cp.OversoldMax = ClampParam(specByName["oversold_max"], v)
	}
	if v, ok := values["overbought_min"]; ok {
		cp.OverboughtMin = ClampParam(specByName["overbought_min"], v)
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	return &cp
}

func (s *RSISMA) Evaluate(candles []Candle) (Signal, error) {
	rsi, err := RSI(candles, s.RSIPeriod)
	if err != nil {
		return Signal{}, err
	}
	sma, err := SMA(candles, s.SMAPeriod)
	if err != nil {
		return Signal{}, err
	}
	price := candles[len(candles)-1].Close

	switch {
	case rsi.LessThanOrEqual(s.OversoldMax) && price.GreaterThan(sma):
		confidence := s.OversoldMax.Sub(rsi).Div(s.OversoldMax)
		return Signal{Side: Buy, Confidence: confidence, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case rsi.GreaterThanOrEqual(s.OverboughtMin) && price.LessThan(sma):
		confidence := rsi.Sub(s.OverboughtMin).Div(hundred.Sub(s.OverboughtMin))
		return Signal{Side: Sell, Confidence: confidence, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
