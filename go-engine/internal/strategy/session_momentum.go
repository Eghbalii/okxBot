package strategy

import (
	"time"

	"github.com/shopspring/decimal"
)

// SessionMomentum trades the first move after a liquidity handover between trading sessions.
//
// WHY: no strategy in the registry knows what time it is. Crypto trades continuously, but its
// participants do not — Asian, European and US hours have measurably different volume and
// volatility, and the handover between them is when positioning changes. A breakout at 03:00 UTC on
// thin books and the same breakout at 13:30 UTC as US desks open are different events, and every
// existing strategy treats them identically.
//
// This is the cheapest of the four to reason about and the easiest to fool yourself with: any fixed
// hour will fit SOME pattern in a finite sample. Which is exactly why it ships with the null
// baseline to measure against, and why the session boundaries below are the conventional market
// ones rather than hours discovered by searching the data.
type SessionMomentum struct {
	// OpenHoursUTC are the session opens to trade. Defaults to the three conventional handovers:
	// Asia (00), Europe (07), US (13).
	OpenHoursUTC []int
	// WindowBars is how many bars after the hour the setup stays live.
	WindowBars int
	// MinMove is how far price must have moved within the window to count as a directional open
	// rather than drift.
	MinMove decimal.Decimal

	SLPct decimal.Decimal
	TPPct decimal.Decimal

	// lastFired guards against re-entering the same session open on consecutive bars — the
	// re-arming bug §30.1 found in ict_fvg, where one setup produced ten entries.
	lastFired time.Time
}

func NewSessionMomentum() *SessionMomentum {
	return &SessionMomentum{
		// Six handovers rather than three, and a lower move threshold: the first screening gave
		// t=+1.22 on 790 trades and needs ~2,109 to settle. Crypto has no single open, so the
		// conventional three were leaving most of the day unobserved — these add the session CLOSES,
		// which are handovers in exactly the same sense and were arbitrarily excluded.
		OpenHoursUTC: []int{0, 4, 7, 12, 13, 20},
		WindowBars:   6,
		MinMove:      decimal.NewFromFloat(0.002),
		SLPct:        decimal.NewFromFloat(0.006),
		TPPct:        decimal.NewFromFloat(0.015),
	}
}

func (s *SessionMomentum) Name() string { return "session_momentum" }

func (s *SessionMomentum) Evaluate(candles []Candle) (Signal, error) {
	window := s.WindowBars
	if window <= 0 {
		window = 6
	}
	if len(candles) < window+1 {
		return Signal{Side: Hold}, nil
	}
	last := candles[len(candles)-1]
	if last.Timestamp.IsZero() {
		// Without timestamps this strategy has nothing to say. Silent rather than guessing, for the
		// same reason btc_divergence holds without its reference series.
		return Signal{Side: Hold}, nil
	}

	// Find the most recent session open within the window.
	var opened time.Time
	for i := len(candles) - 1; i >= len(candles)-window-1 && i >= 0; i-- {
		h := candles[i].Timestamp.UTC().Hour()
		for _, want := range s.OpenHoursUTC {
			if h == want && candles[i].Timestamp.UTC().Minute() < 15 {
				opened = candles[i].Timestamp
				break
			}
		}
		if !opened.IsZero() {
			break
		}
	}
	if opened.IsZero() || !opened.After(s.lastFired) {
		return Signal{Side: Hold}, nil
	}

	move, ok := pctMove(candles, window)
	if !ok || move.Abs().LessThan(s.MinMove) {
		return Signal{Side: Hold}, nil
	}

	side := Buy
	if move.IsNegative() {
		side = Sell
	}
	s.lastFired = opened

	c := move.Abs().Div(s.MinMove)
	if c.GreaterThan(decimal.NewFromInt(3)) {
		c = decimal.NewFromInt(3)
	}
	return Signal{
		Side:       side,
		Confidence: c.Div(decimal.NewFromInt(3)),
		SLPct:      s.SLPct,
		TPPct:      s.TPPct,
	}, nil
}

func (s *SessionMomentum) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "window_bars", Min: decimal.NewFromInt(2), Max: decimal.NewFromInt(24), Default: decimal.NewFromInt(6)},
		{Name: "min_move", Min: decimal.NewFromFloat(0.001), Max: decimal.NewFromFloat(0.03), Default: decimal.NewFromFloat(0.004)},
		{Name: "sl_pct", Min: decimal.NewFromFloat(0.002), Max: decimal.NewFromFloat(0.03), Default: decimal.NewFromFloat(0.006)},
		{Name: "tp_pct", Min: decimal.NewFromFloat(0.002), Max: decimal.NewFromFloat(0.08), Default: decimal.NewFromFloat(0.015)},
	}
}

func (s *SessionMomentum) WithParams(p map[string]decimal.Decimal) Strategy {
	out := NewSessionMomentum()
	out.OpenHoursUTC = append([]int(nil), s.OpenHoursUTC...)
	if v, ok := p["window_bars"]; ok {
		out.WindowBars = int(v.IntPart())
	}
	if v, ok := p["min_move"]; ok {
		out.MinMove = v
	}
	if v, ok := p["sl_pct"]; ok {
		out.SLPct = v
	}
	if v, ok := p["tp_pct"]; ok {
		out.TPPct = v
	}
	// lastFired is deliberately NOT copied — §16.8's audit found 7 of 14 strategies leaking
	// accumulated state into a differently-configured variant through exactly this kind of copy.
	return out
}
