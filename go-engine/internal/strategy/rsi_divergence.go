package strategy

import "github.com/shopspring/decimal"

// RSIDivergence is a Go port of the widely-known TradingView "RSI Divergence Indicator" strategy
// family (raw PineScript source not extractable via automated fetch; implemented from the
// standard, well-documented regular-divergence detection algorithm).
//
// Standard/default parameters: RSI(14) — the universal RSI default — and a fractal pivot depth of
// 5 bars (matching zigzag_pa.go's own default, a common published choice for divergence-pivot
// detection on lower timeframes).
//
// Signal logic: REGULAR bullish divergence — price makes a LOWER swing low while RSI (measured at
// the same swing-low candle) makes a HIGHER low — signals bullish reversal; regular bearish
// divergence — price makes a HIGHER swing high while RSI makes a LOWER high — signals bearish
// reversal. This is the classic, most widely traded divergence type (as opposed to hidden
// divergence, which is a continuation signal and a different, less commonly requested pattern).
// Stop beyond the triggering swing point (a structural level).
type RSIDivergence struct {
	RSIPeriod  int
	Depth      int
	RiskReward decimal.Decimal

	tradedLowPrice  decimal.Decimal
	tradedHighPrice decimal.Decimal
	hasTradedLow    bool
	hasTradedHigh   bool
}

func NewRSIDivergence() *RSIDivergence {
	return &RSIDivergence{
		RSIPeriod:  14,
		Depth:      5,
		RiskReward: decimal.NewFromFloat(1.5),
	}
}

func (s *RSIDivergence) Name() string { return "rsi_divergence" }

func (s *RSIDivergence) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "rsi_period", Default: decimal.NewFromInt(int64(s.RSIPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "depth", Default: decimal.NewFromInt(int64(s.Depth)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(30)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *RSIDivergence) resetState() {
	s.tradedLowPrice, s.tradedHighPrice = decimal.Zero, decimal.Zero
	s.hasTradedLow, s.hasTradedHigh = false, false
}

func (s *RSIDivergence) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["rsi_period"]; ok {
		cp.RSIPeriod = int(ClampParam(spec["rsi_period"], v).IntPart())
	}
	if v, ok := values["depth"]; ok {
		cp.Depth = int(ClampParam(spec["depth"], v).IntPart())
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *RSIDivergence) Evaluate(candles []Candle) (Signal, error) {
	need := 2*s.Depth + s.RSIPeriod + 10
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

	rsiAt := func(idx int) (decimal.Decimal, bool) {
		if idx+1 < s.RSIPeriod+1 {
			return decimal.Zero, false
		}
		r, err := RSI(candles[:idx+1], s.RSIPeriod)
		if err != nil {
			return decimal.Zero, false
		}
		return r, true
	}

	// Bullish regular divergence: price lower low, RSI higher low.
	if n := len(lows); n >= 2 {
		latestLow, priorLow := lows[n-1], lows[n-2]
		alreadyTraded := s.hasTradedLow && s.tradedLowPrice.Equal(latestLow.Price)
		rsiLatest, ok1 := rsiAt(latestLow.Index)
		rsiPrior, ok2 := rsiAt(priorLow.Index)
		if !alreadyTraded && ok1 && ok2 &&
			latestLow.Price.LessThan(priorLow.Price) && rsiLatest.GreaterThan(rsiPrior) {
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

	// Bearish regular divergence: price higher high, RSI lower high.
	if n := len(highs); n >= 2 {
		latestHigh, priorHigh := highs[n-1], highs[n-2]
		alreadyTraded := s.hasTradedHigh && s.tradedHighPrice.Equal(latestHigh.Price)
		rsiLatest, ok1 := rsiAt(latestHigh.Index)
		rsiPrior, ok2 := rsiAt(priorHigh.Index)
		if !alreadyTraded && ok1 && ok2 &&
			latestHigh.Price.GreaterThan(priorHigh.Price) && rsiLatest.LessThan(rsiPrior) {
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

	return Signal{Side: Hold}, nil
}
