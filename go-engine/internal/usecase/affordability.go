package usecase

import (
	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

// Affordability decides which tokens a given account size can actually trade (2026-09-08 request).
//
// The problem it solves: OKX's X-Perp contracts have a fixed minimum size (MinSz, in whole
// contracts of CtVal base units each), so one BTC contract costs ~$7.83 of notional while one
// TRUMP contract costs ~$0.22. With the budget split evenly across the roster (equity/tokenCount),
// a $20 account gives each token ~$2 — enough for TRUMP but structurally impossible for BTC. Such
// a token generates signals that can only ever be declined at sizing time, which is noise in the
// logs and a strategy slot spent on nothing.
//
// Leverage deliberately plays NO part here. Sizing converts notional to contracts as
// notional/price/ctVal, with leverage never entering — it decides how much MARGIN is locked for a
// given notional, not how large a position can be. A token unaffordable at 1x is equally
// unaffordable at 10x.
type Affordability struct {
	// MinNotionalUSD is the smallest notional the exchange will accept for this instrument:
	// CtVal * price * MinSz.
	MinNotionalUSD decimal.Decimal
	// BudgetUSD is the per-token budget this check was made against, kept so a caller can explain
	// the decision rather than only report it.
	BudgetUSD decimal.Decimal
	Affordable bool
}

// PerTokenBudget is the notional one token may commit: the account's tradable equity split evenly
// across the active roster, then bounded by any hard per-position ceiling (account.max_position_pct)
// that is tighter still. Returns zero when there is nothing to trade with, which callers must treat
// as "affordable: nothing" rather than dividing by it.
func PerTokenBudget(equityUSD decimal.Decimal, tokenCount int, maxPositionPct decimal.Decimal) decimal.Decimal {
	if !equityUSD.IsPositive() || tokenCount <= 0 {
		return decimal.Zero
	}
	budget := equityUSD.Div(decimal.NewFromInt(int64(tokenCount)))
	if maxPositionPct.IsPositive() {
		if cap := equityUSD.Mul(maxPositionPct); cap.LessThan(budget) {
			budget = cap
		}
	}
	return budget
}

// MinNotionalFor is the smallest notional an instrument will accept — one MinSz lot of CtVal base
// units at the current price. A missing CtVal/MinSz defaults to 1, matching sizeToContracts' own
// fallback, so an instrument this system knows little about is judged the same way it would be
// traded rather than by a different rule.
func MinNotionalFor(inst domain.Instrument, price decimal.Decimal) decimal.Decimal {
	ctVal := inst.CtVal
	if !ctVal.IsPositive() {
		ctVal = decimal.NewFromInt(1)
	}
	minSz := inst.MinSz
	if !minSz.IsPositive() {
		minSz = decimal.NewFromInt(1)
	}
	return ctVal.Mul(price).Mul(minSz)
}

// CanAfford reports whether budgetUSD covers at least one minimum lot of inst at price.
func CanAfford(inst domain.Instrument, price, budgetUSD decimal.Decimal) Affordability {
	minNotional := MinNotionalFor(inst, price)
	return Affordability{
		MinNotionalUSD: minNotional,
		BudgetUSD:      budgetUSD,
		// Strictly "budget >= one lot". A budget exactly equal to one lot is affordable: sizing
		// floors to whole lots, so it buys exactly one.
		Affordable: budgetUSD.IsPositive() && minNotional.IsPositive() && budgetUSD.GreaterThanOrEqual(minNotional),
	}
}

// AffordabilityPlan is the result of evaluating a whole roster: which tokens should be disabled
// because the current budget cannot buy one lot, and which previously-disabled ones can be brought
// back because it now can.
type AffordabilityPlan struct {
	BudgetUSD decimal.Decimal
	// ToDisable/ToEnable are the CHANGES only — a token already in the desired state appears in
	// neither, so a caller can skip writing when both are empty.
	ToDisable []string
	ToEnable  []string
	// Unaffordable is every token that cannot be traded at this budget, whether or not it was
	// already disabled. Reported separately from ToDisable so a caller can log the full picture.
	Unaffordable []string
}

// Changed reports whether applying this plan would alter the disabled set at all.
func (p AffordabilityPlan) Changed() bool { return len(p.ToDisable) > 0 || len(p.ToEnable) > 0 }

// PlanAffordability decides the roster changes for one evaluation.
//
// budget is computed against the count of tokens that would be ACTIVE, which is what makes this
// stable rather than oscillating: disabling a token raises every remaining token's share, which
// could make a token affordable again and re-enable it, which lowers the share again. The caller
// resolves that by iterating to a fixed point (see AffordabilityService.plan) rather than this
// function guessing.
//
// A token whose price or instrument metadata is unavailable is LEFT ALONE — neither disabled nor
// enabled. Acting on missing data would disable a perfectly tradeable token on a transient API
// failure, which is a far worse outcome than briefly leaving the roster as it is.
func PlanAffordability(
	instruments map[string]domain.Instrument,
	prices map[string]decimal.Decimal,
	currentlyDisabled map[string]bool,
	allTokens []string,
	budget decimal.Decimal,
) AffordabilityPlan {
	plan := AffordabilityPlan{BudgetUSD: budget}
	for _, tok := range allTokens {
		inst, hasInst := instruments[tok]
		price, hasPrice := prices[tok]
		if !hasInst || !hasPrice || !price.IsPositive() {
			continue
		}
		aff := CanAfford(inst, price, budget)
		disabled := currentlyDisabled[tok]
		switch {
		case !aff.Affordable:
			plan.Unaffordable = append(plan.Unaffordable, tok)
			if !disabled {
				plan.ToDisable = append(plan.ToDisable, tok)
			}
		case disabled:
			plan.ToEnable = append(plan.ToEnable, tok)
		}
	}
	return plan
}
