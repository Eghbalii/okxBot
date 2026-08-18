package strategy

// RSISMA is a built-in strategy: buy when RSI is oversold and price is above the trend SMA
// (pullback-in-uptrend), sell when RSI is overbought and price is below the trend SMA.
type RSISMA struct {
	RSIPeriod     int
	SMAPeriod     int
	OversoldMax   float64
	OverboughtMin float64
	SLPct         float64
	TPPct         float64
}

// NewRSISMA creates an RSISMA strategy with sensible defaults for the given periods.
func NewRSISMA(rsiPeriod, smaPeriod int) *RSISMA {
	return &RSISMA{
		RSIPeriod:     rsiPeriod,
		SMAPeriod:     smaPeriod,
		OversoldMax:   30,
		OverboughtMin: 70,
		SLPct:         0.01,
		TPPct:         0.02,
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
	case rsi <= s.OversoldMax && price > sma:
		return Signal{Side: Buy, Confidence: (s.OversoldMax - rsi) / s.OversoldMax, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case rsi >= s.OverboughtMin && price < sma:
		return Signal{Side: Sell, Confidence: (rsi - s.OverboughtMin) / (100 - s.OverboughtMin), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
