package strategy

import (
	"math"

	"github.com/shopspring/decimal"
)

// BBBreakout is a Go port of the real PineScript source the operator supplied
// (pinescript/tv_ports_20260916/bb_breakout.pine, @version=4 "Bollinger Bands Filter" by
// TradeChartist), replacing the first pass's from-description implementation (2026-09-16), which
// used BB(20,2) with a close-above-upper-band trigger and a basis-derived stop — the real source
// uses different defaults, a `barssince` edge-triggered state machine rather than a bare band touch,
// and has no stop-loss/take-profit of its own at all.
//
// Real parameters, read directly from the source:
//   - SMA length 55 (`length`), not 20 — the source's own comment notes "20 for classic Bollinger
//     Bands SMA line (basis)", but its actual default is 55.
//   - Standard deviation multiplier 1.0 (`mult`), not 2 — again, 2 is the "classic" value the
//     source's comment cites, but its own default is 1.
//
// Signal logic, exactly the source's own construction: `short = close < lower`, `long = close >
// upper` (note: touching/closing beyond the band, not merely approaching it). `L1`/`S1` are
// `barssince(long)`/`barssince(short)` — bars since each condition last held. A long SIGNAL fires
// only on the bar where `L1 < S1` FIRST becomes true (edge-triggered: "long happened more recently
// than short, and did not already hold that way last bar") — i.e. the strategy tracks which side
// last touched its band and fires once when that side changes, not every bar the raw touch
// condition holds. Mirrored for short. The source has no explicit exit/stop/target of its own
// (`strategy.close` on the opposite signal only) — SL/TP are a documented addition here, since every
// strategy in this registry needs one (CLAUDE.md §16.9's "a position opened with no stop-loss"
// incident); the band basis is used as the natural invalidation level.
type BBBreakout struct {
	Period     int
	Mult       decimal.Decimal
	RiskReward decimal.Decimal
}

func NewBBBreakout() *BBBreakout {
	return &BBBreakout{
		Period:     55,
		Mult:       decimal.NewFromInt(1),
		RiskReward: decimal.NewFromFloat(1.5),
	}
}

func (s *BBBreakout) Name() string { return "bb_breakout" }

func (s *BBBreakout) Params() []ParamSpec {
	return []ParamSpec{
		{Name: "period", Default: decimal.NewFromInt(int64(s.Period)), Min: decimal.NewFromInt(5), Max: decimal.NewFromInt(200)},
		{Name: "mult", Default: s.Mult, Min: decimal.NewFromFloat(0.236), Max: decimal.NewFromInt(2)},
		{Name: "risk_reward", Default: s.RiskReward, Min: decimal.NewFromFloat(0.2), Max: decimal.NewFromInt(10)},
	}
}

func (s *BBBreakout) WithParams(values map[string]decimal.Decimal) Strategy {
	cp := *s
	spec := paramsByName(s.Params())
	if v, ok := values["period"]; ok {
		cp.Period = int(ClampParam(spec["period"], v).IntPart())
	}
	if v, ok := values["mult"]; ok {
		cp.Mult = ClampParam(spec["mult"], v)
	}
	if v, ok := values["risk_reward"]; ok {
		cp.RiskReward = ClampParam(spec["risk_reward"], v)
	}
	return &cp
}

// barsSinceTrue mirrors Pine's barssince(): the number of bars since cond[i] was last true,
// counting backward from the last index. Returns -1 (never) if cond never held.
func barsSinceTrue(cond []bool) int {
	for i := len(cond) - 1; i >= 0; i-- {
		if cond[i] {
			return len(cond) - 1 - i
		}
	}
	return -1
}

// bollingerSeries computes the (basis, upper, lower) band series for every index from period-1
// onward using a single pass with a running sum/sum-of-squares, rather than recomputing SMA/StdDev
// from scratch (BollingerBands' own O(period) scan) at every index — the same "series, not O(n)
// repeated whole-window calls" reasoning EMASeries/rsiSeriesWindowed exist for. Needed here because
// bb_breakout must inspect many trailing indices at once (barssince), unlike most strategies which
// only ever need the LATEST band value.
func bollingerSeries(candles []Candle, period int, mult decimal.Decimal) (upper, lower []decimal.Decimal) {
	n := len(candles)
	upper = make([]decimal.Decimal, n)
	lower = make([]decimal.Decimal, n)
	if n < period {
		return upper, lower
	}
	// sum/sumSq are ROUNDED at every step (rollingScale dp). A running total built purely from
	// Add/Sub never truncates on its own — decimal.Decimal is arbitrary-precision — so an unrounded
	// accumulator over 1000+ candles grows without bound, exactly the EMA-precision incident CLAUDE.md
	// §16.8/§44 already document once (there, a value fed back into its own next multiplication;
	// here, an add-then-subtract rolling window has the identical unbounded-growth shape). Confirmed
	// live: without this rounding, `sumSq` on real BTC closes grows to the point that a single
	// decimal.Add takes tens of seconds (math/big.Int.Exp dominates the profile), turning what should
	// be a cheap O(n) series computation into a multi-minute hang.
	const rollingScale = 12
	periodDec := decimal.NewFromInt(int64(period))
	sum, sumSq := decimal.Zero, decimal.Zero
	for i := 0; i < period; i++ {
		c := candles[i].Close
		sum = sum.Add(c).Round(rollingScale)
		sumSq = sumSq.Add(c.Mul(c)).Round(rollingScale)
	}
	computeAt := func(i int) {
		mean := sum.Div(periodDec)
		variance := sumSq.Div(periodDec).Sub(mean.Mul(mean))
		if variance.IsNegative() {
			variance = decimal.Zero // guards against float rounding pushing variance just below zero
		}
		f, _ := variance.Float64()
		sd := decimal.NewFromFloat(math.Sqrt(f))
		band := sd.Mul(mult)
		upper[i] = mean.Add(band)
		lower[i] = mean.Sub(band)
	}
	computeAt(period - 1)
	for i := period; i < n; i++ {
		oldC := candles[i-period].Close
		newC := candles[i].Close
		sum = sum.Sub(oldC).Add(newC).Round(rollingScale)
		sumSq = sumSq.Sub(oldC.Mul(oldC)).Add(newC.Mul(newC)).Round(rollingScale)
		computeAt(i)
	}
	return upper, lower
}

func (s *BBBreakout) Evaluate(candles []Candle) (Signal, error) {
	// Need enough history to compute the band series over a trailing window AND still have two
	// consecutive barssince readings (this bar and the prior bar) to edge-detect on.
	need := s.Period + 3
	if len(candles) < need {
		return Signal{Side: Hold}, nil
	}

	// Build long/short condition series over a bounded trailing window so barssince stays cheap.
	lookback := 300
	if lookback > len(candles)-s.Period {
		lookback = len(candles) - s.Period
	}
	start := len(candles) - lookback

	// bollingerSeries computes its series from index 0 of whatever it's given — calling it on the
	// full, ever-growing candle history recomputed the entire band series from scratch on every
	// Evaluate call even though only the trailing `lookback` values are ever read, turning an O(n)
	// helper into effectively O(n) work PER CALL against a window that keeps growing (found running
	// this file's own test suite: a driving loop of hundreds of calls over a 1000+ candle window
	// hung for minutes). Trimmed to exactly what's needed: `period` candles of warm-up before the
	// first index this call will read, plus the lookback window itself.
	trimStart := start - s.Period + 1
	if trimStart < 0 {
		trimStart = 0
	}
	trimmed := candles[trimStart:]
	upperSeries, lowerSeries := bollingerSeries(trimmed, s.Period, s.Mult)
	offset := start - trimStart

	longCond := make([]bool, lookback)
	shortCond := make([]bool, lookback)
	for i := 0; i < lookback; i++ {
		idx := offset + i
		c := trimmed[idx].Close
		longCond[i] = c.GreaterThan(upperSeries[idx])
		shortCond[i] = c.LessThan(lowerSeries[idx])
	}

	nowL1 := barsSinceTrue(longCond)
	nowS1 := barsSinceTrue(shortCond)
	prevL1 := barsSinceTrue(longCond[:len(longCond)-1])
	prevS1 := barsSinceTrue(shortCond[:len(shortCond)-1])

	nowLongLeads := nowL1 >= 0 && (nowS1 < 0 || nowL1 < nowS1)
	prevLongLeads := prevL1 >= 0 && (prevS1 < 0 || prevL1 < prevS1)
	nowShortLeads := nowS1 >= 0 && (nowL1 < 0 || nowS1 < nowL1)
	prevShortLeads := prevS1 >= 0 && (prevL1 < 0 || prevS1 < prevL1)

	longSignal := nowLongLeads && !prevLongLeads
	shortSignal := nowShortLeads && !prevShortLeads

	last := candles[len(candles)-1]
	basis, _, _, err := BollingerBands(candles, s.Period, s.Mult)
	if err != nil {
		return Signal{}, err
	}

	switch {
	case longSignal:
		risk := last.Close.Sub(basis)
		if !risk.IsPositive() {
			return Signal{Side: Buy, Confidence: decimal.NewFromFloat(0.55)}, nil
		}
		return Signal{
			Side:       Buy,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       basis,
			TPPx:       last.Close.Add(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	case shortSignal:
		risk := basis.Sub(last.Close)
		if !risk.IsPositive() {
			return Signal{Side: Sell, Confidence: decimal.NewFromFloat(0.55)}, nil
		}
		return Signal{
			Side:       Sell,
			Confidence: decimal.NewFromFloat(0.55),
			EntryPx:    last.Close,
			SLPx:       basis,
			TPPx:       last.Close.Sub(risk.Mul(s.RiskReward)),
			SLPct:      risk.Div(last.Close),
			TPPct:      risk.Div(last.Close).Mul(s.RiskReward),
		}, nil
	default:
		return Signal{Side: Hold}, nil
	}
}
