package strategy

import "github.com/shopspring/decimal"

// SeasonalATRShort is a Go port of "TASC 2026.08 An Ag Selling Model" by Perry J Kaufman,
// provided by PineCoders (pinescript/strategy_TASC 2026.08 An Ag Selling Model.pine, TradingView
// Pine Script v6 — no separate license beyond standard TASC/PineCoders attribution). Designed for
// commodity futures with a harvest season: trades are only allowed starting a configurable number
// of months after harvest, and only while price trades above an SMA+ATR "sell level" band; all
// positions close once the active window ends (next harvest). The original enforces a minimum
// number of days between entries (DaysBetween) — that's pyramiding/re-entry pacing tracked across
// calls, which Evaluate's stateful receiver models directly (daysSinceLastSale), same pattern as
// GridLike's baseline state.
type SeasonalATRShort struct {
	MALength      int
	ATRLength     int
	ATRMultiplier decimal.Decimal
	HarvestMonth  int // 1-12
	DelayMonths   int // months after harvest before selling starts
	DaysBetween   int // minimum candles between entries while active
	SLPct         decimal.Decimal
	TPPct         decimal.Decimal

	daysSinceLastSale int
	hasOpenTrade      bool
	active            bool
}

func NewSeasonalATRShort() *SeasonalATRShort {
	return &SeasonalATRShort{
		MALength:      40,
		ATRLength:     20,
		ATRMultiplier: decimal.NewFromFloat(2.5),
		HarvestMonth:  11,
		DelayMonths:   2,
		DaysBetween:   30,
		SLPct:         decimal.NewFromFloat(0.05),
		TPPct:         decimal.NewFromFloat(0.1),
	}
}

func (s *SeasonalATRShort) Name() string { return "seasonal_atr_short" }

func (s *SeasonalATRShort) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "ma_length", Default: decimal.NewFromInt(int64(s.MALength)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "atr_length", Default: decimal.NewFromInt(int64(s.ATRLength)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "atr_multiplier", Default: s.ATRMultiplier, Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromInt(10)},
		{Name: "days_between", Default: decimal.NewFromInt(int64(s.DaysBetween)), Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(365)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.3)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *SeasonalATRShort) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["ma_length"]; ok {
		cp.MALength = int(ClampParam(specByName["ma_length"], v).IntPart())
	}
	if v, ok := values["atr_length"]; ok {
		cp.ATRLength = int(ClampParam(specByName["atr_length"], v).IntPart())
	}
	if v, ok := values["atr_multiplier"]; ok {
		cp.ATRMultiplier = ClampParam(specByName["atr_multiplier"], v)
	}
	if v, ok := values["days_between"]; ok {
		cp.DaysBetween = int(ClampParam(specByName["days_between"], v).IntPart())
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	return &cp
}

func (s *SeasonalATRShort) Evaluate(candles []Candle) (Signal, error) {
	need := s.MALength
	if s.ATRLength+1 > need {
		need = s.ATRLength + 1
	}
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	last := candles[len(candles)-1]
	month := int(last.Timestamp.Month())
	prevMonth := month
	if len(candles) >= 2 {
		prevMonth = int(candles[len(candles)-2].Timestamp.Month())
	}

	beginSaleMonth := s.HarvestMonth + s.DelayMonths + 1
	beginSaleMonth = ((beginSaleMonth - 1) % 12) + 1

	// A new crop year starts on the harvest month; active resets false until beginSaleMonth,
	// then stays true (across months) until the next harvest — matching the source's
	// `active`/`newCropYr` state, not a single-month window.
	if month == s.HarvestMonth && prevMonth != s.HarvestMonth {
		s.active = false
		s.hasOpenTrade = false
	}
	if month == beginSaleMonth && prevMonth != beginSaleMonth {
		s.active = true
	}
	if !s.active {
		return Signal{Side: Hold}, nil
	}

	sma, err := SMA(candles, s.MALength)
	if err != nil {
		return Signal{}, err
	}
	atr, err := ATR(candles, s.ATRLength)
	if err != nil {
		return Signal{}, err
	}
	sellLevel := sma.Add(atr.Mul(s.ATRMultiplier))

	s.daysSinceLastSale++
	if last.High.LessThan(sellLevel) {
		return Signal{Side: Hold}, nil
	}
	if s.hasOpenTrade && s.daysSinceLastSale < s.DaysBetween {
		return Signal{Side: Hold}, nil
	}

	s.hasOpenTrade = true
	s.daysSinceLastSale = 0
	return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.5), SLPct: s.SLPct, TPPct: s.TPPct}, nil
}
