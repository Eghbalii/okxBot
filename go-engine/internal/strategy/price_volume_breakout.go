package strategy

import "github.com/shopspring/decimal"

// PriceVolumeBreakout is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/price_volume_breakout.pine, @version=5 "Price and Volume Breakout
// Buy Strategy [TradeDots]" by tradedots), replacing the first pass's from-description
// implementation (2026-09-16), which used a 20-bar lookback, a 1.5x volume multiple, and an
// ATR/structural stop — all invented; the real source's actual construction is different in every
// one of those respects.
//
// Real parameters, read directly from the source:
//   - Price breakout period 60, volume breakout period 60 (both `input_price_breakout_period`/
//     `input_volume_breakout_period`), not 20.
//   - Trendline (SMA) length 200 (`input_trendline_legnth`).
//   - Order direction defaults to "Long" — the source is long-only by default (short is an option,
//     off by default); this port implements the default long-only behavior, mirroring
//     ichimoku_tk_cross.go's own precedent for a source whose real default excludes one side.
//   - No volume MULTIPLE at all: the source's volume confirmation is simply `volume >
//     volume_highest[1]` — current volume exceeding the highest volume of the trailing
//     `input_volume_breakout_period` bars (excluding the current one), not a multiple of the
//     average.
//
// Signal logic, exactly the source's own long entry: `close > price_highest[1] AND volume >
// volume_highest[1] AND close > sma(close, 200)` — a genuine new high on the highest volume seen in
// the lookback window, confirmed by the 200-length trend filter. Exit (the source's own
// `strategy.close`, modeled here as the opposite Signal per this package's convention): 5
// CONSECUTIVE closes below the 200-SMA. The source has no ATR/structural stop of its own; SL/TP are
// a documented addition here (CLAUDE.md §16.9's "no stop-loss" incident), using the breakout level
// itself as the natural invalidation.
type PriceVolumeBreakout struct {
	PriceLookback  int
	VolumeLookback int
	TrendLen       int
	RiskReward     decimal.Decimal
}

func NewPriceVolumeBreakout() *PriceVolumeBreakout {
	return &PriceVolumeBreakout{
		PriceLookback:  60,
		VolumeLookback: 60,
		TrendLen:       200,
		RiskReward:     decimal.NewFromFloat(1.5),
	}
}

func (s *PriceVolumeBreakout) Name() string { return "price_volume_breakout" }

func (s *PriceVolumeBreakout) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "price_lookback", Default: decimal.NewFromInt(int64(s.PriceLookback)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "volume_lookback", Default: decimal.NewFromInt(int64(s.VolumeLookback)), Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(300)},
		{Name: "trend_len", Default: decimal.NewFromInt(int64(s.TrendLen)), Min: decimal.NewFromInt(10), Max: decimal.NewFromInt(400)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *PriceVolumeBreakout) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["price_lookback"]; ok {
		cp.PriceLookback = int(ClampParam(spec["price_lookback"], v).IntPart())
	}
	if v, ok := values["volume_lookback"]; ok {
		cp.VolumeLookback = int(ClampParam(spec["volume_lookback"], v).IntPart())
	}
	if v, ok := values["trend_len"]; ok {
		cp.TrendLen = int(ClampParam(spec["trend_len"], v).IntPart())
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

func highestVolume(candles []Candle, period int) (decimal.Decimal, error) {
	if len(candles) < period {
		return decimal.Zero, errNeedMore(period, len(candles))
	}
	window := candles[len(candles)-period:]
	highest := window[0].Volume
	for _, c := range window[1:] {
		if c.Volume.GreaterThan(highest) {
			highest = c.Volume
		}
	}
	return highest, nil
}

func (s *PriceVolumeBreakout) Evaluate(candles []Candle) (Signal, error) {
	need := maxInt(s.PriceLookback, s.VolumeLookback, s.TrendLen) + 5
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}
	prior := candles[:len(candles)-1]
	priorHigh, err := Highest(prior, s.PriceLookback)
	if err != nil {
		return Signal{}, err
	}
	priorVolHigh, err := highestVolume(prior, s.VolumeLookback)
	if err != nil {
		return Signal{}, err
	}
	trendSMA, err := SMA(candles, s.TrendLen)
	if err != nil {
		return Signal{}, err
	}
	last := candles[len(candles)-1]

	brokeOut := last.Close.GreaterThan(priorHigh)
	volumeConfirmed := last.Volume.GreaterThan(priorVolHigh)
	aboveTrend := last.Close.GreaterThan(trendSMA)

	if brokeOut && volumeConfirmed && aboveTrend {
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

	// Exit: 5 consecutive closes below the trend SMA — the source's own `strategy.close` condition,
	// modeled as an opposite-side Signal per this package's "close = opposite signal" convention
	// (CLAUDE.md §27.3). Only meaningful while a long is actually open; PaperTrader/RealTrader route
	// an opposite-side signal on a flat token as a fresh (and here, long-only-by-design) decision,
	// so this deliberately checks a genuinely bearish run rather than firing constantly.
	if len(candles) >= s.TrendLen+5 {
		belowStreak := true
		for i := 0; i < 5; i++ {
			idx := len(candles) - 1 - i
			sma, err := SMA(candles[:idx+1], s.TrendLen)
			if err != nil {
				belowStreak = false
				break
			}
			if !candles[idx].Close.LessThan(sma) {
				belowStreak = false
				break
			}
		}
		if belowStreak {
			// Every signal in this package needs a stop (CLAUDE.md §16.9's "no stop-loss" incident),
			// including a close-the-open-long signal — a small percentage-based SL/TP is used here
			// since this exit has no structural level of its own the way the entry's breakout level
			// does.
			return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.4), SLPct: decimal.NewFromFloat(0.02), TPPct: decimal.NewFromFloat(0.02)}, nil
		}
	}

	return Signal{Side: Hold}, nil
}
