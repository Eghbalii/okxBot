package strategy

import "github.com/shopspring/decimal"

// PriceVolumeBreakout is a Go port of the widely-known TradingView "Price and Volume Breakout Buy
// Strategy" by TradeDots (raw PineScript source not extractable via automated fetch; implemented
// from the standard price+volume breakout algorithm the title describes).
//
// Standard/default parameters: a 20-bar lookback for the recent high (a common published default
// for breakout-lookback strategies), and a volume surge requirement of 1.5x the 20-bar average
// volume (a widely-used "meaningfully above average" volume confirmation threshold).
//
// Signal logic (buy-side only, per the strategy's own name — it is explicitly a long-only breakout
// system): buy when the close breaks above the highest high of the last `lookback` bars (excluding
// the current one) AND current volume is at least `VolumeMult` times the average volume over the
// same lookback — the volume confirmation is what separates a genuine breakout from a low-conviction
// poke through resistance. Stop at the breakout level itself (a structural invalidation: if price
// falls back below the level it broke, the breakout failed).
type PriceVolumeBreakout struct {
	Lookback   int
	VolumeMult decimal.Decimal
	RiskReward decimal.Decimal
}

func NewPriceVolumeBreakout() *PriceVolumeBreakout {
	return &PriceVolumeBreakout{
		Lookback:   20,
		VolumeMult: decimal.NewFromFloat(1.5),
		RiskReward: decimal.NewFromFloat(1.5),
	}
}

func (s *PriceVolumeBreakout) Name() string { return "price_volume_breakout" }

func (s *PriceVolumeBreakout) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "lookback", Default: decimal.NewFromInt(int64(s.Lookback)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(200)},
		{Name: "volume_mult", Default: s.VolumeMult, Min: decimal.NewFromFloat(1), Max: decimal.NewFromInt(10)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *PriceVolumeBreakout) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["lookback"]; ok {
		cp.Lookback = int(ClampParam(spec["lookback"], v).IntPart())
	}
	if v, ok := values["volume_mult"]; ok {
		cp.VolumeMult = ClampParam(spec["volume_mult"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

func (s *PriceVolumeBreakout) Evaluate(candles []Candle) (Signal, error) {
	if len(candles) < s.Lookback+1 {
		return Signal{Side: Hold}, nil
	}
	prior := candles[:len(candles)-1]
	priorHigh, err := Highest(prior, s.Lookback)
	if err != nil {
		return Signal{}, err
	}
	avgVol, err := AvgVolume(prior, s.Lookback)
	if err != nil {
		return Signal{}, err
	}
	last := candles[len(candles)-1]

	volumeConfirmed := avgVol.IsPositive() && last.Volume.GreaterThanOrEqual(avgVol.Mul(s.VolumeMult))
	brokeOut := last.Close.GreaterThan(priorHigh)

	if brokeOut && volumeConfirmed {
		risk := last.Close.Sub(priorHigh)
		if !risk.IsPositive() {
			return Signal{Side: Hold}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.6),
			EntryPx:    last.Close,
			SLPx:       priorHigh,
			TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	}
	return Signal{Side: Hold}, nil
}
