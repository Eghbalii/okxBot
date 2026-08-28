package strategy

import "github.com/shopspring/decimal"

// WeeklyDipBuy is a Go port of "TASC 2026.03 One Percent A Week" by Dion Kurczek, provided by
// PineCoders (pinescript/strategy_TASC 2026.03 One Percent A Week.pine, TradingView Pine Script
// v6 — no separate license stated beyond the standard TASC/PineCoders attribution). Designed for
// a weekly-bar chart: on Monday, arm a limit buy DipPct below Monday's open; once filled, target
// a gain, or break even if price drops BreakevenPct against entry; force-close by Friday.
// Evaluate can only emit a signal, not place a resting limit order the way the Pine
// strategy.entry(limit=...) does — so this only signals Buy when the current candle's Low has
// actually traded down through the dip level from the week's open, which is the closest
// equivalent to "the limit order would have filled" that a candle-only Evaluate can determine.
// The "move stop to breakeven after -0.5%" and "force close Friday" rules are position-lifecycle
// behavior (like SteppedTrailing/EMACrossTrailing) and aren't expressible as a static SLPct/
// TPPct — only the initial target/stop are carried here.
type WeeklyDipBuy struct {
	DipPct       decimal.Decimal // buy this far below Monday's open
	TargetPct    decimal.Decimal // initial take-profit, % above entry
	BreakevenPct decimal.Decimal // move stop to breakeven if price falls this far against entry
}

func NewWeeklyDipBuy() *WeeklyDipBuy {
	return &WeeklyDipBuy{
		DipPct:       decimal.NewFromFloat(0.01),
		TargetPct:    decimal.NewFromFloat(0.01),
		BreakevenPct: decimal.NewFromFloat(0.005),
	}
}

func (s *WeeklyDipBuy) Name() string { return "weekly_dip_buy" }

func (s *WeeklyDipBuy) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "dip_pct", Default: s.DipPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.1)},
		{Name: "target_pct", Default: s.TargetPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.1)},
		{Name: "breakeven_pct", Default: s.BreakevenPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.1)},
	}
}

func (s *WeeklyDipBuy) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["dip_pct"]; ok {
		cp.DipPct = ClampParam(specByName["dip_pct"], v)
	}
	if v, ok := values["target_pct"]; ok {
		cp.TargetPct = ClampParam(specByName["target_pct"], v)
	}
	if v, ok := values["breakeven_pct"]; ok {
		cp.BreakevenPct = ClampParam(specByName["breakeven_pct"], v)
	}
	return &cp
}

func (s *WeeklyDipBuy) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < 2 {
		return Signal{Side: Hold}, nil
	}
	last := candles[len(candles)-1]

	weekOpen, ok := s.currentWeekOpen(candles)
	if !ok {
		return Signal{Side: Hold}, nil
	}

	dipLevel := weekOpen.Mul(decimal.NewFromInt(1).Sub(s.DipPct))
	if last.Low.GreaterThan(dipLevel) {
		return Signal{Side: Hold}, nil // dip level not reached this bar
	}
	// dipLevel is the limit price this strategy buys at — the bar only has to trade down THROUGH it
	// (the trigger is last.Low), so the close is often well above it. Reporting the level as the
	// entry (CLAUDE.md §15.11) describes the intended fill rather than where the bar happened to
	// settle, and it anchors the breakeven stop and target to that same price.
	return Signal{
		Side:       Buy,
		Confidence: decimal.NewFromFloat(0.5),
		EntryPx:    dipLevel,
		SLPct:      s.BreakevenPct,
		TPPct:      s.TargetPct,
	}, nil
}

// currentWeekOpen finds the Open of the first candle in the current ISO week.
func (s *WeeklyDipBuy) currentWeekOpen(candles []Candle) (decimal.Decimal, bool) {
	_, curWeek := candles[len(candles)-1].Timestamp.ISOWeek()
	for i := len(candles) - 1; i >= 0; i-- {
		_, week := candles[i].Timestamp.ISOWeek()
		if week != curWeek {
			return candles[i+1].Open, true
		}
	}
	return candles[0].Open, true
}
