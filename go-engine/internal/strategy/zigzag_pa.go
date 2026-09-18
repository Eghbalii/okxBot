package strategy

import "github.com/shopspring/decimal"

// ZigZagPA is a Go port of the widely-known TradingView "STRATEGY RS ZigZag PA Strategy V4.1"
// family (raw PineScript source not extractable via automated fetch; implemented from the standard,
// well-documented ZigZag-pivot + price-action structure algorithm the title describes).
//
// Standard/default parameters: a fractal depth of 5 bars on each side (a common published default
// for ZigZag-style pivot detection — enough bars to filter noise while still catching swings on a
// 5m timeframe).
//
// Signal logic: track the two most recent confirmed swing lows and the two most recent confirmed
// swing highs (via ZigZagPivots' fractal detection, indicators.go). A new confirmed swing low that
// sits ABOVE the prior confirmed swing low is a "higher low" — classic uptrend market structure —
// and triggers a buy once price is back above the most recent swing high (structure confirmation
// that the higher low held and the uptrend resumed). Mirrored: a new swing high below the prior
// swing high is a "lower high", triggering a sell once price breaks back below the most recent
// swing low. Stop at the triggering swing point itself (a genuine structural level).
type ZigZagPA struct {
	Depth      int
	RiskReward decimal.Decimal

	// tradedLowPrice/tradedHighPrice identify the pivot already used to trigger an entry, so the same
	// structural pattern cannot fire twice while it remains the most recent one.
	//
	// Identified by PRICE, deliberately not by ZigZagPivots' own Index: the candle window this
	// strategy is evaluated against SLIDES as PaperTrader trims to CandleWindow (CLAUDE.md §30.1's
	// own lesson, learned from ict_fvg/ict_order_block re-arming on exactly this mistake) — index 5
	// means a different bar once the window moves, so an index-based identity would silently start
	// matching the wrong pivot instead of failing safely. A price collision across two genuinely
	// different pivots is possible in principle but requires an exact repeat, which is a much rarer
	// coincidence than an index shift that happens on every single evaluation once the window fills.
	tradedLowPrice  decimal.Decimal
	tradedHighPrice decimal.Decimal
	hasTradedLow    bool
	hasTradedHigh   bool
}

func NewZigZagPA() *ZigZagPA {
	return &ZigZagPA{
		Depth:      5,
		RiskReward: decimal.NewFromFloat(1.5),
	}
}

func (s *ZigZagPA) Name() string { return "zigzag_pa" }

func (s *ZigZagPA) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "depth", Default: decimal.NewFromInt(int64(s.Depth)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(30)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *ZigZagPA) resetState() {
	s.tradedLowPrice, s.tradedHighPrice = decimal.Zero, decimal.Zero
	s.hasTradedLow, s.hasTradedHigh = false, false
}

func (s *ZigZagPA) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["depth"]; ok {
		cp.Depth = int(ClampParam(spec["depth"], v).IntPart())
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *ZigZagPA) Evaluate(candles []Candle) (Signal, error) {
	need := 2*s.Depth + 10
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	pivots := ZigZagPivots(candles, s.Depth)

	var lows, highs []ZigZagPivot
	for _, p := range pivots {
		if p.High {
			highs = append(highs, p)
		} else {
			lows = append(lows, p)
		}
	}
	last := candles[len(candles)-1]

	// Higher low: the most recent confirmed swing low sits above the one before it, and this
	// specific pivot pair hasn't already triggered a trade.
	if n := len(lows); n >= 2 {
		latestLow, priorLow := lows[n-1], lows[n-2]
		alreadyTraded := s.hasTradedLow && s.tradedLowPrice.Equal(latestLow.Price)
		if !alreadyTraded && latestLow.Price.GreaterThan(priorLow.Price) && len(highs) > 0 {
			recentHigh := highs[len(highs)-1]
			if last.Close.GreaterThan(recentHigh.Price) {
				risk := last.Close.Sub(latestLow.Price)
				if risk.IsPositive() {
					s.tradedLowPrice, s.hasTradedLow = latestLow.Price, true
					return Signal{
						Side:       Buy,
						Confidence: decimal.NewFromFloat(0.6),
						EntryPx:    last.Close,
						SLPx:       latestLow.Price,
						TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
						SLPct:      risk.Div(last.Close),
						TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
					}, nil
				}
			}
		}
	}

	// Lower high: mirrored.
	if n := len(highs); n >= 2 {
		latestHigh, priorHigh := highs[n-1], highs[n-2]
		alreadyTraded := s.hasTradedHigh && s.tradedHighPrice.Equal(latestHigh.Price)
		if !alreadyTraded && latestHigh.Price.LessThan(priorHigh.Price) && len(lows) > 0 {
			recentLow := lows[len(lows)-1]
			if last.Close.LessThan(recentLow.Price) {
				risk := latestHigh.Price.Sub(last.Close)
				if risk.IsPositive() {
					s.tradedHighPrice, s.hasTradedHigh = latestHigh.Price, true
					return Signal{
						Side:       Sell,
						Confidence: decimal.NewFromFloat(0.6),
						EntryPx:    last.Close,
						SLPx:       latestHigh.Price,
						TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
						SLPct:      risk.Div(last.Close),
						TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
					}, nil
				}
			}
		}
	}

	return Signal{Side: Hold}, nil
}
