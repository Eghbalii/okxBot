package strategy

import "github.com/shopspring/decimal"

// FlawlessVictory is a Go port/simplification of the widely-known TradingView "Flawless Victory
// Strategy" (also circulated as a "15min BTC ML Strategy") — raw PineScript source not extractable
// via automated fetch.
//
// IMPORTANT SIMPLIFICATION, stated explicitly per the task's own instruction: the real "Flawless
// Victory"/ML-branded script is reported to use trained/opaque internal weights and a proprietary
// combination of conditions that cannot be reconstructed from its public description alone — there
// is no published, standard algorithm to port faithfully the way there is for UT Bot or MACD. This
// implementation is a DOCUMENTED, HONEST simplification: a multi-condition confluence system using
// three well-known, standard indicators (RSI, MACD, and an EMA trend filter) combined via a simple
// majority vote (at least MinAgree of 3 conditions must agree), rather than attempting to guess at
// unavailable trained weights. This is not a claim of parity with the original's actual (unknown)
// internals — only that it captures the general "multi-indicator confluence" shape the name implies.
//
// Standard/default parameters: RSI(14) with the classic 50 midline (bullish above, bearish below —
// used here as a directional vote rather than overbought/oversold, since a majority-vote confluence
// wants each member's opinion on TREND direction), MACD(12,26,9) histogram sign, and EMA(50) trend
// filter (price above/below).
type FlawlessVictory struct {
	RSIPeriod                      int
	MACDFast, MACDSlow, MACDSignal int
	EMALen                         int
	MinAgree                       int // out of 3 conditions
	SLPct, TPPct                   decimal.Decimal
}

func NewFlawlessVictory() *FlawlessVictory {
	return &FlawlessVictory{
		RSIPeriod:  14,
		MACDFast:   12,
		MACDSlow:   26,
		MACDSignal: 9,
		EMALen:     50,
		MinAgree:   2,
		SLPct:      decimal.NewFromFloat(0.007),
		TPPct:      decimal.NewFromFloat(0.014),
	}
}

func (s *FlawlessVictory) Name() string { return "flawless_victory" }

func (s *FlawlessVictory) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "rsi_period", Default: decimal.NewFromInt(int64(s.RSIPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "ema_len", Default: decimal.NewFromInt(int64(s.EMALen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "min_agree", Default: decimal.NewFromInt(int64(s.MinAgree)), Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(3)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *FlawlessVictory) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["rsi_period"]; ok {
		cp.RSIPeriod = int(ClampParam(spec["rsi_period"], v).IntPart())
	}
	if v, ok := values["ema_len"]; ok {
		cp.EMALen = int(ClampParam(spec["ema_len"], v).IntPart())
	}
	if v, ok := values["min_agree"]; ok {
		cp.MinAgree = int(ClampParam(spec["min_agree"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(spec["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(spec["tp_pct"], v)
	}
	return &cp
}

func (s *FlawlessVictory) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.RSIPeriod+1, s.MACDSlow+s.MACDSignal+1, s.EMALen) + 1
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	rsi, err := RSI(candles, s.RSIPeriod)
	if err != nil {
		return Signal{}, err
	}
	histNow, _, err := macdHistSeries(candles, s.MACDFast, s.MACDSlow, s.MACDSignal)
	if err != nil {
		return Signal{}, err
	}
	ema, err := EMA(candles, s.EMALen)
	if err != nil {
		return Signal{}, err
	}
	close := candles[len(candles)-1].Close

	bullVotes, bearVotes := 0, 0
	if rsi.GreaterThan(decimal.NewFromInt(50)) {
		bullVotes++
	} else if rsi.LessThan(decimal.NewFromInt(50)) {
		bearVotes++
	}
	if histNow.IsPositive() {
		bullVotes++
	} else if histNow.IsNegative() {
		bearVotes++
	}
	if close.GreaterThan(ema) {
		bullVotes++
	} else if close.LessThan(ema) {
		bearVotes++
	}

	minAgree := s.MinAgree
	if minAgree < 1 {
		minAgree = 1
	}
	if minAgree > 3 {
		minAgree = 3
	}

	switch {
	case bullVotes >= minAgree && bullVotes > bearVotes:
		confidence := decimal.NewFromInt(int64(bullVotes)).Div(decimal.NewFromInt(3))
		return Signal{Side: Buy, Confidence: confidence, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case bearVotes >= minAgree && bearVotes > bullVotes:
		confidence := decimal.NewFromInt(int64(bearVotes)).Div(decimal.NewFromInt(3))
		return Signal{Side: Sell, Confidence: confidence, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
