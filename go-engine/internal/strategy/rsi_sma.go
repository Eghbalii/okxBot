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
