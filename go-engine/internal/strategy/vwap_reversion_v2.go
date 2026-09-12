package strategy

import "github.com/shopspring/decimal"

// VWAPReversionV2 keeps V1's idea — fade a price extension away from a rolling VWAP — and changes
// how the trade is framed around it.
//
// V1's record: 532 closed paper trades, 39.5% win rate, -2.55 realized. The entry was not the
// problem; it was the highest-volume signal generator in the system and roughly break-even on
// direction. Two things lost the money:
//
//   - Fixed 0.6%/0.8% stop and target on every instrument. A median 5m candle is 0.081% on BTC and
//     0.453% on ZEC — 5.6x apart — so one pair of percentages was simultaneously too tight to
//     survive noise on ZEC and too wide to be reached on BTC.
//   - No trend filter. Fading an extension works in a range and is exactly the wrong trade in a
//     trend, where "extended" is just the trend continuing; V1 took both identically.
//
// V2 sizes both levels in ATR units and only fades WITH the higher-level drift: it requires price
// to still be on the same side of a slow EMA that the fade is aiming toward, which is the standard
// way to keep a mean-reversion scalp out of a runaway trend.
type VWAPReversionV2 struct {
	VWAPPeriod   int
	ATRPeriod    int
	TrendEMALen  int
	DeviationATR decimal.Decimal
	StopATR      decimal.Decimal
	RiskReward   decimal.Decimal
}

func NewVWAPReversionV2() *VWAPReversionV2 {
	return &VWAPReversionV2{
		VWAPPeriod:   20,
		ATRPeriod:    14,
		TrendEMALen:  50,
		DeviationATR: decimal.NewFromFloat(1.8),
		StopATR:      decimal.NewFromFloat(1.1),
		RiskReward:   decimal.NewFromFloat(1.5),
	}
}

func (s *VWAPReversionV2) Name() string { return "vwap_reversion_v2" }

func (s *VWAPReversionV2) Params() []ParamSpec {
	return append([]ParamSpec{
		{Name: "vwap_period", Default: decimal.NewFromInt(int64(s.VWAPPeriod)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(200)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "trend_ema_len", Default: decimal.NewFromInt(int64(s.TrendEMALen)), Min: decimal.NewFromInt(10), Max: decimal.NewFromInt(200)},
		{Name: "deviation_atr", Default: s.DeviationATR, Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromInt(6)},
	}, v2StopParams(s.StopATR, s.RiskReward)...)
}

func (s *VWAPReversionV2) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["vwap_period"]; ok {
		cp.VWAPPeriod = int(ClampParam(spec["vwap_period"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(spec["atr_period"], v).IntPart())
	}
	if v, ok := values["trend_ema_len"]; ok {
		cp.TrendEMALen = int(ClampParam(spec["trend_ema_len"], v).IntPart())
	}
	if v, ok := values["deviation_atr"]; ok {
		cp.DeviationATR = ClampParam(spec["deviation_atr"], v)
	}
	if v, ok := values["stop_atr"]; ok {
		cp.StopATR = ClampParam(spec["stop_atr"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

func (s *VWAPReversionV2) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.VWAPPeriod, s.ATRPeriod+1, s.TrendEMALen)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	vwap, err := SessionVWAP(candles, s.VWAPPeriod)
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
	trend, err := EMA(candles, s.TrendEMALen)
	if err != nil {
		return Signal{}, err
	}

	price := candles[len(candles)-1].Close
	deviation := price.Sub(vwap).Div(atr)

	confidence := func(dev decimal.Decimal) decimal.Decimal {
		c := dev.Abs().Div(s.DeviationATR)
		if c.GreaterThan(decimal.NewFromInt(1)) {
			c = decimal.NewFromInt(1)
		}
		return c
	}

	switch {
	case deviation.LessThan(s.DeviationATR.Neg()):
		// Stretched below VWAP — fade up, but only if price is still above the slow EMA. Below it
		// the market is trending down and "extended" means continuation, not exhaustion.
		if price.LessThan(trend) {
			return Signal{Side: Hold}, nil
		}
		return v2Signal(Buy, confidence(deviation), price, atr, s.StopATR, s.RiskReward), nil
	case deviation.GreaterThan(s.DeviationATR):
		if price.GreaterThan(trend) {
			return Signal{Side: Hold}, nil
		}
		return v2Signal(Sell, confidence(deviation), price, atr, s.StopATR, s.RiskReward), nil
	default:
		return Signal{Side: Hold}, nil
	}
}
