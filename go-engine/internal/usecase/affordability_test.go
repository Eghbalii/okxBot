package usecase

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
)

func inst(ctVal, minSz string) domain.Instrument {
	return domain.Instrument{CtVal: dec(ctVal), MinSz: dec(minSz), LotSz: dec("1")}
}

// The real numbers that motivated this: on a $20 account across 10 tokens, one BTC X-Perp contract
// costs ~$7.83 of notional while the per-token budget is $2, so BTC is structurally untradeable
// while TRUMP at $0.22 is fine.
func TestCanAfford_RealRosterNumbers(t *testing.T) {
	budget := dec("2")
	cases := []struct {
		name       string
		ctVal      string
		price      string
		affordable bool
	}{
		{"BTC", "0.0001", "78350", false}, // 7.835
		{"HYPE", "0.1", "84.19", false},   // 8.419
		{"ETH", "0.001", "2480.2", false}, // 2.4802
		{"SOL", "0.01", "103.06", true},   // 1.0306
		{"XRP", "1", "1.4233", true},      // 1.4233
		{"TRUMP", "0.1", "2.24", true},    // 0.224
	}
	for _, c := range cases {
		got := CanAfford(inst(c.ctVal, "1"), dec(c.price), budget)
		if got.Affordable != c.affordable {
			t.Errorf("%s: minNotional=%s budget=%s — affordable %v, want %v",
				c.name, got.MinNotionalUSD.StringFixed(4), budget, got.Affordable, c.affordable)
		}
	}
}

// A budget exactly equal to one lot buys exactly one lot, since sizing floors to whole lots.
func TestCanAfford_ExactlyOneLotIsAffordable(t *testing.T) {
	got := CanAfford(inst("0.01", "1"), dec("100"), dec("1"))
	if !got.Affordable {
		t.Fatalf("a budget exactly equal to one lot (%s) must be affordable", got.MinNotionalUSD)
	}
}

func TestPerTokenBudget(t *testing.T) {
	// Even split, no cap binding: 20/10 = 2.
	if got := PerTokenBudget(dec("20"), 10, dec("0.25")); !got.Equal(dec("2")) {
		t.Errorf("even split: want 2, got %s", got)
	}
	// With few tokens the even share (20/2=10) exceeds the 25% cap (5), so the cap binds.
	if got := PerTokenBudget(dec("20"), 2, dec("0.25")); !got.Equal(dec("5")) {
		t.Errorf("cap should bind with a small roster: want 5, got %s", got)
	}
	// A drained account has nothing to size against.
	if got := PerTokenBudget(dec("0"), 10, dec("0.25")); !got.IsZero() {
		t.Errorf("drained account: want 0, got %s", got)
	}
	if got := PerTokenBudget(dec("20"), 0, dec("0.25")); !got.IsZero() {
		t.Errorf("no active tokens: want 0, got %s", got)
	}
}

// A token whose instrument or price could not be read must be left ALONE. Disabling on missing
// data would take a tradeable token offline on a transient API failure.
func TestPlanAffordability_MissingDataLeavesTokenUntouched(t *testing.T) {
	instruments := map[string]domain.Instrument{"SOL": inst("0.01", "1")}
	prices := map[string]decimal.Decimal{"SOL": dec("100")}

	plan := PlanAffordability(instruments, prices, map[string]bool{}, []string{"SOL", "BTC"}, dec("2"))
	for _, tok := range append(append([]string{}, plan.ToDisable...), plan.ToEnable...) {
		if tok == "BTC" {
			t.Fatal("a token with no instrument/price data must not be disabled or enabled")
		}
	}
}

// Re-enabling is the other half of the requirement: profit raises the per-token budget, and a token
// that was disabled for being unaffordable must come back on its own.
func TestPlanAffordability_ReEnablesWhenBudgetGrows(t *testing.T) {
	instruments := map[string]domain.Instrument{"ETH": inst("0.001", "1")}
	prices := map[string]decimal.Decimal{"ETH": dec("2480")} // one lot = 2.48

	// $2 budget: unaffordable, and already disabled — no change proposed.
	plan := PlanAffordability(instruments, prices, map[string]bool{"ETH": true}, []string{"ETH"}, dec("2"))
	if len(plan.ToEnable) != 0 || len(plan.ToDisable) != 0 {
		t.Fatalf("already-correct state must propose no change, got %+v", plan)
	}

	// $4 budget after some profit: now affordable, so bring it back.
	plan = PlanAffordability(instruments, prices, map[string]bool{"ETH": true}, []string{"ETH"}, dec("4"))
	if len(plan.ToEnable) != 1 || plan.ToEnable[0] != "ETH" {
		t.Fatalf("a now-affordable token must be re-enabled, got %+v", plan.ToEnable)
	}
}

// Disabling a token raises every remaining token's share, so the decision has to account for what
// it frees up: $20 across 4 tokens is $5 each, which cannot afford BTC ($7.83) — but with BTC out
// the remaining 3 get $6.67 each and all fit.
func TestAffordabilityService_AccountsForFreedBudget(t *testing.T) {
	s := &AffordabilityService{AllTokens: []string{"BTC", "ETH", "SOL", "XRP"}}
	instruments := map[string]domain.Instrument{
		"BTC": inst("0.0001", "1"), "ETH": inst("0.001", "1"),
		"SOL": inst("0.01", "1"), "XRP": inst("1", "1"),
	}
	prices := map[string]decimal.Decimal{
		"BTC": dec("78350"), "ETH": dec("2480"), "SOL": dec("103"), "XRP": dec("1.42"),
	}

	plan := s.plan(instruments, prices, map[string]bool{}, dec("20"))

	if len(plan.ToDisable) != 1 || plan.ToDisable[0] != "BTC" {
		t.Fatalf("only BTC should end up disabled, got %+v", plan.ToDisable)
	}
	if !plan.BudgetUSD.Round(2).Equal(dec("6.67")) {
		t.Fatalf("budget must reflect the SETTLED roster of 3: want 6.67, got %s", plan.BudgetUSD.Round(2))
	}
}

// Growing the account brings tokens back, against the settled roster the same way.
func TestAffordabilityService_ReEnablesAsEquityGrows(t *testing.T) {
	s := &AffordabilityService{AllTokens: []string{"BTC", "ETH", "SOL"}}
	instruments := map[string]domain.Instrument{
		"BTC": inst("0.0001", "1"), "ETH": inst("0.001", "1"), "SOL": inst("0.01", "1"),
	}
	prices := map[string]decimal.Decimal{"BTC": dec("78350"), "ETH": dec("2480"), "SOL": dec("103")}

	plan := s.plan(instruments, prices, map[string]bool{"BTC": true}, dec("100"))
	if len(plan.ToEnable) != 1 || plan.ToEnable[0] != "BTC" {
		t.Fatalf("BTC must be re-enabled once affordable, got %+v", plan.ToEnable)
	}
	if len(plan.ToDisable) != 0 {
		t.Fatalf("nothing should be disabled at this equity, got %+v", plan.ToDisable)
	}
}

// An already-correct roster must propose no write, so the config row is not churned every tick.
func TestAffordabilityService_NoChangeWhenAlreadyCorrect(t *testing.T) {
	// The roster is deliberately large enough that BTC stays unaffordable even after the freed
	// budget is redistributed: 5 tokens with BTC out gives $20/4 = $5 each, still short of BTC's
	// $7.835 lot. An earlier version used a 2-token roster and failed — correctly, since $20
	// across 2 tokens is $10 each and BTC genuinely IS affordable there. The code was right and
	// the expectation was wrong.
	s := &AffordabilityService{AllTokens: []string{"BTC", "SOL", "XRP", "TRUMP", "DOGE"}}
	instruments := map[string]domain.Instrument{
		"BTC": inst("0.0001", "1"), "SOL": inst("0.01", "1"), "XRP": inst("1", "1"),
		"TRUMP": inst("0.1", "1"), "DOGE": inst("10", "1"),
	}
	prices := map[string]decimal.Decimal{
		"BTC": dec("78350"), "SOL": dec("103"), "XRP": dec("1.42"),
		"TRUMP": dec("2.24"), "DOGE": dec("0.0896"),
	}

	plan := s.plan(instruments, prices, map[string]bool{"BTC": true}, dec("20"))
	if plan.Changed() {
		t.Fatalf("already-correct roster must propose no change, got disable=%+v enable=%+v",
			plan.ToDisable, plan.ToEnable)
	}
}

// applyPlan must return a sorted list — an unsorted one makes every save look like a change even
// when the set is identical.
func TestApplyPlan_SortedAndStable(t *testing.T) {
	s := &AffordabilityService{AllTokens: []string{"BTC", "ETH", "SOL", "ZEC"}}
	got := s.applyPlan(map[string]bool{"ZEC": true}, AffordabilityPlan{ToDisable: []string{"ETH", "BTC"}})
	want := []string{"BTC", "ETH", "ZEC"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v (sorted), got %v", want, got)
		}
	}
}

// Regression for a genuine infinite oscillation, found by simulating the real 10-token roster on a
// $20 account BEFORE deploying. An iterate-until-stable design never settles here: PEPE (minimum
// $3.64) is affordable at 5 active tokens ($4.00 budget) and unaffordable at 6 ($3.33), so enabling
// it is precisely what makes it unaffordable, forever.
//
// The greedy cheapest-first pass has no such failure mode — every token is judged against the
// budget implied by its OWN admission — so this asserts both that it terminates and that the result
// is self-consistent: every admitted token must still afford its lot at the final budget.
func TestAffordabilityService_RealRosterHasNoOscillation(t *testing.T) {
	tokens := []string{"BTC", "ETH", "SOL", "ZEC", "XRP", "DOGE", "HYPE", "TRUMP", "PEPE", "PUMP"}
	instruments := map[string]domain.Instrument{
		"BTC": inst("0.0001", "1"), "ETH": inst("0.001", "1"), "SOL": inst("0.01", "1"),
		"ZEC": inst("0.01", "1"), "XRP": inst("1", "1"), "DOGE": inst("10", "1"),
		"HYPE": inst("0.1", "1"), "TRUMP": inst("0.1", "1"), "PEPE": inst("1000000", "1"),
		"PUMP": inst("1000", "1"),
	}
	prices := map[string]decimal.Decimal{
		"BTC": dec("78350"), "ETH": dec("2480.2"), "SOL": dec("103.06"), "ZEC": dec("1151.78"),
		"XRP": dec("1.4233"), "DOGE": dec("0.08963"), "HYPE": dec("84.189"), "TRUMP": dec("2.24"),
		"PEPE": dec("0.000003644"), "PUMP": dec("0.0044090"),
	}
	s := &AffordabilityService{AllTokens: tokens, MaxPositionPct: dec("0.25")}

	plan := s.plan(instruments, prices, map[string]bool{}, dec("20"))

	disabled := map[string]bool{}
	for _, t := range plan.ToDisable {
		disabled[t] = true
	}
	// Self-consistency: every token left ACTIVE must afford one lot at the settled budget. This is
	// the property the oscillating version could not hold — it would leave PEPE active at a budget
	// that could not buy it.
	for _, tok := range tokens {
		if disabled[tok] {
			continue
		}
		minNotional := MinNotionalFor(instruments[tok], prices[tok])
		if plan.BudgetUSD.LessThan(minNotional) {
			t.Errorf("%s left active but its lot (%s) exceeds the settled budget (%s)",
				tok, minNotional.StringFixed(4), plan.BudgetUSD.StringFixed(4))
		}
	}
	if len(plan.ToDisable) == 0 {
		t.Fatal("expected the expensive tokens to be disabled on a $20 account")
	}
}

// Running the plan twice must give the same answer — the second pass, starting from the first's
// result, must propose no further change. An oscillating design fails this immediately.
func TestAffordabilityService_IsIdempotent(t *testing.T) {
	tokens := []string{"BTC", "ETH", "SOL", "XRP", "PEPE"}
	instruments := map[string]domain.Instrument{
		"BTC": inst("0.0001", "1"), "ETH": inst("0.001", "1"), "SOL": inst("0.01", "1"),
		"XRP": inst("1", "1"), "PEPE": inst("1000000", "1"),
	}
	prices := map[string]decimal.Decimal{
		"BTC": dec("78350"), "ETH": dec("2480.2"), "SOL": dec("103.06"),
		"XRP": dec("1.4233"), "PEPE": dec("0.000003644"),
	}
	s := &AffordabilityService{AllTokens: tokens, MaxPositionPct: dec("0.25")}

	first := s.plan(instruments, prices, map[string]bool{}, dec("20"))
	settled := map[string]bool{}
	for _, t := range first.ToDisable {
		settled[t] = true
	}

	second := s.plan(instruments, prices, settled, dec("20"))
	if second.Changed() {
		t.Fatalf("a settled roster must be stable, but a second pass proposed disable=%v enable=%v",
			second.ToDisable, second.ToEnable)
	}
}
