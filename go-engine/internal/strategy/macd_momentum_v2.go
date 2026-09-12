package strategy

import "github.com/shopspring/decimal"

// MACDMomentumV2 keeps V1's trigger — the MACD histogram crossing zero — and adds the two things
// its 64-trade record showed it was missing.
//
// V1: 48.4% win rate, +0.40 realized, 16 TP against 38 SL. Directionally the best of the twelve,
// which is why it survives into V2 largely intact; it simply gave back on exits. Its fixed 0.8%
// stop and 1.2% target ignored the instrument entirely, and a zero-cross fires just as readily in
// chop — where the histogram oscillates around zero and every cross is noise — as it does at the
// start of a real move.
//
// V2 adds:
//   - ATR-scaled levels, so the stop reflects the instrument's own noise.
//   - A trend filter: only take crosses in the direction of a slow EMA, the standard way to drop
//     the counter-trend half of a momentum signal.
//   - A minimum histogram magnitude, so a cross has to clear a threshold proportional to recent
//     volatility rather than merely change sign. This is what filters chop, and it is the reason
//     V2 will signal less often than V1 did.
type MACDMomentumV2 struct {
	FastLen    int
	SlowLen    int
	SignalLen  int
	ATRPeriod  int
	TrendEMA   int
	MinHistATR decimal.Decimal // histogram must exceed this fraction of ATR to count
	StopATR    decimal.Decimal
	RiskReward decimal.Decimal
}

func NewMACDMomentumV2() *MACDMomentumV2 {
	return &MACDMomentumV2{
		FastLen:    12,
		SlowLen:    26,
		SignalLen:  9,
		ATRPeriod:  14,
		TrendEMA:   50,
		MinHistATR: decimal.NewFromFloat(0.08),
		StopATR:    decimal.NewFromFloat(1.2),
		RiskReward: decimal.NewFromFloat(1.8),
	}
}

func (s *MACDMomentumV2) Name() string { return "macd_momentum_v2" }

func (s *MACDMomentumV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "fast_len", Default: decimal.NewFromInt(int64(s.FastLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(50)},
		{Name: "slow_len", Default: decimal.NewFromInt(int64(s.SlowLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(100)},
		{Name: "signal_len", Default: decimal.NewFromInt(int64(s.SignalLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(50)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "trend_ema", Default: decimal.NewFromInt(int64(s.TrendEMA)), Min: decimal.NewFromInt(10), Max: decimal.NewFromInt(200)},
		{Name: "min_hist_atr", Default: s.MinHistATR, Min: decimal.Zero, Max: decimal.NewFromInt(1)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *MACDMomentumV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["fast_len"]; ok {
		cp.FastLen = int(ClampParam(spec["fast_len"], v).IntPart())
	}
	if v, ok := values["slow_len"]; ok {
		cp.SlowLen = int(ClampParam(spec["slow_len"], v).IntPart())
	}
	if v, ok := values["signal_len"]; ok {
		cp.SignalLen = int(ClampParam(spec["signal_len"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["trend_ema"]; ok {
		cp.TrendEMA = int(ClampParam(spec["trend_ema"], v).IntPart())
	}
	if v, ok := values["min_hist_atr"]; ok {
		cp.MinHistATR = ClampParam(spec["min_hist_atr"], v)
	}
	if v, ok := values["stop_atr"]; ok {
		cp.StopATR = ClampParam(spec["stop_atr"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	// Degenerate fast/slow ordering makes the histogram meaningless rather than merely odd, so it
	// is repaired here the same way rsi_sma_fuzzy repairs an inverted zone (CLAUDE.md §14).
	if cp.FastLen >= cp.SlowLen {
		cp.FastLen = cp.SlowLen - 1
		if cp.FastLen < 2 {
			cp.FastLen, cp.SlowLen = 2, 3
		}
	}
	return &cp
}

// macdHist returns the current and previous histogram values, computed entirely from the passed
// candles. Deliberately stateless: V1's original carried the previous value in a struct field,
// which is only the previous CANDLE's value if Evaluate is called exactly once per closed candle —
// a restart reseeds the window and re-evaluating compared a value against itself (CLAUDE.md §30.1).
func (s *MACDMomentumV2) macdHist(candles []Candle) (now, prev decimal.Decimal, err error) {
	fast, err := EMASeries(candles, s.FastLen)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	slow, err := EMASeries(candles, s.SlowLen)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}

	// EMASeries values before each period's warm-up are not meaningful; the MACD line is only
	// defined where BOTH are, which is from slowLen-1 onward.
	start := s.SlowLen - 1
	if start < 0 || start >= len(fast) || start >= len(slow) {
		return decimal.Zero, decimal.Zero, errNeedMore(s.SlowLen, len(candles))
	}
	macd := make([]Candle, 0, len(candles)-start)
	for i := start; i < len(candles) && i < len(fast) && i < len(slow); i++ {
		// EMASeries operates on Close, so the MACD line is carried as a synthetic candle's close.
		macd = append(macd, Candle{Close: fast[i].Sub(slow[i])})
	}
	if len(macd) < s.SignalLen+1 {
		return decimal.Zero, decimal.Zero, errNeedMore(s.SignalLen+1, len(macd))
	}
	sig, err := EMASeries(macd, s.SignalLen)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	n := len(macd) - 1
	return macd[n].Close.Sub(sig[n]), macd[n-1].Close.Sub(sig[n-1]), nil
}

func (s *MACDMomentumV2) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.SlowLen+s.SignalLen+1, s.ATRPeriod+1, s.TrendEMA)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	histNow, histPrev, err := s.macdHist(candles)
	if err != nil {
		return Signal{}, err
	}
	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	if !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	trend, err := EMA(candles, s.TrendEMA)
	if err != nil {
		return Signal{}, err
	}

	price := candles[len(candles)-1].Close
	// The cross must clear a volatility-proportional threshold. A bare sign change is what makes
	// this strategy fire repeatedly in chop.
	minHist := atr.Mul(s.MinHistATR)

	crossedUp := histPrev.LessThanOrEqual(decimal.Zero) && histNow.GreaterThan(minHist)
	crossedDown := histPrev.GreaterThanOrEqual(decimal.Zero) && histNow.LessThan(minHist.Neg())

	switch {
	case crossedUp && price.GreaterThan(trend):
		return v2Signal(Buy, decimal.NewFromFloat(0.6), price, atr, s.StopATR, s.RiskReward), nil
	case crossedDown && price.LessThan(trend):
		return v2Signal(Sell, decimal.NewFromFloat(0.6), price, atr, s.StopATR, s.RiskReward), nil
	default:
		return Signal{Side: Hold}, nil
	}
}
