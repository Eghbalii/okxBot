package strategy

import "github.com/shopspring/decimal"

// PhilakonesFib is a Go port of the well-known TradingView strategy commonly titled
// "Simple, But Perfect Trading Strategy" / "Simple Profitable Trading Strategy" attributed to the
// Philakone 5-EMA fibonacci system (https://www.tradingview.com/script/ — raw PineScript source was
// not extractable via automated fetch, so this is implemented from the strategy's well-documented,
// widely-republished rules rather than a literal transliteration).
//
// Standard/default parameters used, per the strategy's own well-known specification:
//   - Five EMAs at Fibonacci lengths 8, 13, 21, 34, 55 — Philakone's own published sequence.
//   - RSI(14): bullish zone is 40-70 (trade only while RSI sits inside), exit/avoid above 70 or
//     below 30 (RSI's own classic overbought/oversold extremes).
//   - Stochastic(14,3,3): bullish confirmation below 80, exit above 95.
//
// Signal logic: a long requires the five EMAs stacked in strictly ascending order fast-to-slow
// (8>13>21>34>55, i.e. an established uptrend structure) AND RSI inside its bullish 40-70 zone AND
// Stochastic %K below 80 (not yet overbought) — mirrored for a short with descending EMA order and
// the complementary RSI/Stochastic zones. Exit signal (reported as a Sell/Buy-covering Hold
// transition is not modeled here since this package's Signal has no explicit "close" side; the
// 13/55 EMA cross the strategy uses for exits is instead folded into entry suppression: once EMA13
// crosses back through EMA55 the ascending/descending order breaks, so the entry condition itself
// stops firing, which is the same practical effect).
type PhilakonesFib struct {
	EMA1, EMA2, EMA3, EMA4, EMA5             int // 8, 13, 21, 34, 55
	RSIPeriod                                int
	RSIBullMin, RSIBullMax                   decimal.Decimal
	StochKPeriod, StochKSmooth, StochDPeriod int
	StochBullMax                             decimal.Decimal
	SLPct, TPPct                             decimal.Decimal
}

func NewPhilakonesFib() *PhilakonesFib {
	return &PhilakonesFib{
		EMA1: 8, EMA2: 13, EMA3: 21, EMA4: 34, EMA5: 55,
		RSIPeriod:    14,
		RSIBullMin:   decimal.NewFromInt(40),
		RSIBullMax:   decimal.NewFromInt(70),
		StochKPeriod: 14,
		StochKSmooth: 3,
		StochDPeriod: 3,
		StochBullMax: decimal.NewFromInt(80),
		SLPct:        decimal.NewFromFloat(0.01),
		TPPct:        decimal.NewFromFloat(0.02),
	}
}

func (s *PhilakonesFib) Name() string { return "philakones_fib" }

func (s *PhilakonesFib) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "rsi_period", Default: decimal.NewFromInt(int64(s.RSIPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "rsi_bull_min", Default: s.RSIBullMin, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(60)},
		{Name: "rsi_bull_max", Default: s.RSIBullMax, Min: decimal.NewFromInt(50), Max: decimal.NewFromInt(99)},
		{Name: "stoch_bull_max", Default: s.StochBullMax, Min: decimal.NewFromInt(50), Max: decimal.NewFromInt(99)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *PhilakonesFib) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["rsi_period"]; ok {
		cp.RSIPeriod = int(ClampParam(spec["rsi_period"], v).IntPart())
	}
	if v, ok := values["rsi_bull_min"]; ok {
		cp.RSIBullMin = ClampParam(spec["rsi_bull_min"], v)
	}
	if v, ok := values["rsi_bull_max"]; ok {
		cp.RSIBullMax = ClampParam(spec["rsi_bull_max"], v)
	}
	if v, ok := values["stoch_bull_max"]; ok {
		cp.StochBullMax = ClampParam(spec["stoch_bull_max"], v)
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	// Stateless: every value Evaluate uses is recomputed fresh from the candles passed in.
	return &cp
}

func (s *PhilakonesFib) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.EMA5, s.StochKPeriod+s.StochKSmooth+s.StochDPeriod, s.RSIPeriod+1) + 1
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	e1, err := EMA(candles, s.EMA1)
	if err != nil {
		return Signal{}, err
	}
	e2, err := EMA(candles, s.EMA2)
	if err != nil {
		return Signal{}, err
	}
	e3, err := EMA(candles, s.EMA3)
	if err != nil {
		return Signal{}, err
	}
	e4, err := EMA(candles, s.EMA4)
	if err != nil {
		return Signal{}, err
	}
	e5, err := EMA(candles, s.EMA5)
	if err != nil {
		return Signal{}, err
	}
	rsi, err := RSI(candles, s.RSIPeriod)
	if err != nil {
		return Signal{}, err
	}
	k, _, err := Stochastic(candles, s.StochKPeriod, s.StochKSmooth, s.StochDPeriod)
	if err != nil {
		return Signal{}, err
	}

	ascending := e1.GreaterThan(e2) && e2.GreaterThan(e3) && e3.GreaterThan(e4) && e4.GreaterThan(e5)
	descending := e1.LessThan(e2) && e2.LessThan(e3) && e3.LessThan(e4) && e4.LessThan(e5)

	rsiBullZone := rsi.GreaterThanOrEqual(s.RSIBullMin) && rsi.LessThanOrEqual(s.RSIBullMax)
	stochBull := k.LessThan(s.StochBullMax)

	rsiBearZone := rsi.LessThanOrEqual(hundred.Sub(s.RSIBullMin)) && rsi.GreaterThanOrEqual(hundred.Sub(s.RSIBullMax))
	stochBear := k.GreaterThan(hundred.Sub(s.StochBullMax))

	switch {
	case ascending && rsiBullZone && stochBull:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case descending && rsiBearZone && stochBear:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
