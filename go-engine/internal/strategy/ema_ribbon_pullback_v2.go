package strategy

import "github.com/shopspring/decimal"

// EMARibbonPullbackV2 trades a pullback to the middle EMA of a fast/mid/slow ribbon stacked in
// trend order.
//
// V1 was the worst performer of the twelve by win rate: 40 trades, 12.5% — 1 win against 36 stop
// closes. A 12.5% win rate is not a tuning problem, it is a sign the entry was firing against the
// move. The cause is visible in the logic: V1 required only that price TOUCH the middle EMA during
// a stacked trend, and on a 5m chart the first touch of the mid EMA is as often the beginning of a
// trend failure as it is a pullback — particularly once the ribbon has been stacked for a while and
// the move is mature.
//
// V2 requires the pullback to have actually resumed before entering: the candle must touch the mid
// EMA and close back above it (below, for shorts), AND the fast EMA must still be on the correct
// side of the mid, so a ribbon that has already begun to unwind no longer qualifies. That is the
// difference between buying a dip and buying a breakdown.
type EMARibbonPullbackV2 struct {
	FastLen    int
	MidLen     int
	SlowLen    int
	ATRPeriod  int
	StopATR    decimal.Decimal
	RiskReward decimal.Decimal
}

func NewEMARibbonPullbackV2() *EMARibbonPullbackV2 {
	return &EMARibbonPullbackV2{
		FastLen:    8,
		MidLen:     21,
		SlowLen:    55,
		ATRPeriod:  14,
		StopATR:    decimal.NewFromFloat(1.2),
		RiskReward: decimal.NewFromFloat(1.8),
	}
}

func (s *EMARibbonPullbackV2) Name() string { return "ema_ribbon_pullback_v2" }

func (s *EMARibbonPullbackV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "fast_len", Default: decimal.NewFromInt(int64(s.FastLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(50)},
		{Name: "mid_len", Default: decimal.NewFromInt(int64(s.MidLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(100)},
		{Name: "slow_len", Default: decimal.NewFromInt(int64(s.SlowLen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *EMARibbonPullbackV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["fast_len"]; ok {
		cp.FastLen = int(ClampParam(spec["fast_len"], v).IntPart())
	}
	if v, ok := values["mid_len"]; ok {
		cp.MidLen = int(ClampParam(spec["mid_len"], v).IntPart())
	}
	if v, ok := values["slow_len"]; ok {
		cp.SlowLen = int(ClampParam(spec["slow_len"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["stop_atr"]; ok {
		cp.StopATR = ClampParam(spec["stop_atr"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	// A ribbon whose lengths are not strictly increasing is not a ribbon — repaired rather than
	// left to produce meaningless comparisons (the rsi_sma_fuzzy precedent, CLAUDE.md §14).
	if cp.MidLen <= cp.FastLen {
		cp.MidLen = cp.FastLen + 1
	}
	if cp.SlowLen <= cp.MidLen {
		cp.SlowLen = cp.MidLen + 1
	}
	return &cp
}

func (s *EMARibbonPullbackV2) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.SlowLen, s.ATRPeriod+1)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	fast, err := EMA(candles, s.FastLen)
	if err != nil {
		return Signal{}, err
	}
	mid, err := EMA(candles, s.MidLen)
	if err != nil {
		return Signal{}, err
	}
	slow, err := EMA(candles, s.SlowLen)
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

	last := candles[len(candles)-1]
	stackedUp := fast.GreaterThan(mid) && mid.GreaterThan(slow)
	stackedDown := fast.LessThan(mid) && mid.LessThan(slow)

	switch {
	// Touched the mid EMA and closed back above it, with the ribbon still stacked: the dip was
	// bought. V1 entered on the touch alone, which also matches price cutting straight through.
	case stackedUp && last.Low.LessThanOrEqual(mid) && last.Close.GreaterThan(mid):
		return v2Signal(Buy, decimal.NewFromFloat(0.6), last.Close, atr, s.StopATR, s.RiskReward), nil
	case stackedDown && last.High.GreaterThanOrEqual(mid) && last.Close.LessThan(mid):
		return v2Signal(Sell, decimal.NewFromFloat(0.6), last.Close, atr, s.StopATR, s.RiskReward), nil
	default:
		return Signal{Side: Hold}, nil
	}
}
