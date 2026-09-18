package strategy

import "github.com/shopspring/decimal"

// UTBot is a Go port of "UT Bot Alerts v5" by Yo_adriiiiaan (a widely-republished derivative of
// the original UT Bot by HPotter), one of TradingView's most copied ATR-trailing-stop scripts.
// Raw PineScript source was not extractable via automated fetch, so this is implemented from the
// indicator's well-known, widely-documented algorithm.
//
// Standard/default parameters, per UT Bot's own published defaults:
//   - Key Value (ATR multiplier) = 1 — UT Bot's published "sensitivity" default.
//   - ATR Period = 10 — UT Bot's published default.
//
// Algorithm: an ATR-based trailing stop line ("xATRTrailingStop") that ratchets in the direction of
// the trend — the same Chandelier-style construction as pmax.go, but flipped on a simple
// close-vs-stop crossover rather than a moving-average-vs-band comparison. Buy when price closes
// above the trailing stop after having been below it (and, per the original's own "EMA(1)" smoothing
// of price — which is just price itself since EMA of length 1 has no smoothing effect — crosses
// above the stop); sell on the mirrored cross below.
type UTBot struct {
	ATRPeriod int
	KeyValue  decimal.Decimal // ATR multiplier

	prevStop    decimal.Decimal
	prevClose   decimal.Decimal
	prevUptrend bool
	hasPrev     bool
}

func NewUTBot() *UTBot {
	return &UTBot{
		ATRPeriod: 10,
		KeyValue:  decimal.NewFromInt(1),
	}
}

func (s *UTBot) Name() string { return "ut_bot" }

func (s *UTBot) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "key_value", Default: s.KeyValue, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromInt(10)},
	}
}

func (s *UTBot) resetState() {
	s.prevStop, s.prevClose = decimal.Zero, decimal.Zero
	s.prevUptrend, s.hasPrev = false, false
}

func (s *UTBot) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["key_value"]; ok {
		cp.KeyValue = ClampParam(spec["key_value"], v)
	}
	cp.resetState()
	return &cp
}

func (s *UTBot) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.ATRPeriod+2 {
		return Signal{Side: Hold}, nil
	}
	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	nLoss := s.KeyValue.Mul(atr)
	last := candles[len(candles)-1]
	close := last.Close

	if !s.hasPrev {
		// Seed the trailing stop below price on the first evaluable bar — an uptrend assumption,
		// matching the original's own initial state before any cross has occurred.
		s.prevStop = close.Sub(nLoss)
		s.prevClose = close
		s.prevUptrend = true
		s.hasPrev = true
		return Signal{Side: Hold}, nil
	}

	// Ratchet the trailing stop: rises with price in an uptrend, falls with price in a downtrend,
	// flips instantly on a cross through it — UT Bot's own xATRTrailingStop recurrence.
	var stop decimal.Decimal
	switch {
	case close.GreaterThan(s.prevStop) && s.prevClose.GreaterThan(s.prevStop):
		stop = decimal.Max(s.prevStop, close.Sub(nLoss))
	case close.LessThan(s.prevStop) && s.prevClose.LessThan(s.prevStop):
		stop = decimal.Min(s.prevStop, close.Add(nLoss))
	case close.GreaterThan(s.prevStop):
		stop = close.Sub(nLoss)
	default:
		stop = close.Add(nLoss)
	}

	crossedAbove := s.prevClose.LessThanOrEqual(s.prevStop) && close.GreaterThan(stop)
	crossedBelow := s.prevClose.GreaterThanOrEqual(s.prevStop) && close.LessThan(stop)

	s.prevStop = stop
	s.prevClose = close

	switch {
	case crossedAbove:
		s.prevUptrend = true
		risk := close.Sub(stop)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    close,
			SLPx:       stop,
			TPPx:       close.Add(risk.Mul(decimal.NewFromFloat(1.5))),
			SLPct:      risk.Div(close),
			TPPct:      risk.Div(close).Mul(decimal.NewFromFloat(1.5)),
		}, nil
	case crossedBelow:
		s.prevUptrend = false
		risk := stop.Sub(close)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    close,
			SLPx:       stop,
			TPPx:       close.Sub(risk.Mul(decimal.NewFromFloat(1.5))),
			SLPct:      risk.Div(close),
			TPPct:      risk.Div(close).Mul(decimal.NewFromFloat(1.5)),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
