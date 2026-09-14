package strategy

import "github.com/shopspring/decimal"

// GradientRibbon enters when an EMA ribbon is FANNING OPEN — each faster line sloping harder than
// the one below it — rather than merely being stacked in order.
//
// Ported from the TradingView strategy "Gradient Ribbon" (2026-09-14). The distinction from
// ema_ribbon_pullback, which already exists here and scored t=+1.16 (inconclusive), is the whole
// point: ORDER is a state a trend leaves behind and keeps for as long as it takes to unwind, while
// ACCELERATION is a thing happening now. A ribbon can stay perfectly stacked through an entire
// stall, and a strategy reading order alone cannot tell that apart from the move that created it.
//
// Two filters come with it, and both are gaps in the existing roster: RSI bounded on BOTH sides
// (weak momentum is not worth taking, and an extreme reading is where a move ends rather than
// begins) and a volume floor, since a fan opening on thin participation is the classic false start.
//
// The exit idea is also worth keeping: it closes when the fan COMPRESSES back toward parallel,
// ahead of an actual cross. By the time fast and slow lines cross, most of the move is gone — the
// crossover is the last confirmation, not the first warning. This engine takes exits from SL/TP
// rather than a strategy signal, so that is expressed as a tighter target than a crossover strategy
// would use, and noted here because it is the part that does not port cleanly.
type GradientRibbon struct {
	// Periods are the ribbon's EMAs, fastest first. The default 8/13/21/34/55 is the original's.
	Periods []int
	// SlopeBars is how many bars each line's slope is measured over.
	SlopeBars int

	// MinFanSpread is how much the slopes must differ, as a fraction of price, for the fan to count
	// as opening rather than drifting.
	MinFanSpread decimal.Decimal

	RSIPeriod int
	// RSI is bounded on BOTH sides: below MinRSI the momentum is not there, above MaxRSI the move
	// is already extended. A one-sided bound would take every blow-off top as a buy.
	MinRSI decimal.Decimal
	MaxRSI decimal.Decimal

	MinVolumeRatio decimal.Decimal

	ATRPeriod  int
	StopATR    decimal.Decimal
	RewardRisk decimal.Decimal

	lastFiredBar int
	bar          int
}

func NewGradientRibbon() *GradientRibbon {
	return &GradientRibbon{
		Periods:        []int{8, 13, 21, 34, 55},
		SlopeBars:      3,
		MinFanSpread:   decimal.NewFromFloat(0.0004),
		RSIPeriod:      14,
		MinRSI:         decimal.NewFromInt(50),
		MaxRSI:         decimal.NewFromInt(75),
		MinVolumeRatio: decimal.NewFromFloat(1.1),
		ATRPeriod:      14,
		StopATR:        decimal.NewFromFloat(1.5),
		RewardRisk:     decimal.NewFromFloat(2),
	}
}

func (g *GradientRibbon) Name() string { return "gradient_ribbon" }

func (g *GradientRibbon) Evaluate(candles []Candle) (Signal, error) {
	slowest := 0
	for _, p := range g.Periods {
		if p > slowest {
			slowest = p
		}
	}
	if len(candles) < slowest+g.SlopeBars+g.ATRPeriod+2 {
		return Signal{Side: Hold}, nil
	}
	g.bar = len(candles)
	if g.bar <= g.lastFiredBar {
		return Signal{Side: Hold}, nil
	}

	last := candles[len(candles)-1]
	if !last.Close.IsPositive() {
		return Signal{Side: Hold}, nil
	}

	// Each line's slope over SlopeBars, as a fraction of price so the comparison is scale-free —
	// the same reason every price in the observation is fed relative (§15.6).
	slopes := make([]decimal.Decimal, 0, len(g.Periods))
	for _, p := range g.Periods {
		now, err1 := EMA(candles, p)
		then, err2 := EMA(candles[:len(candles)-g.SlopeBars], p)
		if err1 != nil || err2 != nil || !then.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		slopes = append(slopes, now.Sub(then).Div(last.Close))
	}

	// FANNING: every slope points the same way, and each faster line slopes harder than the slower
	// one beneath it. That ordering IS the acceleration — the ribbon is spreading, not just aligned.
	up := true
	down := true
	for i := range slopes {
		if !slopes[i].IsPositive() {
			up = false
		}
		if !slopes[i].IsNegative() {
			down = false
		}
		if i > 0 {
			if !slopes[i-1].GreaterThan(slopes[i]) {
				up = false
			}
			if !slopes[i-1].LessThan(slopes[i]) {
				down = false
			}
		}
	}
	if !up && !down {
		return Signal{Side: Hold}, nil
	}

	// The spread between fastest and slowest must be wide enough to be a fan rather than five lines
	// drifting together.
	spread := slopes[0].Sub(slopes[len(slopes)-1]).Abs()
	if spread.LessThan(g.MinFanSpread) {
		return Signal{Side: Hold}, nil
	}

	rsi, err := RSI(candles, g.RSIPeriod)
	if err != nil {
		return Signal{Side: Hold}, nil
	}
	if up {
		if rsi.LessThan(g.MinRSI) || rsi.GreaterThan(g.MaxRSI) {
			return Signal{Side: Hold}, nil
		}
	} else {
		// Mirrored for shorts: 100-MinRSI is the ceiling, 100-MaxRSI the floor.
		hundred := decimal.NewFromInt(100)
		if rsi.GreaterThan(hundred.Sub(g.MinRSI)) || rsi.LessThan(hundred.Sub(g.MaxRSI)) {
			return Signal{Side: Hold}, nil
		}
	}

	if g.MinVolumeRatio.IsPositive() {
		avg, err := AvgVolume(candles, 20)
		if err != nil || !avg.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		if last.Volume.Div(avg).LessThan(g.MinVolumeRatio) {
			return Signal{Side: Hold}, nil
		}
	}

	atr, err := ATR(candles, g.ATRPeriod)
	if err != nil || !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	dist := atr.Mul(g.StopATR)
	reward := dist.Mul(g.RewardRisk)

	g.lastFiredBar = g.bar
	side := Buy
	if down {
		side = Sell
	}
	sig := Signal{Side: side, Confidence: fanConfidence(spread, g.MinFanSpread), EntryPx: last.Close}
	if side == Buy {
		sig.SLPx = last.Close.Sub(dist)
		sig.TPPx = last.Close.Add(reward)
	} else {
		sig.SLPx = last.Close.Add(dist)
		sig.TPPx = last.Close.Sub(reward)
	}
	return sig, nil
}

// fanConfidence scales with how far the spread exceeds its threshold, capped so one violent bar
// cannot report a confidence the rest of the system reads as certainty.
func fanConfidence(spread, min decimal.Decimal) decimal.Decimal {
	if !min.IsPositive() {
		return decimal.NewFromFloat(0.5)
	}
	c := spread.Div(min).Div(decimal.NewFromInt(3))
	if c.GreaterThan(decimal.NewFromInt(1)) {
		return decimal.NewFromInt(1)
	}
	return c
}

func (g *GradientRibbon) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "slope_bars", Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(10), Default: decimal.NewFromInt(3)},
		{Name: "min_fan_spread", Min: decimal.NewFromFloat(0.00005), Max: decimal.NewFromFloat(0.005), Default: decimal.NewFromFloat(0.0004)},
		{Name: "min_rsi", Min: decimal.NewFromInt(40), Max: decimal.NewFromInt(65), Default: decimal.NewFromInt(50)},
		{Name: "max_rsi", Min: decimal.NewFromInt(65), Max: decimal.NewFromInt(90), Default: decimal.NewFromInt(75)},
		{Name: "min_volume_ratio", Min: decimal.Zero, Max: decimal.NewFromFloat(3), Default: decimal.NewFromFloat(1.1)},
		{Name: "stop_atr", Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromFloat(4), Default: decimal.NewFromFloat(1.5)},
		{Name: "reward_risk", Min: decimal.NewFromFloat(1), Max: decimal.NewFromFloat(5), Default: decimal.NewFromFloat(2)},
	}
}

func (g *GradientRibbon) WithParams(p map[string]decimal.Decimal) Strategy {
	out := NewGradientRibbon()
	out.Periods = append([]int(nil), g.Periods...)
	if v, ok := p["slope_bars"]; ok {
		out.SlopeBars = int(v.IntPart())
	}
	if v, ok := p["min_fan_spread"]; ok {
		out.MinFanSpread = v
	}
	if v, ok := p["min_rsi"]; ok {
		out.MinRSI = v
	}
	if v, ok := p["max_rsi"]; ok {
		out.MaxRSI = v
	}
	if v, ok := p["min_volume_ratio"]; ok {
		out.MinVolumeRatio = v
	}
	if v, ok := p["stop_atr"]; ok {
		out.StopATR = v
	}
	if v, ok := p["reward_risk"]; ok {
		out.RewardRisk = v
	}
	// An inverted RSI band can never be satisfied, so the strategy would go permanently silent —
	// repaired rather than rejected, matching rsi_sma_fuzzy's handling of a degenerate zone.
	if out.MaxRSI.LessThanOrEqual(out.MinRSI) {
		out.MaxRSI = out.MinRSI.Add(decimal.NewFromInt(15))
	}
	// lastFiredBar/bar deliberately not copied (§16.8).
	return out
}
