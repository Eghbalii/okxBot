package strategy

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// v2Kinds is every registered V2 strategy, derived from the registry rather than hardcoded so a
// newly added one is covered automatically instead of being silently skipped.
func v2Kinds() []string {
	out := []string{}
	for k := range Factories {
		if strings.HasSuffix(k, "_v2") {
			out = append(out, k)
		}
	}
	return out
}

// driveV2 feeds candles one closed bar at a time, the way the engine does, and collects signals.
// Passing a whole sequence in one call would exercise a code path production never takes.
func driveV2(t *testing.T, s Strategy, cs []Candle, from int) []Signal {
	t.Helper()
	var sigs []Signal
	for i := from; i <= len(cs); i++ {
		sig, err := s.Evaluate(cs[:i])
		if err != nil {
			t.Fatalf("%s: evaluate at %d: %v", s.Name(), i, err)
		}
		if sig.Side == Buy || sig.Side == Sell {
			sigs = append(sigs, sig)
		}
	}
	return sigs
}

// TestV2_AllFire is the anti-vacuity guard: a strategy that never signals cannot have its levels
// checked, and would pass every other test in this file trivially. pmax shipped inert for exactly
// this reason (CLAUDE.md §16.8) — registered, assignable, and silently returning Hold forever.
func TestV2_AllFire(t *testing.T) {
	cs := loadRealCandles(t, "SOL")
	for _, kind := range v2Kinds() {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			sigs := driveV2(t, Factories[kind](), cs, 250)
			if len(sigs) == 0 {
				t.Fatalf("%s never fired on 700 realistic candles — inert or over-filtered", kind)
			}
			t.Logf("%s fired %d times", kind, len(sigs))
		})
	}
}

// TestV2_LevelsAreCoherent asserts every emitted signal is a tradeable trade: the stop on the
// losing side of entry, the target on the winning side. The sign errors in this codebase's history
// (§16.9's inverted TP, which closed trades at a loss under close_reason='tp') were all of this
// shape, so both directions are checked explicitly rather than assumed to mirror.
func TestV2_LevelsAreCoherent(t *testing.T) {
	cs := loadRealCandles(t, "SOL")
	for _, kind := range v2Kinds() {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			sigs := driveV2(t, Factories[kind](), cs, 250)
			if len(sigs) == 0 {
				t.Fatalf("%s produced no signals", kind)
			}
			for i, sig := range sigs {
				if sig.EntryPx.IsZero() || sig.SLPx.IsZero() || sig.TPPx.IsZero() {
					t.Fatalf("%s signal %d has a zero level: entry=%s sl=%s tp=%s",
						kind, i, sig.EntryPx, sig.SLPx, sig.TPPx)
				}
				if !sig.SLPx.IsPositive() || !sig.TPPx.IsPositive() {
					t.Fatalf("%s signal %d has a non-positive price: sl=%s tp=%s", kind, i, sig.SLPx, sig.TPPx)
				}
				if sig.Side == Buy {
					if !sig.SLPx.LessThan(sig.EntryPx) {
						t.Fatalf("%s buy signal %d: stop %s is not below entry %s", kind, i, sig.SLPx, sig.EntryPx)
					}
					if !sig.TPPx.GreaterThan(sig.EntryPx) {
						t.Fatalf("%s buy signal %d: target %s is not above entry %s", kind, i, sig.TPPx, sig.EntryPx)
					}
				} else {
					if !sig.SLPx.GreaterThan(sig.EntryPx) {
						t.Fatalf("%s sell signal %d: stop %s is not above entry %s", kind, i, sig.SLPx, sig.EntryPx)
					}
					if !sig.TPPx.LessThan(sig.EntryPx) {
						t.Fatalf("%s sell signal %d: target %s is not below entry %s", kind, i, sig.TPPx, sig.EntryPx)
					}
				}
			}
		})
	}
}

// TestV2_RiskRewardStaysInBounds is the operator's 2026-09-12 requirement expressed as a test:
// never worse than 1:1 (a target nearer than the stop is negative expectancy by construction), and
// never so wide that price cannot reach it — the failure that produced 60-70%-of-margin targets.
func TestV2_RiskRewardStaysInBounds(t *testing.T) {
	cs := loadRealCandles(t, "SOL")
	for _, kind := range v2Kinds() {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			sigs := driveV2(t, Factories[kind](), cs, 250)
			if len(sigs) == 0 {
				t.Fatalf("%s produced no signals", kind)
			}
			for i, sig := range sigs {
				risk := sig.EntryPx.Sub(sig.SLPx).Abs()
				reward := sig.TPPx.Sub(sig.EntryPx).Abs()
				if !risk.IsPositive() {
					t.Fatalf("%s signal %d has zero risk", kind, i)
				}
				rr := reward.Div(risk)
				if rr.LessThan(decimal.NewFromFloat(0.9999)) {
					t.Fatalf("%s signal %d: R:R %s is below the 1:1 floor", kind, i, rr)
				}
				if rr.GreaterThan(decimal.NewFromInt(4)) {
					t.Fatalf("%s signal %d: R:R %s exceeds 4:1 — the unreachable-target failure", kind, i, rr)
				}
				// And the percentage fields must agree with the prices they were derived from,
				// since both reach the model and a disagreement is invisible until it misleads it.
				if diff := sig.TPPct.Sub(reward.Div(sig.EntryPx)).Abs(); diff.GreaterThan(decimal.NewFromFloat(1e-9)) {
					t.Fatalf("%s signal %d: TPPct %s disagrees with its own price", kind, i, sig.TPPct)
				}
				if sig.TPPct.LessThan(sig.SLPct) {
					t.Fatalf("%s signal %d: TPPct %s below SLPct %s — the operator's explicit floor",
						kind, i, sig.TPPct, sig.SLPct)
				}
			}
		})
	}
}

// TestV2_TargetsAreReachable is the practical form of the same requirement, in the units the
// operator actually reads: at the deployed 10x leverage, a target 60-70% of margin away is one
// price does not reach on a 5m bar. Production's V1 mean was 29%-36% with individual orders at 65%.
func TestV2_TargetsAreReachable(t *testing.T) {
	cs := loadRealCandles(t, "SOL")
	const leverage = 10
	for _, kind := range v2Kinds() {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			sigs := driveV2(t, Factories[kind](), cs, 250)
			for i, sig := range sigs {
				marginPct := sig.TPPct.Mul(decimal.NewFromInt(100)).Mul(decimal.NewFromInt(leverage))
				if marginPct.GreaterThan(decimal.NewFromInt(30)) {
					t.Fatalf("%s signal %d: target is %s%% of margin at %dx — unreachable on a 5m bar",
						kind, i, marginPct.StringFixed(1), leverage)
				}
			}
		})
	}
}

// TestV2_WithParamsDoesNotCarryState enforces the contract a shallow `cp := *s` silently breaks:
// a warmed-up instance must not hand its accumulated evaluation state to a differently-configured
// copy. CLAUDE.md §16.8 found this in 7 of 14 strategies, including one where two variants shared
// a slice and overwrote each other.
//
// The assertion is checked for non-vacuity: if neither copy ever fires, the comparison is
// meaningless and the test says so rather than passing.
func TestV2_WithParamsDoesNotCarryState(t *testing.T) {
	cs := loadRealCandles(t, "SOL")
	for _, kind := range v2Kinds() {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			warmed := Factories[kind]()
			for i := 250; i < 500; i++ {
				if _, err := warmed.Evaluate(cs[:i]); err != nil {
					t.Fatalf("warm-up: %v", err)
				}
			}
			params := map[string]decimal.Decimal{"risk_reward": decimal.NewFromFloat(2.5)}
			fromWarmed := warmed.WithParams(params)
			fromFresh := Factories[kind]().WithParams(params)

			a := driveV2(t, fromWarmed, cs, 250)
			b := driveV2(t, fromFresh, cs, 250)

			if len(a) == 0 && len(b) == 0 {
				t.Fatalf("%s: neither copy fired — comparison is vacuous", kind)
			}
			if len(a) != len(b) {
				t.Fatalf("%s: copy from warmed instance produced %d signals, copy from fresh produced %d — state leaked",
					kind, len(a), len(b))
			}
			for i := range a {
				if !a[i].EntryPx.Equal(b[i].EntryPx) || a[i].Side != b[i].Side || !a[i].SLPx.Equal(b[i].SLPx) {
					t.Fatalf("%s: signal %d differs between warmed-copy and fresh-copy — state leaked", kind, i)
				}
			}
		})
	}
}
