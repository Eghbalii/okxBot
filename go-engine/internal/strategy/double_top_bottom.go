package strategy

import "github.com/shopspring/decimal"

// DoubleTopBottom is a Go port of the core pattern-detection/entry logic from the Pine Script
// "Bjorgum Double Tap" by Bjorgum (pinescript/strategy_Bjorgum Double Tap.pine, MPL 2.0). The
// original's line/label drawing, data table, and third-party alert-webhook formatting (Alertatron/
// 3Commas/Discord/TradingConnector JSON builders) are pure display/integration and aren't ported.
//
// Pattern logic: track alternating swing pivots (via a rolling high/low pivot log, PivotLength
// bars each side). A double top forms when pivot[n-4] < pivot[n-2] (rising into the pattern),
// pivot[n-2] and pivot[n] (the two "tops") are within Tolerance% of each other in height, and
// price closes back below the pattern's neckline (pivot[n-1], the low between the two tops) —
// double bottom is the mirror image. On confirmation, entry target is Fib% of the pattern height
// beyond the neckline, and the invalidation stop sits at the high/low point of the pattern (the
// StopFib offset variant from the source is not ported — the plain 0% case, i.e. stop at the
// pattern's own extreme, is what's implemented). ATR-trailing-stop mode from the source isn't
// ported — same reasoning as the other trailing-stop strategies here: that's per-trade lifecycle
// state for the execution layer, not a static SLPct/TPPct.
type DoubleTopBottom struct {
	PivotLength int
	Tolerance   decimal.Decimal // max height difference between the two tops/bottoms, as % of pattern height
	FibTarget   decimal.Decimal // target extension beyond the neckline, as % of pattern height

	// pivot log: up to 5 most recent confirmed alternating swing points, oldest first.
	pivots      []decimal.Decimal
	pivotIsHigh []bool
	dir         int // +1 tracking toward a high, -1 tracking toward a low, 0 = unset
}

func NewDoubleTopBottom() *DoubleTopBottom {
	return &DoubleTopBottom{
		PivotLength: 50,
		Tolerance:   decimal.NewFromInt(15),
		FibTarget:   decimal.NewFromInt(100),
	}
}

func (s *DoubleTopBottom) Name() string { return "double_top_bottom" }

func (s *DoubleTopBottom) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "pivot_length", Default: decimal.NewFromInt(int64(s.PivotLength)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "tolerance", Default: s.Tolerance, Min: decimal.NewFromFloat(1), Max: decimal.NewFromInt(50)},
		{Name: "fib_target", Default: s.FibTarget, Min: decimal.NewFromInt(10), Max: decimal.NewFromInt(300)},
	}
}

func (s *DoubleTopBottom) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["pivot_length"]; ok {
		cp.PivotLength = int(ClampParam(specByName["pivot_length"], v).IntPart())
	}
	if v, ok := values["tolerance"]; ok {
		cp.Tolerance = ClampParam(specByName["tolerance"], v)
	}
	if v, ok := values["fib_target"]; ok {
		cp.FibTarget = ClampParam(specByName["fib_target"], v)
	}
	return &cp
}

func (s *DoubleTopBottom) pushPivot(value decimal.Decimal, isHigh bool) {
	s.pivots = append(s.pivots, value)
	s.pivotIsHigh = append(s.pivotIsHigh, isHigh)
	if len(s.pivots) > 5 {
		s.pivots = s.pivots[1:]
		s.pivotIsHigh = s.pivotIsHigh[1:]
	}
}

func (s *DoubleTopBottom) Evaluate(candles []Candle) (Signal, error) {
	need := s.PivotLength*2 + 1
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	// Highest/lowest-over-PivotLength, evaluated one bar back so it's a settled pivot (mirrors
	// the source's ta.highestbars/lowestbars == 0 confirmation, simplified to a single-bar-back
	// check rather than the full rolling-window bars-since-extreme test).
	idx := len(candles) - 1
	highest, err := Highest(candles[:idx+1], s.PivotLength)
	if err != nil {
		return Signal{}, err
	}
	lowest, err := Lowest(candles[:idx+1], s.PivotLength)
	if err != nil {
		return Signal{}, err
	}

	newDir := s.dir
	switch {
	case candles[idx].High.Equal(highest):
		newDir = 1
	case candles[idx].Low.Equal(lowest):
		newDir = -1
	}

	if newDir != s.dir && newDir != 0 {
		if newDir == 1 {
			s.pushPivot(highest, true)
		} else {
			s.pushPivot(lowest, false)
		}
		s.dir = newDir
	}

	if len(s.pivots) < 5 {
		return Signal{Side: Hold}, nil
	}

	p1, p2, p3, p4 := s.pivots[0], s.pivots[1], s.pivots[2], s.pivots[3]
	isTop := s.pivotIsHigh[1] // p2/p4 (the repeated extremes) determine top vs. bottom pattern
	close := candles[idx].Close

	height := p2.Add(p4).Div(decimal.NewFromInt(2)).Sub(p3).Abs()
	if height.IsZero() {
		return Signal{Side: Hold}, nil
	}
	diff := p2.Sub(p4).Abs().Div(height).Mul(hundred)
	withinTolerance := diff.LessThanOrEqual(s.Tolerance)

	target := p3.Sub(height.Mul(s.FibTarget).Div(hundred))
	if isTop {
		risingIn := p1.LessThan(p3)
		brokeNeckline := close.LessThan(p3)
		if risingIn && withinTolerance && brokeNeckline {
			stopDist := p2.Sub(p3).Abs()
			tpDist := p3.Sub(target).Abs()
			if !close.IsZero() {
				return Signal{
					Side:       Sell,
					Confidence: decimal.NewFromFloat(0.65),
					SLPct:      stopDist.Div(close),
					TPPct:      tpDist.Div(close),
				}, nil
			}
		}
	} else {
		fallingIn := p1.GreaterThan(p3)
		brokeNeckline := close.GreaterThan(p3)
		if fallingIn && withinTolerance && brokeNeckline {
			stopDist := p3.Sub(p2).Abs()
			tpDist := target.Sub(p3).Abs()
			if !close.IsZero() {
				return Signal{
					Side:       Buy,
					Confidence: decimal.NewFromFloat(0.65),
					SLPct:      stopDist.Div(close),
					TPPct:      tpDist.Div(close),
				}, nil
			}
		}
	}
	return Signal{Side: Hold}, nil
}
