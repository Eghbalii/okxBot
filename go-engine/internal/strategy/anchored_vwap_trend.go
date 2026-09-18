package strategy

import "github.com/shopspring/decimal"

// AnchoredVWAPTrend is a Go port of the widely-known TradingView strategy family commonly titled
// "Golden Trident: Swing Anchored VWAP Trend System" (raw PineScript source not extractable via
// automated fetch; implemented from the standard, well-documented anchored-VWAP construction).
//
// Standard/default parameters: a fractal swing-detection depth of 5 bars (matching zigzag_pa.go's
// and rsi_divergence.go's own default — a common published choice for swing-pivot detection on
// lower timeframes) and a broader EMA(50) trend filter, a widely-used length for a "broader trend"
// context filter on a 5m chart.
//
// Signal logic: anchor a VWAP calculation at the most recent confirmed significant swing low (in an
// uptrend context, EMA50 rising) or swing high (in a downtrend context, EMA50 falling) — the
// "Golden Trident" naming refers to using the most recent swing extreme as the VWAP's anchor point,
// a common discretionary anchored-VWAP technique. Buy when price crosses back ABOVE that
// low-anchored VWAP while the broader trend filter is bullish; sell when price crosses back BELOW a
// high-anchored VWAP while the broader trend is bearish. Stop at the anchor pivot itself (a genuine
// structural level).
type AnchoredVWAPTrend struct {
	Depth       int
	TrendEMALen int
	RiskReward  decimal.Decimal

	prevTrendEMA decimal.Decimal
	hasPrevTrend bool
	// tradedAnchorPrice identifies the currently-traded anchor, so the identical swing point cannot
	// fire a second entry while it remains the most recent one.
	tradedAnchorPrice decimal.Decimal
	hasTradedAnchor   bool
}

func NewAnchoredVWAPTrend() *AnchoredVWAPTrend {
	return &AnchoredVWAPTrend{
		Depth:       5,
		TrendEMALen: 50,
		RiskReward:  decimal.NewFromFloat(1.5),
	}
}

func (s *AnchoredVWAPTrend) Name() string { return "anchored_vwap_trend" }

func (s *AnchoredVWAPTrend) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "depth", Default: decimal.NewFromInt(int64(s.Depth)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(30)},
		{Name: "trend_ema_len", Default: decimal.NewFromInt(int64(s.TrendEMALen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *AnchoredVWAPTrend) resetState() {
	s.prevTrendEMA, s.hasPrevTrend = decimal.Zero, false
	s.tradedAnchorPrice, s.hasTradedAnchor = decimal.Zero, false
}

func (s *AnchoredVWAPTrend) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["depth"]; ok {
		cp.Depth = int(ClampParam(spec["depth"], v).IntPart())
	}
	if v, ok := values["trend_ema_len"]; ok {
		cp.TrendEMALen = int(ClampParam(spec["trend_ema_len"], v).IntPart())
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *AnchoredVWAPTrend) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(2*s.Depth+10, s.TrendEMALen+1)
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	trendEMA, err := EMA(candles, s.TrendEMALen)
	if err != nil {
		return Signal{}, err
	}
	if !s.hasPrevTrend {
		s.prevTrendEMA, s.hasPrevTrend = trendEMA, true
		return Signal{Side: Hold}, nil
	}
	trendUp := trendEMA.GreaterThan(s.prevTrendEMA)
	trendDown := trendEMA.LessThan(s.prevTrendEMA)
	s.prevTrendEMA = trendEMA

	pivots := ZigZagPivots(candles, s.Depth)
	var lows, highs []ZigZagPivot
	for _, p := range pivots {
		if p.High {
			highs = append(highs, p)
		} else {
			lows = append(lows, p)
		}
	}
	if len(candles) < 2 {
		return Signal{Side: Hold}, nil
	}
	last := candles[len(candles)-1]
	prev := candles[len(candles)-2]

	// Uptrend context: anchor VWAP at the most recent swing low. The cross is detected against the
	// SAME anchor's VWAP computed on both the current and prior bar's window — deliberately not a
	// carried "was above" flag, since the active anchor itself can change between calls (a new swing
	// low forms), which would make a flag from a different anchor's VWAP meaningless to compare
	// against.
	if trendUp && len(lows) > 0 {
		anchor := lows[len(lows)-1]
		if anchor.Index < len(candles)-1 { // need at least one bar before `last` under this anchor
			vwapNow, err := AnchoredVWAP(candles, anchor.Index)
			if err == nil {
				vwapPrev, err := AnchoredVWAP(candles[:len(candles)-1], anchor.Index)
				if err == nil {
					crossedUp := prev.Close.LessThanOrEqual(vwapPrev) && last.Close.GreaterThan(vwapNow)
					alreadyTraded := s.hasTradedAnchor && s.tradedAnchorPrice.Equal(anchor.Price)
					if crossedUp && !alreadyTraded {
						risk := last.Close.Sub(anchor.Price)
						if risk.IsPositive() {
							s.tradedAnchorPrice, s.hasTradedAnchor = anchor.Price, true
							return Signal{
								Side:       Buy,
								Confidence: decimal.NewFromFloat(0.6),
								EntryPx:    last.Close,
								SLPx:       anchor.Price,
								TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
								SLPct:      risk.Div(last.Close),
								TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
							}, nil
						}
					}
				}
			}
		}
	}

	// Downtrend context: anchor VWAP at the most recent swing high, mirrored.
	if trendDown && len(highs) > 0 {
		anchor := highs[len(highs)-1]
		if anchor.Index < len(candles)-1 {
			vwapNow, err := AnchoredVWAP(candles, anchor.Index)
			if err == nil {
				vwapPrev, err := AnchoredVWAP(candles[:len(candles)-1], anchor.Index)
				if err == nil {
					crossedDown := prev.Close.GreaterThanOrEqual(vwapPrev) && last.Close.LessThan(vwapNow)
					alreadyTraded := s.hasTradedAnchor && s.tradedAnchorPrice.Equal(anchor.Price)
					if crossedDown && !alreadyTraded {
						risk := anchor.Price.Sub(last.Close)
						if risk.IsPositive() {
							s.tradedAnchorPrice, s.hasTradedAnchor = anchor.Price, true
							return Signal{
								Side:       Sell,
								Confidence: decimal.NewFromFloat(0.6),
								EntryPx:    last.Close,
								SLPx:       anchor.Price,
								TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
								SLPct:      risk.Div(last.Close),
								TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
							}, nil
						}
					}
				}
			}
		}
	}

	return Signal{Side: Hold}, nil
}
