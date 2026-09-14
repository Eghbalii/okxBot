package strategy

import (
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
)

// Factory constructs a fresh built-in Strategy value for a "kind" string (the same value stored
// in port.StrategyConfig.Kind), used to resolve durable DB rows (origin + sub-strategies) back
// into live Strategy values at process startup (CLAUDE.md §11.3) — a restart must reload exactly
// which variant was assigned to which token/timeframe from Postgres, not from hardcoded Go.
type Factory func() Strategy

// Factories is the built-in kind -> constructor registry. Add an entry here whenever a new
// built-in strategy should be creatable/assignable from the panel.
var Factories = map[string]Factory{
	"rsi_sma":              func() Strategy { return NewRSISMA(14, 50) },
	"rsi_sma_fuzzy":        func() Strategy { return NewRSISMAFuzzy() },
	"double_top_bottom":    func() Strategy { return NewDoubleTopBottom() },
	"dual_ma_atr":          func() Strategy { return NewDualMAATR() },
	"ema_cross_trailing":   func() Strategy { return NewEMACrossTrailing() },
	"grid_like":            func() Strategy { return NewGridLike(decimal.NewFromFloat(0.01)) },
	"pivot_reversal":       func() Strategy { return NewPivotReversal() },
	"pmax":                 func() Strategy { return NewPMax() },
	"seasonal_atr_short":   func() Strategy { return NewSeasonalATRShort() },
	"sma_cross_fixed_exit": func() Strategy { return NewSMACrossFixedExit() },
	"stepped_trailing":     func() Strategy { return NewSteppedTrailing() },
	"stoch_cross":          func() Strategy { return NewStochCross() },
	"trend_confluence":     func() Strategy { return NewTrendConfluence() },
	// The null baseline (see coinflip.go). Registered so it runs through the identical path as
	// every real kind — it measures what an entry signal is worth by measuring what no signal is
	// worth. Never assign it to live trading.
	"coin_flip":      func() Strategy { return NewCoinFlip() },
	"weekly_dip_buy": func() Strategy { return NewWeeklyDipBuy() },

	// The four kinds added 2026-09-14 to fill gaps the whole existing roster shares: none of the 36
	// asks whether the market is a place its pattern can work, none combines opinions, none looks
	// outside its own instrument, and none knows what time it is (docs/RL_V8_PLAN.md).
	"confluence":     func() Strategy { return NewConfluence() },
	"btc_divergence": func() Strategy { return NewBTCDivergence() },
	"btc_divergence_fade": func() Strategy {
		d := NewBTCDivergence()
		d.Mode = "fade"
		// Fade is a mean-reversion premise, so it wants a RANGING market — the mirror of follow's
		// requirement. Trading it in a trend is what a spread that keeps stretching looks like.
		d.MinEfficiency = decimal.Zero
		d.MaxEfficiency = decimal.NewFromFloat(0.35)
		return d
	},
	"session_momentum": func() Strategy { return NewSessionMomentum() },

	// Ported from TradingView at the operator's request (2026-09-14), chosen from seven candidates
	// for carrying ideas the roster lacks rather than another entry pattern: a regime gate that
	// declines to trade, filters on an existing pattern, and slope ACCELERATION rather than order.
	"trendshift":      func() Strategy { return NewTrendShift() },
	"sweep_reverse":   func() Strategy { return NewSweepReverse() },
	"gradient_ribbon": func() Strategy { return NewGradientRibbon() },

	// Scalp/price-action/ICT additions (CLAUDE.md §9, added 2026-09) — aimed at low timeframes
	// (5m) per an explicit operator request for well-known scalping-style strategies, since the
	// original 14 skew toward slower swing-style setups.
	"vwap_reversion":      func() Strategy { return NewVWAPReversion() },
	"bb_squeeze_breakout": func() Strategy { return NewBBSqueezeBreakout() },
	"range_breakout":      func() Strategy { return NewRangeBreakout() },
	"keltner_trend_scalp": func() Strategy { return NewKeltnerTrendScalp() },
	"ict_fvg":             func() Strategy { return NewICTFairValueGap() },
	"ict_order_block":     func() Strategy { return NewICTOrderBlock() },
	"ict_liquidity_sweep": func() Strategy { return NewICTLiquiditySweep() },
	"engulfing_reversal":  func() Strategy { return NewEngulfingReversal() },
	"inside_bar_breakout": func() Strategy { return NewInsideBarBreakout() },
	"macd_momentum":       func() Strategy { return NewMACDMomentum() },
	"volume_breakout":     func() Strategy { return NewVolumeBreakout() },
	"ema_ribbon_pullback": func() Strategy { return NewEMARibbonPullback() },

	// V2 revisions of the twelve above (2026-09-12). Registered as SEPARATE kinds rather than as
	// edits to their parents, per CLAUDE.md §11.3's locked-origin rule: the V1 rows keep their
	// accumulated trade history and stay independently comparable, which is the whole point of
	// shipping a revision rather than a replacement.
	//
	// What changed, uniformly (CLAUDE.md §45): levels are sized in ATR units instead of fixed
	// percentages or raw structural distances, and the reward:risk ratio is bounded at the source
	// so a target stays somewhere price can actually reach. Most also gained a regime filter —
	// trend, volatility or conviction — since the V1 data showed the losses came from taking every
	// occurrence of a pattern rather than from the pattern itself.
	"vwap_reversion_v2":      func() Strategy { return NewVWAPReversionV2() },
	"bb_squeeze_breakout_v2": func() Strategy { return NewBBSqueezeBreakoutV2() },
	"range_breakout_v2":      func() Strategy { return NewRangeBreakoutV2() },
	"keltner_trend_scalp_v2": func() Strategy { return NewKeltnerTrendScalpV2() },
	"ict_fvg_v2":             func() Strategy { return NewICTFairValueGapV2() },
	"ict_order_block_v2":     func() Strategy { return NewICTOrderBlockV2() },
	"ict_liquidity_sweep_v2": func() Strategy { return NewICTLiquiditySweepV2() },
	"engulfing_reversal_v2":  func() Strategy { return NewEngulfingReversalV2() },
	"inside_bar_breakout_v2": func() Strategy { return NewInsideBarBreakoutV2() },
	"macd_momentum_v2":       func() Strategy { return NewMACDMomentumV2() },
	"volume_breakout_v2":     func() Strategy { return NewVolumeBreakoutV2() },
	"ema_ribbon_pullback_v2": func() Strategy { return NewEMARibbonPullbackV2() },
}

// FromConfig builds a live Strategy for kind, applying config as WithParams overrides (config is
// a JSON object of ParamSpec-named values, e.g. {"rsi_period": 21, "sl_pct": 0.015}) — this is
// what turns a persisted sub-strategy row's Config back into a runnable strategy.Strategy.
func FromConfig(kind string, config json.RawMessage) (Strategy, error) {
	factory, ok := Factories[kind]
	if !ok {
		return nil, fmt.Errorf("unknown strategy kind %q", kind)
	}
	s := factory()
	if len(config) == 0 || string(config) == "{}" {
		return s, nil
	}
	var raw map[string]float64
	if err := json.Unmarshal(config, &raw); err != nil {
		return nil, fmt.Errorf("parse config for kind %q: %w", kind, err)
	}
	values := make(map[string]decimal.Decimal, len(raw))
	for k, v := range raw {
		values[k] = decimal.NewFromFloat(v)
	}
	return s.WithParams(values), nil
}
