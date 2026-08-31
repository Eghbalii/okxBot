package conductor

import (
	"github.com/shopspring/decimal"
)

// Clamps bound the SL/TP price levels the model sets on an open (CLAUDE.md §15.11/§15.12). The
// ratchet (usecase.RatchetSLTP) already governs how levels may MOVE on an update; these govern
// where they may be PLACED in the first place, which the ratchet says nothing about.
//
// This matters most early in training, when the policy is close to random: a stop placed 0.001%
// from entry stops out on noise instantly, one placed 40% away turns a bounded loss into an
// account event, and a target closer than the stop is a negative-expectancy trade by construction
// no matter how good the entry is. None of those are things to hope the model has learned not to
// do — same reasoning as §5's risk manager and §15.4's ratchet.
//
// Zero-valued fields disable the corresponding clamp, so a partially-configured Clamps is valid.
type Clamps struct {
	// MinSLDistPct / MaxSLDistPct bound the stop's distance from entry, as a fraction of entry
	// price (0.005 = 0.5%). This is a raw PRICE-move bound, independent of leverage.
	MinSLDistPct decimal.Decimal
	MaxSLDistPct decimal.Decimal
	// MaxLossPct bounds the REALIZED loss the stop can produce once leverage is applied (0.15 =
	// 15% of margin) — a distinct question from MaxSLDistPct above, which only limits the price
	// distance and says nothing about leverage (2026-08-31 request: "SL should never allow more
	// than 15% loss, at any leverage, but no limit on profit"). At entry+leverage, a price-distance
	// stop of D% produces a D%*leverage loss, so this clamp is enforced by capping the effective
	// price distance to MaxLossPct/leverage — the tighter of that and MaxSLDistPct wins. Deliberately
	// separate from MaxSLDistPct rather than replacing it: MaxSLDistPct still bounds a 1x/unleveraged
	// stop from being absurdly wide, while MaxLossPct is what actually matters once leverage is
	// applied. No equivalent exists for TP — profit is never capped, only loss.
	MaxLossPct decimal.Decimal
	// MinTPSLRatio is the minimum reward:risk ratio — the TP distance must be at least this
	// multiple of the SL distance. A too-near target is widened to satisfy it rather than the trade
	// being rejected, since the entry itself may still be sound.
	MinTPSLRatio decimal.Decimal
}

// maxSLDistPctFor returns the effective SL-distance cap for a position at the given leverage: the
// tighter of the raw MaxSLDistPct and MaxLossPct/leverage. A non-positive leverage is treated as
// 1x (unleveraged), matching usecase.unrealizedPnLPct's same defensive fallback.
func (cl Clamps) maxSLDistPctFor(leverage decimal.Decimal) decimal.Decimal {
	max := cl.MaxSLDistPct
	if cl.MaxLossPct.IsPositive() {
		lev := leverage
		if !lev.IsPositive() {
			lev = decimal.NewFromInt(1)
		}
		lossBound := cl.MaxLossPct.Div(lev)
		if !max.IsPositive() || lossBound.LessThan(max) {
			max = lossBound
		}
	}
	return max
}

// EnsureStop guarantees a position never opens without a stop.
//
// Observed 2026-08-29 as order 80 (TRUMP-USDT-SWAP, sell): stoch_cross emits only TPPct and never
// sets SLPct at all, ResolveLevels leaves a level it was given nothing to derive from at zero,
// buildPaperOrder writes a nil sl_px, and the position opened with a take-profit and unbounded
// downside. Nothing objected anywhere along that path.
//
// A missing stop is filled at MaxSLDistPct — the widest distance the clamps already consider
// acceptable — rather than rejecting the signal. Rejecting would silently disable every
// strategy that expresses only a target (a legitimate design: stoch_cross exits on the opposite
// crossover, not on a stop), whereas a worst-case-but-bounded stop preserves the strategy's intent
// while making the loss finite. It is deliberately the widest rather than the tightest: a stop
// this position never asked for should interfere with the strategy's own exit as little as
// possible, and a tight one would stop out on noise before that exit can trigger.
//
// Returns the levels unchanged when a usable stop is already present, or when no bound is
// configured to derive one from. leverage narrows the effective distance via MaxLossPct — see
// maxSLDistPctFor.
func (cl Clamps) EnsureStop(side string, entryPx, leverage decimal.Decimal, in Levels) Levels {
	if in.SLPx != nil && in.SLPx.IsPositive() {
		return in
	}
	max := cl.maxSLDistPctFor(leverage)
	if !entryPx.IsPositive() || !max.IsPositive() {
		return in
	}
	long := side != "sell"
	px := offsetFrom(long, entryPx, max.Mul(entryPx), true)
	in.SLPx = &px
	return in
}

// Levels is a resolved SL/TP pair. Either may be nil, meaning "not set" — a strategy or model that
// declined to place one is a real case, not an error.
type Levels struct {
	SLPx *decimal.Decimal
	TPPx *decimal.Decimal
}

// Apply clamps proposed SL/TP levels for a position opening at entryPx on the given side ("buy" or
// "sell"), returning the levels actually safe to use.
//
// Order matters: the stop is clamped first, then the target is checked against the CLAMPED stop
// distance. Validating the ratio against the model's original stop would let a rejected stop
// silently justify a target that no longer matches the risk actually being taken.
//
// A level on the wrong side of entry (a long's stop above its entry) is dropped rather than
// mirrored: an inverted level means the model produced something incoherent, and guessing what it
// meant would invent a decision it did not make.
//
// leverage narrows the SL distance bound via MaxLossPct so a leveraged position's stop can never
// realize more than that fraction of margin, regardless of how wide MaxSLDistPct alone would
// allow (2026-08-31 request). TP has no such leverage-aware cap — profit is never limited.
func (cl Clamps) Apply(side string, entryPx, leverage decimal.Decimal, in Levels) Levels {
	if !entryPx.IsPositive() {
		return in
	}
	long := side != "sell"

	out := Levels{}
	slDist := decimal.Zero
	maxSLDist := cl.maxSLDistPctFor(leverage)

	if in.SLPx != nil {
		dist := signedDist(long, entryPx, *in.SLPx, true)
		if dist.IsPositive() {
			dist = clampRange(dist, cl.MinSLDistPct.Mul(entryPx), maxSLDist.Mul(entryPx))
			slDist = dist
			px := offsetFrom(long, entryPx, dist, true)
			out.SLPx = &px
		}
	}

	if in.TPPx != nil {
		dist := signedDist(long, entryPx, *in.TPPx, false)
		if dist.IsPositive() {
			if cl.MinTPSLRatio.IsPositive() && slDist.IsPositive() {
				if min := slDist.Mul(cl.MinTPSLRatio); dist.LessThan(min) {
					dist = min
				}
			}
			px := offsetFrom(long, entryPx, dist, false)
			out.TPPx = &px
		}
	}

	return out
}

// signedDist returns how far px sits from entry in the direction that level is supposed to be on:
// below entry for a long's stop, above for a long's target, mirrored for a short. A level on the
// wrong side yields a non-positive result, which callers drop.
func signedDist(long bool, entry, px decimal.Decimal, isStop bool) decimal.Decimal {
	below := isStop == long // long stop and short target sit below entry
	if below {
		return entry.Sub(px)
	}
	return px.Sub(entry)
}

// offsetFrom is signedDist's inverse: rebuild a price from entry and a positive distance.
func offsetFrom(long bool, entry, dist decimal.Decimal, isStop bool) decimal.Decimal {
	if isStop == long {
		return entry.Sub(dist)
	}
	return entry.Add(dist)
}

// clampRange clamps v into [min, max], ignoring either bound that is non-positive (disabled).
func clampRange(v, min, max decimal.Decimal) decimal.Decimal {
	if min.IsPositive() && v.LessThan(min) {
		v = min
	}
	if max.IsPositive() && v.GreaterThan(max) {
		v = max
	}
	return v
}
