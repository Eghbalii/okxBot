package strategy

import "github.com/shopspring/decimal"

// BTCDivergence trades an altcoin against what the wider market is doing.
//
// WHY THIS ONE IS DIFFERENT FROM THE OTHER 36. Every existing strategy reads one instrument's own
// price and nothing else. The operator's own observation is what this implements: an altcoin can be
// cleanly trending and reverse the moment BTC's candle turns red — so a breakout on a token while
// BTC is falling is a materially different event from the same breakout while BTC rises, and no
// strategy in the registry can tell them apart.
//
// Two setups, deliberately opposite, because the honest answer to "does the market lead the token"
// is not obvious and both readings are defensible:
//
//   - FOLLOW: the token lags a BTC move. BTC breaks out, the token has not yet, and the expectation
//     is that it follows. This is the standard crypto correlation trade.
//   - FADE: the token has run against BTC and is expected to snap back into line. A token up 3%
//     while BTC is down 1% is a stretched spread, not a trend.
//
// Which is right is an empirical question this cannot settle by reasoning, so the strategy takes a
// Mode and the backtest decides — the same discipline that killed three of my own diagnoses in this
// investigation.
//
// Requires a MarketView carrying BTC's series, so it implements MultiTimeframeStrategy's
// EvaluateWith path rather than plain Evaluate: with only its own candles it has nothing to compare
// against and correctly says nothing.
type BTCDivergence struct {
	// Mode is "follow" or "fade".
	Mode string
	// Window is how many bars the comparison spans.
	Window int
	// MinDivergence is how far apart the two must have moved, as a fraction. Below this the two are
	// simply moving together and there is no signal.
	MinDivergence decimal.Decimal
	// MinBTCMove requires BTC itself to have done something — a flat market leads nothing, and a
	// divergence measured against noise is noise.
	MinBTCMove decimal.Decimal

	SLPct decimal.Decimal
	TPPct decimal.Decimal
}

func NewBTCDivergence() *BTCDivergence {
	return &BTCDivergence{
		Mode:          "follow",
		Window:        12,
		MinDivergence: decimal.NewFromFloat(0.008),
		MinBTCMove:    decimal.NewFromFloat(0.003),
		SLPct:         decimal.NewFromFloat(0.006),
		TPPct:         decimal.NewFromFloat(0.015),
	}
}

func (b *BTCDivergence) Name() string { return "btc_divergence" }

// Evaluate with only the token's own candles has nothing to compare against.
//
// Returning Hold rather than guessing is the point: this strategy's entire premise is the
// comparison, and a version that falls back to a single-instrument rule would be a different
// strategy wearing this one's name and results.
func (b *BTCDivergence) Evaluate(candles []Candle) (Signal, error) {
	return Signal{Side: Hold}, nil
}

// EvaluateView is the real entry point (the MultiTimeframeStrategy interface, §9).
func (b *BTCDivergence) EvaluateView(v MarketView) (Signal, error) {
	window := b.Window
	if window <= 0 {
		window = 12
	}
	own := v.Candles
	btc := v.Reference
	if len(own) < window+1 || len(btc) < window+1 {
		return Signal{Side: Hold}, nil
	}

	ownMove, ok1 := pctMove(own, window)
	btcMove, ok2 := pctMove(btc, window)
	if !ok1 || !ok2 {
		return Signal{Side: Hold}, nil
	}

	// A flat market leads nothing, and a divergence measured against noise is noise.
	if btcMove.Abs().LessThan(b.MinBTCMove) {
		return Signal{Side: Hold}, nil
	}

	spread := ownMove.Sub(btcMove)
	if spread.Abs().LessThan(b.MinDivergence) {
		return Signal{Side: Hold}, nil
	}

	side := Hold
	switch b.Mode {
	case "fade":
		// The token has run ahead of the market; expect it to come back into line.
		if spread.IsPositive() {
			side = Sell
		} else {
			side = Buy
		}
	default: // "follow"
		// The token lags a real BTC move; expect it to catch up. Only when the token has moved LESS
		// than BTC in BTC's own direction — a token already ahead has nothing to catch up to.
		if btcMove.IsPositive() && spread.IsNegative() {
			side = Buy
		} else if btcMove.IsNegative() && spread.IsPositive() {
			side = Sell
		}
	}
	if side == Hold {
		return Signal{Side: Hold}, nil
	}

	// Confidence scales with how stretched the spread is, capped so an extreme dislocation — which
	// is as likely to be a data glitch as an opportunity — cannot dominate.
	c := spread.Abs().Div(b.MinDivergence)
	if c.GreaterThan(decimal.NewFromInt(3)) {
		c = decimal.NewFromInt(3)
	}
	return Signal{
		Side:       side,
		Confidence: c.Div(decimal.NewFromInt(3)),
		SLPct:      b.SLPct,
		TPPct:      b.TPPct,
	}, nil
}

// pctMove is the close-to-close move over the last `window` bars, as a fraction.
func pctMove(candles []Candle, window int) (decimal.Decimal, bool) {
	if len(candles) < window+1 {
		return decimal.Zero, false
	}
	from := candles[len(candles)-window-1].Close
	to := candles[len(candles)-1].Close
	if !from.IsPositive() {
		return decimal.Zero, false
	}
	return to.Sub(from).Div(from), true
}

func (b *BTCDivergence) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "window", Min: decimal.NewFromInt(3), Max: decimal.NewFromInt(60), Default: decimal.NewFromInt(12)},
		{Name: "min_divergence", Min: decimal.NewFromFloat(0.002), Max: decimal.NewFromFloat(0.05), Default: decimal.NewFromFloat(0.008)},
		{Name: "min_btc_move", Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.03), Default: decimal.NewFromFloat(0.003)},
		{Name: "sl_pct", Min: decimal.NewFromFloat(0.002), Max: decimal.NewFromFloat(0.03), Default: decimal.NewFromFloat(0.006)},
		{Name: "tp_pct", Min: decimal.NewFromFloat(0.002), Max: decimal.NewFromFloat(0.08), Default: decimal.NewFromFloat(0.015)},
	}
}

func (b *BTCDivergence) WithParams(p map[string]decimal.Decimal) Strategy {
	out := NewBTCDivergence()
	out.Mode = b.Mode
	if v, ok := p["window"]; ok {
		out.Window = int(v.IntPart())
	}
	if v, ok := p["min_divergence"]; ok {
		out.MinDivergence = v
	}
	if v, ok := p["min_btc_move"]; ok {
		out.MinBTCMove = v
	}
	if v, ok := p["sl_pct"]; ok {
		out.SLPct = v
	}
	if v, ok := p["tp_pct"]; ok {
		out.TPPct = v
	}
	return out
}
