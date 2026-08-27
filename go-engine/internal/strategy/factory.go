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
	"weekly_dip_buy":       func() Strategy { return NewWeeklyDipBuy() },
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
