package strategy

import "github.com/shopspring/decimal"

// Confluence requires several independent strategies to agree before it emits a signal.
//
// WHY THIS IS THE MOST PROMISING OF THE FOUR. Screening 37 strategies over 68,113 trades found no
// individual gap from a coin flip that survived its own sample size — the edges, if any, are far
// smaller than the cost of trading. Confluence is the one mechanism that can make a small edge
// large: if three strategies are even weakly and INDEPENDENTLY informative, the probability that
// all three are right together is higher than any of them alone, while the fee is paid once.
//
// The honest caveat, and the reason this needs measuring rather than assuming: the strategies are
// NOT independent. Most read the same price series through similar lenses, so three agreeing may
// just be one signal counted three times — which would multiply the fee without multiplying the
// edge. §9 proposed this as a "signal aggregator" and it was never built; this is the first
// version, and the backtest against coin_flip is what decides whether it is worth keeping.
//
// Deliberately NOT a learned weighting. §15.10 dropped the model's strategy_weights output for a
// reason that applies here too: a count of agreeing strategies is measurable and explainable, while
// a learned weight over a roster that changes is a ceiling and a retrain.
type Confluence struct {
	Members []Strategy
	// MinAgree is how many members must point the same way. Below 2 this is just the first member.
	MinAgree int
	// RequireUnanimous suppresses the signal when ANY member disagrees, rather than merely counting
	// agreement — a member pointing the other way is evidence, not absence of evidence.
	RequireUnanimous bool

	// AgreeWindow is how many bars a member's opinion stays live for the purpose of counting
	// agreement.
	//
	// MEASURED, not chosen: requiring SAME-BAR agreement produced ZERO signals in 470 bars across
	// four members that fired 294 times between them. Strategies react to the same setup at
	// different moments — one on the breakout candle, another on the retest two bars later — so
	// same-bar agreement asks for a coincidence that a candle boundary makes rare by construction.
	// Measured agreement by window on the test fixture: 1 bar -> 0, 3 -> 1, 5 -> 37, 10 -> 203.
	//
	// This is the difference between "three strategies agree" and "three strategies fired on the
	// same tick", and only the first is the idea worth testing. A wider window is not free though —
	// past some point every member is always live and the count stops meaning agreement — which is
	// why it is a tunable parameter the backtest decides rather than a constant.
	AgreeWindow int

	// recent remembers when each member last spoke and which way, indexed to match Members.
	recent []memberOpinion

	SLPct decimal.Decimal
	TPPct decimal.Decimal
}

func (c *Confluence) Name() string { return "confluence" }

func (c *Confluence) Evaluate(candles []Candle) (Signal, error) {
	minAgree := c.MinAgree
	if minAgree < 2 {
		minAgree = 2
	}
	window := c.AgreeWindow
	if window <= 0 {
		window = 5
	}
	if len(c.recent) != len(c.Members) {
		c.recent = make([]memberOpinion, len(c.Members))
	}
	bar := len(candles)

	for i, m := range c.Members {
		sig, err := EvaluateWith(m, MarketView{Candles: candles})
		if err != nil {
			// One member failing must not silence the rest — the same best-effort rule
			// buildObservation applies to a failing strategy.
			continue
		}
		if sig.Side != Hold {
			c.recent[i] = memberOpinion{side: sig.Side, bar: bar}
		}
	}

	// Count opinions still inside the window rather than only this bar's.
	buys, sells := 0, 0
	for _, o := range c.recent {
		if o.side == Hold || bar-o.bar >= window {
			continue
		}
		if o.side == Buy {
			buys++
		} else {
			sells++
		}
	}

	side := Hold
	agree := 0
	switch {
	case buys >= minAgree && sells == 0:
		side, agree = Buy, buys
	case sells >= minAgree && buys == 0:
		side, agree = Sell, sells
	case !c.RequireUnanimous && buys >= minAgree && buys > sells:
		side, agree = Buy, buys
	case !c.RequireUnanimous && sells >= minAgree && sells > buys:
		side, agree = Sell, sells
	}
	if side == Hold {
		return Signal{Side: Hold}, nil
	}

	// Confidence scales with how many agreed, so a 5-of-5 signal is distinguishable from a bare
	// 2-of-5. The model reads this, and it is the one place a count is more informative than a flag.
	c2 := decimal.NewFromInt(int64(agree)).Div(decimal.NewFromInt(int64(maxIntOf(len(c.Members), 1))))

	// Consume the opinions that produced this signal, so one cluster of agreement fires ONCE rather
	// than on every bar until the window slides past it — the re-arming bug §30.1 found in ict_fvg,
	// where a single setup produced ten entries.
	for i := range c.recent {
		c.recent[i] = memberOpinion{}
	}
	return Signal{Side: side, Confidence: c2, SLPct: c.SLPct, TPPct: c.TPPct}, nil
}

// memberOpinion is one member's last signal and when it spoke.
type memberOpinion struct {
	side Side
	bar  int
}

func (c *Confluence) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "min_agree", Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(6), Default: decimal.NewFromInt(2)},
		{Name: "agree_window", Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(20), Default: decimal.NewFromInt(5)},
		{Name: "sl_pct", Min: decimal.NewFromFloat(0.002), Max: decimal.NewFromFloat(0.03), Default: decimal.NewFromFloat(0.006)},
		{Name: "tp_pct", Min: decimal.NewFromFloat(0.002), Max: decimal.NewFromFloat(0.08), Default: decimal.NewFromFloat(0.015)},
	}
}

func (c *Confluence) WithParams(p map[string]decimal.Decimal) Strategy {
	out := &Confluence{
		MinAgree:         c.MinAgree,
		AgreeWindow:      c.AgreeWindow,
		RequireUnanimous: c.RequireUnanimous,
		SLPct:            c.SLPct,
		TPPct:            c.TPPct,
	}
	// recent is deliberately NOT copied — carrying accumulated opinions into a differently
	// configured variant is exactly the state leak §16.8's audit found in 7 of 14 strategies.
	// Members are rebuilt from their own factories rather than copied: a warmed member carries
	// accumulated state into a differently-configured variant, which §16.8's audit found leaking in
	// 7 of 14 strategies.
	for _, m := range c.Members {
		out.Members = append(out.Members, m.WithParams(p))
	}
	if v, ok := p["min_agree"]; ok {
		out.MinAgree = int(v.IntPart())
	}
	if v, ok := p["agree_window"]; ok {
		out.AgreeWindow = int(v.IntPart())
	}
	if v, ok := p["sl_pct"]; ok {
		out.SLPct = v
	}
	if v, ok := p["tp_pct"]; ok {
		out.TPPct = v
	}
	return out
}

func maxIntOf(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// NewConfluence builds the default committee.
//
// Members are chosen to read DIFFERENT things about the market rather than to be the individually
// best-scoring kinds — which would be selecting on noise, since no ranking in the screening survived
// its sample size. A momentum reading, a mean-reversion reading, a structural level and a
// volatility-breakout reading are as close to independent views as this registry offers, and
// independence is the entire mechanism by which agreement is worth more than any one opinion.
func NewConfluence() *Confluence {
	return &Confluence{
		Members: []Strategy{
			NewMACDMomentum(),      // momentum
			NewVWAPReversion(),     // mean reversion
			NewICTOrderBlock(),     // structure
			NewKeltnerTrendScalp(), // volatility/trend
		},
		MinAgree:    2,
		AgreeWindow: 5,
		SLPct:       decimal.NewFromFloat(0.006),
		TPPct:       decimal.NewFromFloat(0.015),
	}
}
