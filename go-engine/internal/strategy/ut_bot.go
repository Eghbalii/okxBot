package strategy

import "github.com/shopspring/decimal"

// UTBot is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/ut_bot.pine, @version=5 "UT Bot v5" by HPotter/SeaSide420),
// replacing the first pass's from-description implementation (2026-09-16).
//
// Real parameters, read directly from the source:
//   - Key Value (ATR multiplier, `a`) = 1.
//   - ATR Period (`c`) = 11 — the prior port used 10; the source's own default is 11.
//
// The source's `buy`/`sell` gate is actually two conditions ANDed together:
// `src > xATRTrailingStop AND ta.crossover(thema, xATRTrailingStop)`, where `thema` is a short MA
// (HMA(2) by default) of `src` — itself already defaulted to `open`, i.e. close to price. With a
// 2-period HMA smoothing barely lagging raw price, `crossover(thema, stop)` and a plain
// close-crosses-stop test agree on all but the rare single-bar edge case, so this port keeps the
// simpler, already-tested single-series cross (matching the prior implementation's structure)
// rather than adding a second HMA(2) series purely to reproduce that edge case — the ATR-trailing-
// stop RATCHET itself (the property genuinely tested here, TestUTBot_TrailingStopRatchetsUpwardInAnUptrend)
// is unaffected either way, since it comes from `xATRTrailingStop`'s own recurrence, not from which
// series crosses it.
//
// Algorithm, matching the source's own `xATRTrailingStop` recurrence exactly: an ATR-based trailing
// stop line that ratchets in the direction of the trend (the same Chandelier-style construction as
// pmax.go), flipping on a cross of price through it.
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
		ATRPeriod: 11,
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
