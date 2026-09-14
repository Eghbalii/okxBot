package strategy

import "github.com/shopspring/decimal"

// TrendShift is a Supertrend whose sensitivity ADAPTS to the market regime, and which declines to
// trade at all when the market is chopping.
//
// Ported from the TradingView strategy "TrendShift — Supertrend + ADX Regime-Adaptive" (2026-09-14,
// at the operator's request). Two ideas here are genuinely absent from all 41 existing kinds:
//
//  1. IT REFUSES TO TRADE IN THE WRONG REGIME. Every existing strategy fires whenever its pattern
//     appears; screening them over 73,626 trades found none whose edge survived its own sample size,
//     which is what trading a trend pattern through a range looks like in aggregate.
//
//  2. THE STOP WIDTH ADAPTS. A tight ATR multiple works in a clean trend and gets stopped out by
//     noise in a range; a wide one survives the range and gives back too much in the trend. One
//     fixed multiplier is wrong in one regime by construction, which no existing kind accounts for.
//
// HYSTERESIS is the detail worth keeping from the original. A single ADX threshold makes the regime
// flicker bar to bar whenever ADX sits near it — and a strategy whose stop width flickers is worse
// than one with a fixed width, because the level moves for reasons that have nothing to do with
// price. Two thresholds with a sticky state in between is what stops that.
type TrendShift struct {
	ATRPeriod int
	ADXPeriod int

	// TrendMult is the ATR multiplier in a trending market — tight, because a real trend should not
	// retrace into it. ChopMult is the multiplier in a range — wide, to sit outside the noise.
	TrendMult decimal.Decimal
	ChopMult  decimal.Decimal

	// EnterTrend is the ADX level at which the market is declared trending; ExitTrend the level it
	// must fall back below to be declared ranging again. EnterTrend > ExitTrend gives the state its
	// stickiness — the gap between them is the hysteresis band.
	EnterTrend decimal.Decimal
	ExitTrend  decimal.Decimal

	// SuppressInChop stops the strategy trading at all while the regime reads choppy. This is the
	// gap the whole roster shares, and the reason this strategy was ported first.
	SuppressInChop bool

	// RewardRisk sets the target as a multiple of the stop distance, which is itself the distance
	// to the Supertrend band — so both levels come from the same volatility measure rather than one
	// being structural and the other a fixed percentage.
	RewardRisk decimal.Decimal

	// --- evaluation state ---
	trending bool            // the hysteresis state
	dir      int             // +1 long band, -1 short band; 0 until initialized
	band     decimal.Decimal // the current trailing band (the Supertrend line itself)
	prevDir  int
}

func NewTrendShift() *TrendShift {
	return &TrendShift{
		ATRPeriod: 10,
		ADXPeriod: 14,
		TrendMult: decimal.NewFromFloat(1.75),
		ChopMult:  decimal.NewFromFloat(4.5),
		// 35/25, not the original's 25/20 — MEASURED on this project's own market, not carried over.
		//
		// The TradingView defaults were written for daily charts. On real 5m candles (SOL and BTC,
		// 1,440 readings each) ADX has a MEDIAN of 32 and sits at or above 25 on 76% of bars, so a
		// 25 threshold would pass three-quarters of the market and gate almost nothing — a filter in
		// name only, which is the failure mode every one of these ports was chosen to avoid.
		//
		// 35 admits the top ~40%. The hysteresis band (35 down to 25) is wider in absolute terms
		// than the original's 5 points because the distribution itself is wider here: p25=25, p90=50.
		EnterTrend:     decimal.NewFromInt(35),
		ExitTrend:      decimal.NewFromInt(25),
		SuppressInChop: true,
		RewardRisk:     decimal.NewFromFloat(2),
	}
}

func (t *TrendShift) Name() string { return "trendshift" }

func (t *TrendShift) Evaluate(candles []Candle) (Signal, error) {
	need := t.ADXPeriod*2 + 2
	if t.ATRPeriod+2 > need {
		need = t.ATRPeriod + 2
	}
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	atr, err := ATR(candles, t.ATRPeriod)
	if err != nil || !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	adx, err := ADX(candles, t.ADXPeriod)
	if err != nil {
		return Signal{Side: Hold}, nil
	}

	// Hysteresis: cross ABOVE EnterTrend to become trending, fall BELOW ExitTrend to stop being so.
	// Between the two the state simply persists, which is what stops the regime — and therefore the
	// stop width — flickering while ADX hovers near one number.
	switch {
	case adx.GreaterThanOrEqual(t.EnterTrend):
		t.trending = true
	case adx.LessThanOrEqual(t.ExitTrend):
		t.trending = false
	}

	mult := t.ChopMult
	if t.trending {
		mult = t.TrendMult
	}

	last := candles[len(candles)-1]
	mid := last.High.Add(last.Low).Div(decimal.NewFromInt(2))
	offset := atr.Mul(mult)
	upper := mid.Add(offset)
	lower := mid.Sub(offset)

	// Supertrend's ratchet: while the direction holds, the band only ever moves TOWARD price. A band
	// that could retreat would follow price back down in a pullback and stop protecting anything.
	if t.dir == 0 {
		if last.Close.GreaterThan(mid) {
			t.dir, t.band = 1, lower
		} else {
			t.dir, t.band = -1, upper
		}
		t.prevDir = t.dir
		return Signal{Side: Hold}, nil
	}

	t.prevDir = t.dir
	if t.dir > 0 {
		if lower.GreaterThan(t.band) {
			t.band = lower
		}
		if last.Close.LessThan(t.band) {
			t.dir, t.band = -1, upper
		}
	} else {
		if upper.LessThan(t.band) {
			t.band = upper
		}
		if last.Close.GreaterThan(t.band) {
			t.dir, t.band = 1, lower
		}
	}

	// The signal is the FLIP, not the state: holding a direction is not a reason to enter again.
	if t.dir == t.prevDir {
		return Signal{Side: Hold}, nil
	}

	// The regime gate. Deliberately checked AFTER the band update so the trend state stays current
	// through a choppy stretch — a band that stopped updating would come back stale and produce a
	// flip that reflects the gap rather than the market.
	if t.SuppressInChop && !t.trending {
		return Signal{Side: Hold}, nil
	}

	side := Buy
	if t.dir < 0 {
		side = Sell
	}

	// The stop IS the Supertrend band — the strategy's own statement of where it would be wrong —
	// and the target is a multiple of that same distance. Emitting prices rather than percentages
	// keeps the structure §16.8's audit found the other strategies discarding.
	stopDist := last.Close.Sub(t.band).Abs()
	if !stopDist.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	reward := stopDist.Mul(t.RewardRisk)

	sig := Signal{Side: side, Confidence: regimeConfidence(adx), EntryPx: last.Close}
	if side == Buy {
		sig.SLPx = last.Close.Sub(stopDist)
		sig.TPPx = last.Close.Add(reward)
	} else {
		sig.SLPx = last.Close.Add(stopDist)
		sig.TPPx = last.Close.Sub(reward)
	}
	return sig, nil
}

// regimeConfidence scales with trend strength, normalized so a very strong reading does not run
// away: ADX is unbounded above in principle, and a confidence of 4.0 means nothing downstream.
func regimeConfidence(adx decimal.Decimal) decimal.Decimal {
	c := adx.Div(decimal.NewFromInt(50))
	if c.GreaterThan(decimal.NewFromInt(1)) {
		return decimal.NewFromInt(1)
	}
	return c
}

func (t *TrendShift) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "atr_period", Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(30), Default: decimal.NewFromInt(10)},
		{Name: "adx_period", Min: decimal.NewFromInt(7), Max: decimal.NewFromInt(30), Default: decimal.NewFromInt(14)},
		{Name: "trend_mult", Min: decimal.NewFromFloat(1), Max: decimal.NewFromFloat(4), Default: decimal.NewFromFloat(1.75)},
		{Name: "chop_mult", Min: decimal.NewFromFloat(2), Max: decimal.NewFromFloat(8), Default: decimal.NewFromFloat(4.5)},
		{Name: "enter_trend", Min: decimal.NewFromInt(20), Max: decimal.NewFromInt(55), Default: decimal.NewFromInt(35)},
		{Name: "exit_trend", Min: decimal.NewFromInt(15), Max: decimal.NewFromInt(45), Default: decimal.NewFromInt(25)},
		{Name: "reward_risk", Min: decimal.NewFromFloat(1), Max: decimal.NewFromFloat(5), Default: decimal.NewFromFloat(2)},
	}
}

func (t *TrendShift) WithParams(p map[string]decimal.Decimal) Strategy {
	out := NewTrendShift()
	out.SuppressInChop = t.SuppressInChop
	if v, ok := p["atr_period"]; ok {
		out.ATRPeriod = int(v.IntPart())
	}
	if v, ok := p["adx_period"]; ok {
		out.ADXPeriod = int(v.IntPart())
	}
	if v, ok := p["trend_mult"]; ok {
		out.TrendMult = v
	}
	if v, ok := p["chop_mult"]; ok {
		out.ChopMult = v
	}
	if v, ok := p["enter_trend"]; ok {
		out.EnterTrend = v
	}
	if v, ok := p["exit_trend"]; ok {
		out.ExitTrend = v
	}
	if v, ok := p["reward_risk"]; ok {
		out.RewardRisk = v
	}
	// A degenerate band (exit above enter) would invert the hysteresis into a state that can never
	// settle. Repaired rather than rejected, matching rsi_sma_fuzzy's handling of an inverted zone.
	if out.ExitTrend.GreaterThanOrEqual(out.EnterTrend) {
		out.ExitTrend = out.EnterTrend.Mul(decimal.NewFromFloat(0.8))
	}
	// Evaluation state (trending/dir/band) is deliberately NOT copied — §16.8's audit found 7 of 14
	// strategies leaking accumulated state into a differently-configured variant this way.
	return out
}
