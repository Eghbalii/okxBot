package strategy

import "github.com/shopspring/decimal"

// SweepReverse trades a liquidity sweep that REJECTS — price wicks through a prior swing level and
// closes back inside — behind three filters the existing ict_liquidity_sweep has none of.
//
// Ported from the TradingView strategy "Sweep & Reverse" (2026-09-14). The pattern itself is not
// new here; ict_liquidity_sweep already trades it and scored t=+0.86 against the coin-flip baseline
// over 3,987 trades — positive, but indistinguishable from chance. What this adds is the three
// gates, which makes it a direct test of the hypothesis the whole screening pointed at: that the
// edge is conditional, and the roster loses it by trading the pattern unconditionally.
//
//  1. VOLUME. A sweep on thin volume is nobody being stopped out; it is a wick. Real liquidity
//     being taken shows up as participation.
//  2. REJECTION WICK. The wick must be meaningfully larger than the body, which is what separates
//     "price probed the level and was pushed back" from "price traded through it and paused".
//  3. NEXT-BAR CONFIRMATION. Enter only after price continues in the reversal direction. This costs
//     one bar of entry price and refuses every sweep that merely paused — and the original author's
//     own warning is the reason it matters: in a strong trend, sweeps CONTINUE rather than reverse.
//
// Each is independently switchable so the backtest can attribute any improvement to a specific
// filter rather than to the bundle. That is the whole point of porting this one: if filtering is
// what turns a near-zero edge into a real one, this measures which filter does it.
type SweepReverse struct {
	// SwingLookback is how far back a pivot must be the extreme to count as a swing level.
	SwingLookback int
	// LevelMaxAge bounds how long a swept level stays tradeable. An ancient level is not where
	// today's stops are resting, and without this the strategy would keep firing on structure the
	// market has long since forgotten.
	LevelMaxAge int

	RequireVolume    bool
	MinVolumeRatio   decimal.Decimal
	RequireWick      bool
	MinWickBodyRatio decimal.Decimal
	RequireNextBar   bool

	ATRPeriod  int
	StopATRPad decimal.Decimal
	RewardRisk decimal.Decimal

	// pending holds a sweep awaiting next-bar confirmation.
	pending *sweepSetup
	// lastFiredBar prevents one sweep producing an entry on every following bar — the re-arming bug
	// §30.1 found in ict_fvg, where a single setup produced ten entries.
	lastFiredBar int
	bar          int
}

type sweepSetup struct {
	side   Side
	level  decimal.Decimal
	wickPx decimal.Decimal // the extreme of the sweeping wick — where the stop goes
	atBar  int
}

func NewSweepReverse() *SweepReverse {
	return &SweepReverse{
		SwingLookback:    10,
		LevelMaxAge:      50,
		RequireVolume:    true,
		MinVolumeRatio:   decimal.NewFromFloat(1.3),
		RequireWick:      true,
		MinWickBodyRatio: decimal.NewFromFloat(1.5),
		RequireNextBar:   true,
		ATRPeriod:        14,
		StopATRPad:       decimal.NewFromFloat(0.25),
		RewardRisk:       decimal.NewFromFloat(2),
	}
}

func (s *SweepReverse) Name() string { return "sweep_reverse" }

func (s *SweepReverse) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.SwingLookback*2+s.ATRPeriod+2 {
		return Signal{Side: Hold}, nil
	}
	s.bar = len(candles)
	last := candles[len(candles)-1]

	// A pending sweep either confirms on this bar or is dropped. Carrying it further would turn a
	// one-bar confirmation into an open-ended one, which is a different rule.
	if s.pending != nil {
		p := s.pending
		s.pending = nil
		if s.bar-p.atBar == 1 && s.confirms(p, last) {
			return s.enter(candles, p, last)
		}
	}

	setup, ok := s.detectSweep(candles)
	if !ok {
		return Signal{Side: Hold}, nil
	}
	if s.RequireNextBar {
		s.pending = setup
		return Signal{Side: Hold}, nil
	}
	return s.enter(candles, setup, last)
}

// detectSweep finds a bar that wicked through a swing level and closed back inside it.
func (s *SweepReverse) detectSweep(candles []Candle) (*sweepSetup, bool) {
	n := len(candles)
	cur := candles[n-1]

	// Only fire once per bar, and never twice on the same setup.
	if s.bar <= s.lastFiredBar {
		return nil, false
	}

	// Look for the most recent swing high and low, excluding the bars that would make the current
	// candle its own pivot.
	lb := s.SwingLookback
	end := n - 1 - lb
	if end <= lb {
		return nil, false
	}
	start := end - s.LevelMaxAge
	if start < lb {
		start = lb
	}

	for i := end; i >= start; i-- {
		if isSwingHigh(candles, i, lb) {
			// A sweep of a swing HIGH that closes back below it is a short setup.
			if cur.High.GreaterThan(candles[i].High) && cur.Close.LessThan(candles[i].High) {
				if s.passesFilters(candles, cur, Sell) {
					return &sweepSetup{side: Sell, level: candles[i].High, wickPx: cur.High, atBar: s.bar}, true
				}
			}
			break
		}
	}
	for i := end; i >= start; i-- {
		if isSwingLow(candles, i, lb) {
			if cur.Low.LessThan(candles[i].Low) && cur.Close.GreaterThan(candles[i].Low) {
				if s.passesFilters(candles, cur, Buy) {
					return &sweepSetup{side: Buy, level: candles[i].Low, wickPx: cur.Low, atBar: s.bar}, true
				}
			}
			break
		}
	}
	return nil, false
}

// passesFilters applies the volume and rejection-wick gates.
func (s *SweepReverse) passesFilters(candles []Candle, cur Candle, side Side) bool {
	if s.RequireVolume {
		avg, err := AvgVolume(candles, 20)
		if err != nil || !avg.IsPositive() {
			// Cannot verify participation. Refusing is the honest answer for a filter — "I cannot
			// tell" is not "yes" — and matches RegimeFilter's handling of an unclassifiable window.
			return false
		}
		if cur.Volume.Div(avg).LessThan(s.MinVolumeRatio) {
			return false
		}
	}

	if s.RequireWick {
		body := cur.Close.Sub(cur.Open).Abs()
		var wick decimal.Decimal
		if side == Sell {
			wick = cur.High.Sub(decimal.Max(cur.Open, cur.Close))
		} else {
			wick = decimal.Min(cur.Open, cur.Close).Sub(cur.Low)
		}
		if !wick.IsPositive() {
			return false
		}
		// A doji has a near-zero body, which makes the ratio enormous and meaningless. Treating a
		// zero body as passing is correct — a bar that is ALL wick is the strongest rejection there
		// is — but the division has to be avoided rather than the case excluded.
		if body.IsPositive() && wick.Div(body).LessThan(s.MinWickBodyRatio) {
			return false
		}
	}
	return true
}

// confirms checks that the bar after the sweep continued in the reversal direction.
func (s *SweepReverse) confirms(p *sweepSetup, cur Candle) bool {
	if p.side == Buy {
		return cur.Close.GreaterThan(p.level)
	}
	return cur.Close.LessThan(p.level)
}

func (s *SweepReverse) enter(candles []Candle, p *sweepSetup, cur Candle) (Signal, error) {
	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil || !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	pad := atr.Mul(s.StopATRPad)

	// The stop sits just beyond the sweeping wick — the price that proved the level held. A stop
	// inside it would be taken out by a retest of the very move the trade is based on.
	var sl decimal.Decimal
	if p.side == Buy {
		sl = p.wickPx.Sub(pad)
	} else {
		sl = p.wickPx.Add(pad)
	}
	dist := cur.Close.Sub(sl).Abs()
	if !dist.IsPositive() {
		return Signal{Side: Hold}, nil
	}

	s.lastFiredBar = s.bar
	sig := Signal{Side: p.side, Confidence: decimal.NewFromFloat(0.6), EntryPx: cur.Close, SLPx: sl}
	if p.side == Buy {
		sig.TPPx = cur.Close.Add(dist.Mul(s.RewardRisk))
	} else {
		sig.TPPx = cur.Close.Sub(dist.Mul(s.RewardRisk))
	}
	return sig, nil
}

func isSwingHigh(candles []Candle, i, lb int) bool {
	if i-lb < 0 || i+lb >= len(candles) {
		return false
	}
	h := candles[i].High
	for j := i - lb; j <= i+lb; j++ {
		if j != i && candles[j].High.GreaterThanOrEqual(h) {
			return false
		}
	}
	return true
}

func isSwingLow(candles []Candle, i, lb int) bool {
	if i-lb < 0 || i+lb >= len(candles) {
		return false
	}
	l := candles[i].Low
	for j := i - lb; j <= i+lb; j++ {
		if j != i && candles[j].Low.LessThanOrEqual(l) {
			return false
		}
	}
	return true
}

func (s *SweepReverse) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "swing_lookback", Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(30), Default: decimal.NewFromInt(10)},
		{Name: "level_max_age", Min: decimal.NewFromInt(10), Max: decimal.NewFromInt(200), Default: decimal.NewFromInt(50)},
		{Name: "min_volume_ratio", Min: decimal.NewFromFloat(1), Max: decimal.NewFromFloat(4), Default: decimal.NewFromFloat(1.3)},
		{Name: "min_wick_body_ratio", Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromFloat(5), Default: decimal.NewFromFloat(1.5)},
		{Name: "stop_atr_pad", Min: decimal.Zero, Max: decimal.NewFromFloat(1.5), Default: decimal.NewFromFloat(0.25)},
		{Name: "reward_risk", Min: decimal.NewFromFloat(1), Max: decimal.NewFromFloat(5), Default: decimal.NewFromFloat(2)},
	}
}

func (s *SweepReverse) WithParams(p map[string]decimal.Decimal) Strategy {
	out := NewSweepReverse()
	out.RequireVolume, out.RequireWick, out.RequireNextBar = s.RequireVolume, s.RequireWick, s.RequireNextBar
	if v, ok := p["swing_lookback"]; ok {
		out.SwingLookback = int(v.IntPart())
	}
	if v, ok := p["level_max_age"]; ok {
		out.LevelMaxAge = int(v.IntPart())
	}
	if v, ok := p["min_volume_ratio"]; ok {
		out.MinVolumeRatio = v
	}
	if v, ok := p["min_wick_body_ratio"]; ok {
		out.MinWickBodyRatio = v
	}
	if v, ok := p["stop_atr_pad"]; ok {
		out.StopATRPad = v
	}
	if v, ok := p["reward_risk"]; ok {
		out.RewardRisk = v
	}
	// pending/lastFiredBar/bar deliberately not copied (§16.8).
	return out
}
