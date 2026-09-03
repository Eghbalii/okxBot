package strategy

import "github.com/shopspring/decimal"

// BBSqueezeBreakout is the classic "Bollinger squeeze" scalp: when Bollinger Band width
// contracts to a multi-bar low (volatility compression, i.e. the market is coiling), trade the
// breakout once price closes outside the bands again. Very standard on low timeframes for
// catching the start of a move right as it begins, rather than chasing it mid-run.
type BBSqueezeBreakout struct {
	BBPeriod     int
	BBMult       decimal.Decimal
	SqueezeLen   int             // how many bars back to compare band-width against
	SqueezeRatio decimal.Decimal // current width must be <= this fraction of the SqueezeLen-back width
	SLPct        decimal.Decimal
	TPPct        decimal.Decimal
}

func NewBBSqueezeBreakout() *BBSqueezeBreakout {
	return &BBSqueezeBreakout{
		BBPeriod:     20,
		BBMult:       decimal.NewFromFloat(2),
		SqueezeLen:   20,
		SqueezeRatio: decimal.NewFromFloat(0.7),
		SLPct:        decimal.NewFromFloat(0.006),
		TPPct:        decimal.NewFromFloat(0.012),
	}
}

func (s *BBSqueezeBreakout) Name() string { return "bb_squeeze_breakout" }

func (s *BBSqueezeBreakout) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "bb_period", Default: decimal.NewFromInt(int64(s.BBPeriod)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(200)},
		{Name: "bb_mult", Default: s.BBMult, Min: decimal.NewFromFloat(0.5), Max: decimal.NewFromInt(5)},
		{Name: "squeeze_len", Default: decimal.NewFromInt(int64(s.SqueezeLen)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(300)},
		{Name: "squeeze_ratio", Default: s.SqueezeRatio, Min: decimal.NewFromFloat(0.1), Max: decimal.NewFromFloat(0.99)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *BBSqueezeBreakout) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["bb_period"]; ok {
		cp.BBPeriod = int(ClampParam(specByName["bb_period"], v).IntPart())
	}
	if v, ok := values["bb_mult"]; ok {
		cp.BBMult = ClampParam(specByName["bb_mult"], v)
	}
	if v, ok := values["squeeze_len"]; ok {
		cp.SqueezeLen = int(ClampParam(specByName["squeeze_len"], v).IntPart())
	}
	if v, ok := values["squeeze_ratio"]; ok {
		cp.SqueezeRatio = ClampParam(specByName["squeeze_ratio"], v)
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	return &cp
}

func (s *BBSqueezeBreakout) bandWidth(candles []Candle) (decimal.Decimal, error) {
	basis, upper, lower, err := BollingerBands(candles, s.BBPeriod, s.BBMult)
	if err != nil {
		return decimal.Zero, err
	}
	if !basis.IsPositive() {
		return decimal.Zero, nil
	}
	return upper.Sub(lower).Div(basis), nil
}

func (s *BBSqueezeBreakout) Evaluate(candles []Candle) (Signal, error) {
	need := s.BBPeriod + s.SqueezeLen
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	widthNow, err := s.bandWidth(candles)
	if err != nil {
		return Signal{}, err
	}
	widthPast, err := s.bandWidth(candles[:len(candles)-s.SqueezeLen])
	if err != nil {
		return Signal{}, err
	}
	if !widthPast.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	wasSqueezed := widthNow.LessThanOrEqual(widthPast.Mul(s.SqueezeRatio))
	if !wasSqueezed {
		return Signal{Side: Hold}, nil
	}

	_, upper, lower, err := BollingerBands(candles, s.BBPeriod, s.BBMult)
	if err != nil {
		return Signal{}, err
	}
	last := candles[len(candles)-1]

	switch {
	case last.Close.GreaterThan(upper):
		return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case last.Close.LessThan(lower):
		return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.6), SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
