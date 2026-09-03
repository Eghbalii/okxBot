package strategy

import (
	"time"

	"github.com/shopspring/decimal"
)

// ICTFairValueGap trades the "Fair Value Gap" (FVG) concept from ICT/Smart-Money price action: a
// 3-candle imbalance where candle 1's high sits below candle 3's low (a bullish FVG — the market
// left a gap because it moved too fast for two-sided trading to occur) or candle 1's low sits
// above candle 3's high (bearish FVG). Price frequently returns to "fill" part of that gap before
// continuing in the original direction — this strategy watches for that: an FVG forms, and price
// later trades back into the gap's zone, which is treated as an entry in the FVG's original
// direction (continuation, not reversal — the gap acted as support/resistance).
//
// The gap's own boundary is the structural stop (a return past the far edge invalidates the
// premise that it was ever real support/resistance), and the target is placed at RiskReward
// multiples of that risk — well suited to 5m scalping since FVGs form and get revisited within a
// handful of candles on a fast timeframe.
type ICTFairValueGap struct {
	LookbackBars int // how many recent candles to scan for an unfilled FVG
	RiskReward   decimal.Decimal

	// The active gap, if any, identified by the TIMESTAMP of its third candle. Identity is what
	// makes the rescan safe: without it, re-detecting the same 3-candle triple on the next call
	// re-arms a gap this strategy already traded, and one setup then fires an entry on every
	// subsequent candle for as long as it stays inside LookbackBars.
	//
	// A timestamp rather than a slice index because the caller's window SLIDES — PaperTrader trims
	// to CandleWindow (papertrade.go's applyCandle), so index 5 refers to a different candle once
	// the window is full, and index-based identity would silently start matching the wrong bar.
	//
	// gapBull distinguishes a bullish zone [c1.High, c3.Low] from a bearish one [c3.High, c1.Low];
	// only the most recent gap is tracked, matching how a discretionary trader works the freshest
	// imbalance rather than a stale one.
	hasGap  bool
	gapBull bool
	gapLow  decimal.Decimal
	gapHigh decimal.Decimal
	gapAt   time.Time

	// handledAt is the gapAt of the last gap traded or invalidated, so the rescan never re-arms
	// it. Zero means nothing has been handled yet.
	handledAt time.Time
}

func NewICTFairValueGap() *ICTFairValueGap {
	return &ICTFairValueGap{
		LookbackBars: 30,
		RiskReward:   decimal.NewFromFloat(2),
	}
}

func (s *ICTFairValueGap) Name() string { return "ict_fvg" }

func (s *ICTFairValueGap) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "lookback_bars", Default: decimal.NewFromInt(int64(s.LookbackBars)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *ICTFairValueGap) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["lookback_bars"]; ok {
		cp.LookbackBars = int(ClampParam(specByName["lookback_bars"], v).IntPart())
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(specByName["risk_reward"], v)
	}
	cp.resetState()
	return &cp
}

func (s *ICTFairValueGap) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < 3 {
		return Signal{Side: Hold}, nil
	}
	last := len(candles) - 1

	// Scan the trailing window newest-first for an imbalance strictly newer than both the
	// currently-armed gap and the last one handled, so an already-traded or still-tracked setup is
	// never re-armed by the rescan.
	start := len(candles) - s.LookbackBars
	if start < 0 {
		start = 0
	}
	for i := last; i >= start+2; i-- {
		at := candles[i].Timestamp
		if !at.After(s.handledAt) {
			break // this and everything older was already traded or invalidated
		}
		if s.hasGap && !at.After(s.gapAt) {
			break // already tracking this gap or an older one; leave its state alone
		}
		c1, c3 := candles[i-2], candles[i]
		if c1.High.LessThan(c3.Low) {
			s.hasGap, s.gapBull, s.gapLow, s.gapHigh, s.gapAt = true, true, c1.High, c3.Low, at
			break
		}
		if c1.Low.GreaterThan(c3.High) {
			s.hasGap, s.gapBull, s.gapLow, s.gapHigh, s.gapAt = true, false, c3.High, c1.Low, at
			break
		}
	}

	if !s.hasGap {
		return Signal{Side: Hold}, nil
	}

	c := candles[last]

	// A gap is only tradeable on a candle AFTER the one that formed it. The entry premise is that
	// price RETURNED to the zone, and the forming candle is the move that created it — testing it
	// against its own zone is trivially true (a bullish gap's upper edge IS that candle's low), so
	// this used to enter at the top of the impulse, the worst price of the whole setup, rather
	// than on the pullback the strategy exists to trade.
	if !c.Timestamp.After(s.gapAt) {
		return Signal{Side: Hold}, nil
	}

	if s.gapBull {
		switch {
		case c.Low.LessThan(s.gapLow):
			// Price traded through the gap's far edge — it was not support after all.
			s.hasGap, s.handledAt = false, s.gapAt
		case c.Low.LessThanOrEqual(s.gapHigh):
			// Price returned into the zone — take the continuation entry.
			s.hasGap, s.handledAt = false, s.gapAt
			risk := c.Close.Sub(s.gapLow)
			if risk.IsPositive() {
				return Signal{
					Side:       Buy,
					Confidence: decimal.NewFromFloat(0.6),
					EntryPx:    c.Close,
					SLPx:       s.gapLow,
					TPPx:       c.Close.Add(risk.Mul(s.RiskReward)),
					SLPct:      risk.Div(c.Close),
					TPPct:      risk.Div(c.Close).Mul(s.RiskReward),
				}, nil
			}
		}
		return Signal{Side: Hold}, nil
	}

	switch {
	case c.High.GreaterThan(s.gapHigh):
		s.hasGap, s.handledAt = false, s.gapAt
	case c.High.GreaterThanOrEqual(s.gapLow):
		s.hasGap, s.handledAt = false, s.gapAt
		risk := s.gapHigh.Sub(c.Close)
		if risk.IsPositive() {
			return Signal{
				Side:       Sell,
				Confidence: decimal.NewFromFloat(0.6),
				EntryPx:    c.Close,
				SLPx:       s.gapHigh,
				TPPx:       c.Close.Sub(risk.Mul(s.RiskReward)),
				SLPct:      risk.Div(c.Close),
				TPPct:      risk.Div(c.Close).Mul(s.RiskReward),
			}, nil
		}
	}
	return Signal{Side: Hold}, nil
}

// resetState clears accumulated evaluation state, returning the strategy to how it behaves when
// freshly constructed. Called by WithParams, whose copy must not inherit it (see
// Strategy.WithParams for why).
//
// This lives beside the state fields on purpose: it is the one place that has to know what they
// are, so adding a field means updating the reset right here rather than remembering a zeroing
// line buried at the bottom of WithParams.
func (s *ICTFairValueGap) resetState() {
	s.hasGap, s.gapBull = false, false
	s.gapLow, s.gapHigh = decimal.Zero, decimal.Zero
	s.gapAt, s.handledAt = time.Time{}, time.Time{}
}
