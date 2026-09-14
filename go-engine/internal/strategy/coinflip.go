package strategy

import "github.com/shopspring/decimal"

// CoinFlip is a null baseline: it fires on a fixed bar cadence with no reference to price at all.
//
// WHY A NULL STRATEGY IS SHIPPED CODE. Screening 36 strategies over 65,966 trades found 35 losing
// money, and raising the reward:risk floor moved the win rate almost exactly along the 1/(1+R)
// breakeven line — the signature of a system whose outcomes are distributed as though price were
// random. But that was an inference from an arithmetic identity, and acting on it means abandoning
// parameter tuning for the whole roster.
//
// A strategy that CANNOT know anything settles it. Run through the identical path — same sizing,
// same clamps, same fees, same SL-wins-a-tie rule — it measures what the entry logic is worth by
// measuring what no entry logic is worth. Any real strategy that cannot beat it is not contributing
// information, however plausible its rules look.
//
// Deliberately registered in Factories so it runs as an ordinary kind rather than through a
// test-only path: a separate code path for the baseline would measure the path, not the strategies.
// It must never be assigned to live trading — it has no opinion to be right about.
type CoinFlip struct {
	// Cadence is how many bars pass between signals. Not a tuning parameter in any real sense:
	// it only controls sample size.
	Cadence int
	// SLPct/TPPct are fixed percentage levels, since a strategy with no view of the market has no
	// structure to derive a level from.
	SLPct decimal.Decimal
	TPPct decimal.Decimal

	n int
}

func NewCoinFlip() *CoinFlip {
	return &CoinFlip{
		Cadence: 12,
		SLPct:   decimal.NewFromFloat(0.006),
		TPPct:   decimal.NewFromFloat(0.012),
	}
}

func (c *CoinFlip) Name() string { return "coin_flip" }

func (c *CoinFlip) Evaluate(candles []Candle) (Signal, error) {
	cadence := c.Cadence
	if cadence <= 0 {
		cadence = 12
	}
	c.n++
	if c.n%cadence != 0 {
		return Signal{Side: Hold}, nil
	}
	// Alternating rather than pseudo-random: a deterministic baseline is reproducible, and
	// alternating sides cancels any directional drift in the sample, which a fixed side would not.
	side := Buy
	if (c.n/cadence)%2 == 0 {
		side = Sell
	}
	return Signal{Side: side, Confidence: decimal.NewFromFloat(0.5), SLPct: c.SLPct, TPPct: c.TPPct}, nil
}

func (c *CoinFlip) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "cadence", Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(100), Default: decimal.NewFromInt(12)},
		{Name: "sl_pct", Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.05), Default: decimal.NewFromFloat(0.006)},
		{Name: "tp_pct", Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.10), Default: decimal.NewFromFloat(0.012)},
	}
}

func (c *CoinFlip) WithParams(p map[string]decimal.Decimal) Strategy {
	out := NewCoinFlip()
	if v, ok := p["cadence"]; ok {
		out.Cadence = int(v.IntPart())
	}
	if v, ok := p["sl_pct"]; ok {
		out.SLPct = v
	}
	if v, ok := p["tp_pct"]; ok {
		out.TPPct = v
	}
	// n is deliberately NOT copied: WithParams must not carry accumulated state into a
	// differently-configured variant (§16.8's state-leak audit).
	return out
}
