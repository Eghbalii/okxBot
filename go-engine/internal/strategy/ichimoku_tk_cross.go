package strategy

import "github.com/shopspring/decimal"

// IchimokuTKCross is a Go port of the widely-known TradingView strategy commonly titled "Ichimoku
// TK Cross EMA200 Crypto Strategy" (raw PineScript source not extractable via automated fetch;
// implemented from Ichimoku's own standard, well-documented construction).
//
// Standard/default parameters used, Goichi Hosoda's own published defaults:
//   - Tenkan-sen (conversion line) period = 9.
//   - Kijun-sen (base line) period = 26.
//   - EMA(200) trend filter — the standard long-term filter length used by this strategy family.
//
// Signal logic: only take a Tenkan/Kijun bullish cross (Tenkan crosses above Kijun) when price is
// above EMA200 (uptrend), and only a bearish cross (Tenkan crosses below Kijun) when price is below
// EMA200 (downtrend) — the EMA200 filter exists specifically to reject counter-trend TK crosses,
// which is the whole premise of this strategy over a bare Ichimoku TK cross.
type IchimokuTKCross struct {
	TenkanPeriod int
	KijunPeriod  int
	EMALen       int
	SLPct, TPPct decimal.Decimal
}

func NewIchimokuTKCross() *IchimokuTKCross {
	return &IchimokuTKCross{
		TenkanPeriod: 9,
		KijunPeriod:  26,
		EMALen:       200,
		SLPct:        decimal.NewFromFloat(0.008),
		TPPct:        decimal.NewFromFloat(0.016),
	}
}

func (s *IchimokuTKCross) Name() string { return "ichimoku_tk_cross" }

func (s *IchimokuTKCross) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "tenkan_period", Default: decimal.NewFromInt(int64(s.TenkanPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(60)},
		{Name: "kijun_period", Default: decimal.NewFromInt(int64(s.KijunPeriod)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(150)},
		{Name: "ema_len", Default: decimal.NewFromInt(int64(s.EMALen)), Min: decimal.NewFromInt(20), Max: decimal.NewFromInt(400)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *IchimokuTKCross) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["tenkan_period"]; ok {
		cp.TenkanPeriod = int(ClampParam(spec["tenkan_period"], v).IntPart())
	}
	if v, ok := values["kijun_period"]; ok {
		cp.KijunPeriod = int(ClampParam(spec["kijun_period"], v).IntPart())
	}
	if v, ok := values["ema_len"]; ok {
		cp.EMALen = int(ClampParam(spec["ema_len"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	return &cp
}

func (s *IchimokuTKCross) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.KijunPeriod, s.EMALen) + 2
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	tenkanNow, kijunNow, err := Ichimoku(candles, s.TenkanPeriod, s.KijunPeriod)
	if err != nil {
		return Signal{}, err
	}
	tenkanPrev, kijunPrev, err := Ichimoku(candles[:len(candles)-1], s.TenkanPeriod, s.KijunPeriod)
	if err != nil {
		return Signal{}, err
	}
	ema200, err := EMA(candles, s.EMALen)
	if err != nil {
		return Signal{}, err
	}
	close := candles[len(candles)-1].Close

	crossedUp := tenkanPrev.LessThanOrEqual(kijunPrev) && tenkanNow.GreaterThan(kijunNow)
	crossedDown := tenkanPrev.GreaterThanOrEqual(kijunPrev) && tenkanNow.LessThan(kijunNow)

	switch {
	case crossedUp && close.GreaterThan(ema200):
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case crossedDown && close.LessThan(ema200):
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
