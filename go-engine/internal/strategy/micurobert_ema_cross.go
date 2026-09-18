package strategy

import "github.com/shopspring/decimal"

// MicuRobertEMACross is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/micurobert_ema_cross.pine, @version=2 "[STRATEGY][RS]MicuRobert EMA
// cross V2"), replacing the first pass's from-description implementation (2026-09-16), which used a
// plain dual-EMA cross with an EMA(100) trend filter — the real strategy uses zero-lag EMAs at
// different periods and has TWO independent entry triggers, neither of which is a trend filter.
//
// Real parameters, read directly from the source:
//   - `ma00` = zero-lag EMA(5) of price ("EMA length"), `ma01` = zero-lag EMA(34) ("DEMA length" —
//     the source's own input name, despite computing a zero-lag EMA, not a true DEMA).
//     f_LB_zlema(src, len) = ema1 + (ema1 - ema2), where ema1 = ema(src, len), ema2 = ema(ema1, len)
//     — LazyBear's classic zero-lag EMA construction (double-smoothing correction).
//   - Price source defaults to `open`.
//
// Signal logic, exactly the source's two ORed triggers:
//   - buy_cond1 = crossover(ma00, ma01)                          — the fast/slow ZLEMA cross itself.
//   - buy_cond0 = crossover(price, ma00) AND ma00 > ma01         — price reclaiming the fast ZLEMA
//     while already in an established up-alignment.
//
// Mirrored for sell with crossunder. The source's session-time filter (USE_TRADESESSION, an
// intraday forex trading-hours gate) and its own trailing-stop mechanic are both period-agnostic to
// crypto/24-7 trading and specific to the source's own exit bookkeeping, so neither is modeled here
// — this package's caller already provides SL/TP/session handling uniformly (CLAUDE.md §16.9).
type MicuRobertEMACross struct {
	FastLen, SlowLen int
	SLPct, TPPct     decimal.Decimal
}

func NewMicuRobertEMACross() *MicuRobertEMACross {
	return &MicuRobertEMACross{
		FastLen: 5,
		SlowLen: 34,
		SLPct:   decimal.NewFromFloat(0.008),
		TPPct:   decimal.NewFromFloat(0.016),
	}
}

func (s *MicuRobertEMACross) Name() string { return "micurobert_ema_cross" }

func (s *MicuRobertEMACross) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "fast_len", Default: decimal.NewFromInt(int64(s.FastLen)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "slow_len", Default: decimal.NewFromInt(int64(s.SlowLen)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *MicuRobertEMACross) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["fast_len"]; ok {
		cp.FastLen = int(ClampParam(spec["fast_len"], v).IntPart())
	}
	if v, ok := values["slow_len"]; ok {
		cp.SlowLen = int(ClampParam(spec["slow_len"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	return &cp
}

// zlemaSeries computes LazyBear's zero-lag EMA over the OPEN price series (the source's own default
// price input), reusing EMASeries twice per the source's f_LB_zlema definition.
func zlemaSeries(candles []Candle, period int) ([]decimal.Decimal, error) {
	openCandles := make([]Candle, len(candles))
	for i, c := range candles {
		openCandles[i] = Candle{Close: c.Open}
	}
	ema1, err := EMASeries(openCandles, period)
	if err != nil {
		return nil, err
	}
	ema1AsCandles := make([]Candle, len(ema1))
	for i, v := range ema1 {
		ema1AsCandles[i] = Candle{Close: v}
	}
	ema2, err := EMASeries(ema1AsCandles, period)
	if err != nil {
		return nil, err
	}
	out := make([]decimal.Decimal, len(candles))
	for i := range out {
		out[i] = ema1[i].Add(ema1[i].Sub(ema2[i]))
	}
	return out, nil
}

func (s *MicuRobertEMACross) Evaluate(candles []Candle) (Signal, error) {
	// zlemaSeries needs SlowLen candles for ema1, then another SlowLen for ema2's own smoothing of
	// ema1 to stabilize — a conservative 2x bound plus room for the prior-bar comparison.
	need := 2*s.SlowLen + 2
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	fast, err := zlemaSeries(candles, s.FastLen)
	if err != nil {
		return Signal{}, err
	}
	slow, err := zlemaSeries(candles, s.SlowLen)
	if err != nil {
		return Signal{}, err
	}
	last := len(candles) - 1
	fastNow, fastPrev := fast[last], fast[last-1]
	slowNow, slowPrev := slow[last], slow[last-1]
	priceNow, pricePrev := candles[last].Open, candles[last-1].Open

	maCrossedUp := fastPrev.LessThanOrEqual(slowPrev) && fastNow.GreaterThan(slowNow)
	maCrossedDown := fastPrev.GreaterThanOrEqual(slowPrev) && fastNow.LessThan(slowNow)
	priceCrossedUpFast := pricePrev.LessThanOrEqual(fastPrev) && priceNow.GreaterThan(fastNow)
	priceCrossedDownFast := pricePrev.GreaterThanOrEqual(fastPrev) && priceNow.LessThan(fastNow)

	buy := maCrossedUp || (priceCrossedUpFast && fastNow.GreaterThan(slowNow))
	sell := maCrossedDown || (priceCrossedDownFast && fastNow.LessThan(slowNow))

	switch {
	case buy:
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case sell:
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.55), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
