package strategy

import "github.com/shopspring/decimal"

// VWAPReversion is a well-known intraday scalp: fade a price extension away from a rolling VWAP
// back toward it. Deviation is measured in ATR units (not a fixed percentage) so the entry
// threshold scales with the instrument's own recent volatility. Designed for low timeframes (5m)
// where price frequently overextends from VWAP on short bursts of order flow and mean-reverts
// within a few candles.
type VWAPReversion struct {
	VWAPPeriod   int
	ATRPeriod    int
	DeviationATR decimal.Decimal // how many ATRs away from VWAP triggers a fade
	SLPct        decimal.Decimal
	TPPct        decimal.Decimal
}

func NewVWAPReversion() *VWAPReversion {
	return &VWAPReversion{
		VWAPPeriod:   20,
		ATRPeriod:    14,
		DeviationATR: decimal.NewFromFloat(1.5),
		SLPct:        decimal.NewFromFloat(0.006),
		TPPct:        decimal.NewFromFloat(0.008),
	}
}

func (s *VWAPReversion) Name() string { return "vwap_reversion" }

func (s *VWAPReversion) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "vwap_period", Default: decimal.NewFromInt(int64(s.VWAPPeriod)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(200)},
		{Name: "atr_period", Default: decimal.NewFromInt(int64(s.ATRPeriod)), Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100)},
		{Name: "deviation_atr", Default: s.DeviationATR, Min: decimal.NewFromFloat(0.3), Max: decimal.NewFromInt(6)},
		{Name: "sl_pct", Default: s.SLPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.2)},
		{Name: "tp_pct", Default: s.TPPct, Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.5)},
	}
}

func (s *VWAPReversion) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	specByName := paramsByName(s.Params())
	if v, ok := values["vwap_period"]; ok {
		cp.VWAPPeriod = int(ClampParam(specByName["vwap_period"], v).IntPart())
	}
	if v, ok := values["atr_period"]; ok {
		cp.ATRPeriod = int(ClampParam(specByName["atr_period"], v).IntPart())
	}
	if v, ok := values["deviation_atr"]; ok {
		cp.DeviationATR = ClampParam(specByName["deviation_atr"], v)
	}
	if v, ok := values["sl_pct"]; ok {
		cp.SLPct = ClampParam(specByName["sl_pct"], v)
	}
	if v, ok := values["tp_pct"]; ok {
		cp.TPPct = ClampParam(specByName["tp_pct"], v)
	}
	return &cp
}

func (s *VWAPReversion) Evaluate(candles []Candle) (Signal, error) {
	need := s.VWAPPeriod
	if s.ATRPeriod+1 > need {
		need = s.ATRPeriod + 1
	}
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	vwap, err := SessionVWAP(candles, s.VWAPPeriod)
	if err != nil {
		return Signal{}, err
	}
	atr, err := ATR(candles, s.ATRPeriod)
	if err != nil {
		return Signal{}, err
	}
	if !atr.IsPositive() {
		return Signal{Side: Hold}, nil
	}
	price := candles[len(candles)-1].Close
	deviation := price.Sub(vwap).Div(atr) // in ATR units, signed

	switch {
	case deviation.LessThan(s.DeviationATR.Neg()):
		// price extended below VWAP by more than the threshold — fade back up.
		confidence := deviation.Neg().Div(s.DeviationATR)
		if confidence.GreaterThan(decimal.NewFromInt(1)) {
			confidence = decimal.NewFromInt(1)
		}
		return Signal{Side: Buy, Confidence: confidence, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	case deviation.GreaterThan(s.DeviationATR):
		confidence := deviation.Div(s.DeviationATR)
		if confidence.GreaterThan(decimal.NewFromInt(1)) {
			confidence = decimal.NewFromInt(1)
		}
		return Signal{Side: Sell, Confidence: confidence, SLPct: s.SLPct, TPPct: s.TPPct}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
