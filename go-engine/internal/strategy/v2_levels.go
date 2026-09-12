package strategy

import "github.com/shopspring/decimal"

// This file holds what every "_v2" strategy shares: how it turns a trade idea into actual price
// levels. The V1 strategies each did this their own way, and the 2026-09-12 review of 1841 closed
// paper orders showed that was the real problem rather than the entry logic.
//
// Two failure modes came out of that data, and both are structural rather than per-strategy:
//
//  1. UNREACHABLE TARGETS. V1 either used a fixed percentage (macd_momentum's 1.2% regardless of
//     whether BTC or ZEC — instruments whose median 5m candle differs by 5.6x) or derived the
//     target from a raw structural level that could sit a hair from entry. A structurally tight
//     stop then got widened to the MinSLDistPct floor and MinTPSLRatio carried that widening into
//     the target, so a strategy's intended 2:1 became an effective 47:1 (ict_fvg) or 80:1
//     (ict_order_block) and the target landed 60-70% of margin away at 10x. Price on a 5m bar does
//     not travel that far, so those trades could realistically only ever end at the stop.
//
//  2. GIVE-BACK. 309 positions reached +10% of margin or better and 74 of them still closed at a
//     loss, because the only exit they had was a target price could not touch.
//
// V2 answers both by sizing every level in ATR units, so the stop and the target both scale with
// the instrument's own current volatility, and by bounding the reward:risk ratio at the source
// instead of relying on conductor.Clamps to catch it downstream. The clamp is still there and is
// still the real safety boundary — this is defence in depth, the same posture as the 15% loss cap
// living in three independent places (CLAUDE.md §19.2/§19.3/§23), not a replacement for it.

// v2Levels builds entry/stop/target from an ATR measurement.
//
// stopATR sizes the stop as a multiple of ATR. A stop must sit outside the noise a single candle
// routinely produces, or it gets taken out by ordinary wiggle before the idea has a chance; ATR is
// exactly that noise measurement, which a fixed percentage is not.
//
// riskReward then places the target at a multiple of the REALIZED stop distance, so the ratio is
// true by construction rather than being an intention that later arithmetic can distort.
//
// The operator's 2026-09-12 requirement is enforced here as a hard floor: the target's own
// percentage is never allowed below the stop's, i.e. never worse than 1:1. A target nearer than
// the stop is negative-expectancy by construction no matter how good the entry, and V2 must not be
// able to express one even if a parameter search proposes it.
// maxTPPriceFrac caps how far a V2 target may sit from entry as a fraction of entry price.
//
// The ratio bound alone is not sufficient, which real data made clear rather than reasoning: ATR is
// relative to the instrument, so a 1.3-ATR stop at 2:1 is a reachable 8% of margin on BTC (median
// 5m candle 0.081%) and an unreachable 32% on ZEC (0.453%) at the same 10x leverage. The ratio was
// identical and correct in both cases; the absolute distance was not.
//
// 2% of price is 20% of margin at the deployed 10x — inside what a 5m move reaches in a normal
// session, where the 60-70% targets V1 produced are not. Expressed against PRICE rather than margin
// because a strategy cannot see the leverage a position will eventually use; the conversion holds
// as long as leverage does, and conductor.Clamps remains the leverage-aware boundary either way.
var maxTPPriceFrac = decimal.NewFromFloat(0.02)

func v2Levels(side Side, entry, atr, stopATR, riskReward decimal.Decimal) (sl, tp decimal.Decimal, ok bool) {
	if !entry.IsPositive() || !atr.IsPositive() || !stopATR.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}

	risk := atr.Mul(stopATR)
	if !risk.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}

	// On a very volatile instrument an ATR-sized stop can itself be wide enough that even a 1:1
	// target lands out of reach. Cap the RISK first so the reward stays reachable, rather than
	// capping the reward alone — shrinking only the target would silently push the ratio below 1:1,
	// which is the one thing the operator asked never to happen.
	if maxRisk := entry.Mul(maxTPPriceFrac); risk.GreaterThan(maxRisk) {
		risk = maxRisk
	}

	rr := riskReward
	if rr.LessThan(decimal.NewFromInt(1)) {
		// Never below 1:1 — the operator's explicit instruction, and the one property that makes a
		// target worth taking at all.
		rr = decimal.NewFromInt(1)
	}
	reward := risk.Mul(rr)
	// Then bound the reward itself. Because risk is already capped above, trimming the reward here
	// can only reduce the ratio toward — never below — 1:1, which the floor below re-guarantees.
	if maxReward := entry.Mul(maxTPPriceFrac); reward.GreaterThan(maxReward) {
		reward = maxReward
		if reward.LessThan(risk) {
			// Keeping the 1:1 floor is non-negotiable, so when the cap would invert the trade the
			// RISK gives way instead: a tighter stop preserves a real reward:risk rather than
			// producing a target nearer than the stop.
			risk = reward
		}
	}

	if side == Sell {
		sl = entry.Add(risk)
		tp = entry.Sub(reward)
		// A short's target must stay above zero to be a real price. On a token quoted in tiny
		// fractions an over-wide reward could otherwise cross into negative territory, which is
		// not a take-profit at all — the same class of bug as the 2026-08-29 TP-past-entry
		// incident (CLAUDE.md §16.9), guarded rather than assumed impossible.
		if !tp.IsPositive() {
			return decimal.Zero, decimal.Zero, false
		}
		return sl, tp, true
	}

	sl = entry.Sub(risk)
	if !sl.IsPositive() {
		return decimal.Zero, decimal.Zero, false
	}
	tp = entry.Add(reward)
	return sl, tp, true
}

// v2Signal assembles the full Signal, reporting levels as both prices and percentages.
//
// Both representations are emitted deliberately: the price is what reaches the model and the order
// (CLAUDE.md §15.11 — a level derived from structure carries information a percentage cannot), and
// the percentage is what anything reading SLPct/TPPct still expects. Deriving the percentage from
// the SAME price that was just computed keeps the two from disagreeing, which is how V1's audit
// (§16.8) found strategies reporting one risk while carrying another.
func v2Signal(side Side, confidence, entry, atr, stopATR, riskReward decimal.Decimal) Signal {
	sl, tp, ok := v2Levels(side, entry, atr, stopATR, riskReward)
	if !ok {
		return Signal{Side: Hold}
	}
	return Signal{
		Side:       side,
		Confidence: confidence,
		EntryPx:    entry,
		SLPx:       sl,
		TPPx:       tp,
		SLPct:      sl.Sub(entry).Abs().Div(entry),
		TPPct:      tp.Sub(entry).Abs().Div(entry),
	}
}

// v2StopParams is the ParamSpec pair every V2 strategy exposes, so the optimizer tunes the same
// two knobs by the same names across all of them rather than each inventing its own vocabulary.
func v2StopParams(stopATR, riskReward decimal.Decimal) []ParamSpec {
	return []ParamSpec{
		{Name: "stop_atr", Default: stopATR, Min: decimal.NewFromFloat(0.3), Max: decimal.NewFromFloat(5)},
		// Capped at 4 rather than left open: the whole point of V2 is that targets stay reachable,
		// and the production data showed what happens when nothing bounds this.
		{Name: "risk_reward", Default: riskReward, Min: decimal.NewFromInt(1), Max: decimal.NewFromInt(4)},
	}
}
