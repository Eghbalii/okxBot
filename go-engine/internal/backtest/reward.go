package backtest

import (
	"math"

	"github.com/shopspring/decimal"
)

// The reward, ported from rl_service/reward.py.
//
// A SECOND IMPLEMENTATION IS A LIABILITY, and this one is accepted deliberately rather than by
// default. The alternative is calling into Python from a Go replay loop — a process boundary per
// trade across a dataset of thousands — which would make the run slow enough to discourage
// regenerating it, and a dataset nobody regenerates is one that quietly outlives the schema it was
// built for.
//
// What makes the duplication safe is that the numbers are pinned: reward_test.go asserts this
// produces the SAME value as reward.py for the same inputs, using the figures reward.py's own tests
// use. If either drifts, a test fails rather than the warm start quietly optimising a different
// objective than production scores — which is the §19.1 skew class, and exactly what v8 exists to
// remove.
//
// Every constant below is the same value and the same reasoning as reward.py's. Change them
// together or not at all.
const (
	// typicalReturn anchors the weights: the median absolute risk-adjusted return of a real closed
	// trade, measured over 2084 paper_orders on 2026-09-14. A weight of W costs "W typical trades"
	// at full strength.
	typicalReturn = 0.05

	// churnWeight: moving SL/TP must earn its keep rather than being free to twitch (§15.5).
	churnWeight = 0.004

	// leverageWeight makes LEVERAGE ITSELF expensive. The drawdown term only charges for losses
	// already taken, so without this the policy could hold maximum leverage indefinitely at no cost
	// right up until it blew up (§15.13).
	leverageWeight = 0.6
	// liqBufferFloor: distance to liquidation, as a fraction of price, at or beyond which leverage
	// is free. 10% is exactly one 10x position — deliberate, since 10x is the OKX cap this project
	// trades at, and penalising the only leverage available would be a constant, not a signal.
	liqBufferFloor = 0.10

	// drawdownWeight charges for distance below the high-water mark, which is what makes a round
	// trip cost something: reward is per trade, so running the account up and giving it all back
	// nets to ~0 and would read as no worse than never having traded (§15.13).
	drawdownWeight = 0.3

	// rewardClip bounds one trade's score. A stop is capped at 15% of margin (§19.2) so a realistic
	// loss cannot exceed ~1.0; a data error or a gapped fill can, and one outlier distorts every
	// sample drawn from a small replay buffer afterwards.
	rewardClip = 3.0
)

// RewardInput is one closed trade, as the reward sees it.
type RewardInput struct {
	RealizedPnLUSD decimal.Decimal
	FeesUSD        decimal.Decimal
	// RiskPct is the entry-to-stop distance as a fraction of margin — what the trade actually put
	// at risk, and what the reward divides by. Zero falls back to position size, which reduces this
	// to return on capital: worse, but defined.
	RiskPct       decimal.Decimal
	PositionSize  decimal.Decimal
	Leverage      decimal.Decimal
	EquityUSD     decimal.Decimal
	PeakEquityUSD decimal.Decimal
	Adjustments   int
	// ZeroReward is for an operator's manual close (§15.12): no gradient follows a decision the
	// policy did not make.
	ZeroReward bool
}

// TradeReward scores one closed trade. Mirrors reward.py's trade_reward exactly.
func TradeReward(in RewardInput) float64 {
	if in.ZeroReward {
		return 0
	}

	pnl, _ := in.RealizedPnLUSD.Float64()
	fees, _ := in.FeesUSD.Float64()
	net := pnl - math.Abs(fees)

	risk, _ := in.RiskPct.Float64()
	size, _ := in.PositionSize.Float64()

	var denom float64
	switch {
	case risk > 0 && size > 0:
		denom = risk * size
	case size > 0:
		denom = size
	default:
		return 0
	}

	ret := net / denom
	lev, _ := in.Leverage.Float64()
	eq, _ := in.EquityUSD.Float64()
	peak, _ := in.PeakEquityUSD.Float64()

	total := ret -
		churnWeight*float64(maxInt(0, in.Adjustments)) -
		leverageWeight*LiquidationPenalty(lev) -
		drawdownWeight*DrawdownPct(eq, peak)

	return math.Max(-rewardClip, math.Min(rewardClip, total))
}

// LiquidationPenalty is the [0,1] cost of sitting close to liquidation, from leverage alone: at Nx
// an adverse move of roughly 1/N wipes the position out. Approximate by design — it ignores
// maintenance-margin tiers — matching the same conservative approximation the risk manager uses.
func LiquidationPenalty(leverage float64) float64 {
	if leverage <= 0 {
		return 0
	}
	buffer := 1.0 / leverage
	if buffer >= liqBufferFloor {
		return 0
	}
	return (liqBufferFloor - buffer) / liqBufferFloor
}

// DrawdownPct is how far below its high-water mark the account sits, in [0,1]. Measured from the
// PEAK rather than the starting balance: a cap change rewrites the starting balance (§32.2), which
// would wipe the model's view of drawdown back to zero.
func DrawdownPct(equity, peak float64) float64 {
	if peak <= 0 {
		return 0
	}
	return math.Max(0, (peak-equity)/peak)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// log10 is math.Log10, named locally so tokenProfile reads the same as its usecase counterpart.
func log10(f float64) float64 { return math.Log10(f) }

// typicalReturn is documentation for the weights above; referenced so it cannot silently rot.

// minTradableUSD is the balance below which the simulated account is considered drained.
//
// Not zero: at $0.05 spread across position slots, an order is sized in fractions of a cent, and a
// trade that small is not a decision anyone would make — it is arithmetic continuing after the
// account is gone. Measured on real history without this floor, the account reached $0.000007 and
// the run went on producing 21,000 more samples at sizes production would never open.
var minTradableUSD = decimal.NewFromFloat(1.0)

// SetTakerFee overrides the fee rate for a run.
//
// Exposed because the first full screening found 35 of 36 strategies losing money, and the fee is a
// large enough share of a typical trade to be a candidate cause rather than a rounding detail: at
// 0.05% taker both ways on 10x leverage it costs 1% of margin per round trip, against an average
// stop risking ~6.8%. Being able to vary it is how that stops being an assertion and becomes a
// measurement.
func SetTakerFee(rate decimal.Decimal) { takerFeeRate = rate }
