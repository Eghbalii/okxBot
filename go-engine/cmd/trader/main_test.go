package main

import (
	"os"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/config"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// TestBuildRealTraderClamps_MapsMaxLossPct guards against the exact class of bug CLAUDE.md §23
// documents for cmd/paper-trader's own buildRLClamps: a struct literal silently dropping a field
// (there, MaxLossPct — §19.2's leverage-aware 15%-loss cap never actually applied to a real order).
// buildRealTraderClamps is a separate copy of that mapping for RealTrader, so it needs its own
// regression test rather than assuming the paper-trader fix covers it.
func TestBuildRealTraderClamps_MapsMaxLossPct(t *testing.T) {
	cfg := &config.Config{}
	cfg.PaperTrading.RLClamps.MinSLDistPct = decimal.NewFromFloat(0.005)
	cfg.PaperTrading.RLClamps.MaxSLDistPct = decimal.NewFromFloat(0.05)
	cfg.PaperTrading.RLClamps.MaxLossPct = decimal.NewFromFloat(0.15)
	cfg.PaperTrading.RLClamps.MinTPSLRatio = decimal.NewFromFloat(1.5)

	got := buildRealTraderClamps(cfg)

	if !got.MinSLDistPct.Equal(cfg.PaperTrading.RLClamps.MinSLDistPct) {
		t.Errorf("MinSLDistPct not mapped: got %s", got.MinSLDistPct)
	}
	if !got.MaxSLDistPct.Equal(cfg.PaperTrading.RLClamps.MaxSLDistPct) {
		t.Errorf("MaxSLDistPct not mapped: got %s", got.MaxSLDistPct)
	}
	if !got.MaxLossPct.Equal(cfg.PaperTrading.RLClamps.MaxLossPct) {
		t.Errorf("MaxLossPct not mapped (this is the exact field class that caused CLAUDE.md §23's incident): got %s, want %s", got.MaxLossPct, cfg.PaperTrading.RLClamps.MaxLossPct)
	}
	if !got.MinTPSLRatio.Equal(cfg.PaperTrading.RLClamps.MinTPSLRatio) {
		t.Errorf("MinTPSLRatio not mapped: got %s", got.MinTPSLRatio)
	}
}

// TestBuildRealTraderClamps_ProductionScenarioIsNowCaught reproduces CLAUDE.md §23's order-636
// scenario (20x leverage, a naive 5%-distance stop — inside MaxSLDistPct alone) through
// buildRealTraderClamps + the real conductor.Clamps.Apply pipeline, confirming the resulting stop
// respects MaxLossPct rather than the leverage-blind raw distance.
func TestBuildRealTraderClamps_ProductionScenarioIsNowCaught(t *testing.T) {
	cfg := &config.Config{}
	cfg.PaperTrading.RLClamps.MinSLDistPct = decimal.NewFromFloat(0.005)
	cfg.PaperTrading.RLClamps.MaxSLDistPct = decimal.NewFromFloat(0.05)
	cfg.PaperTrading.RLClamps.MaxLossPct = decimal.NewFromFloat(0.15)
	clamps := buildRealTraderClamps(cfg)

	entry := decimal.NewFromFloat(0.004533)
	leverage := decimal.NewFromInt(20)
	naiveSL := decimal.NewFromFloat(0.00430635) // a 5%-distance stop, uncapped

	out := clamps.Apply("buy", entry, leverage, conductor.Levels{SLPx: &naiveSL})
	if out.SLPx == nil {
		t.Fatal("expected a stop to survive Apply")
	}

	actualLossPct := entry.Sub(*out.SLPx).Div(entry).Mul(leverage)
	maxAllowed := decimal.NewFromFloat(0.15)
	if actualLossPct.GreaterThan(maxAllowed) {
		t.Errorf("stop %s realizes %.4f%% loss at %sx leverage, want <= %.0f%%",
			out.SLPx, actualLossPct.Mul(decimal.NewFromInt(100)).InexactFloat64(), leverage, maxAllowed.Mul(decimal.NewFromInt(100)).InexactFloat64())
	}
	if out.SLPx.Equal(naiveSL) {
		t.Error("stop was NOT tightened — still at the naive 5% distance, MaxLossPct had no effect")
	}
}

// TestUseConductorLifecycle_DefaultsOff confirms the config flag defaults to false — the flip must
// be explicit, matching CLAUDE.md §27's plan's "off-by-default safety valve" for the riskiest
// commit in the rollout.
func TestUseConductorLifecycle_DefaultsOff(t *testing.T) {
	cfg := &config.Config{}
	if cfg.Trading.UseConductorLifecycle {
		t.Error("expected UseConductorLifecycle to default to false")
	}
}

// Real trading's early-close switch must be its OWN, never paper_trading's: enabling early close
// for paper research must not silently enable it against real capital (2026-09-08 request). This
// asserts the two are genuinely independent in BOTH directions, since a mapping that reads the
// wrong field would still look right whenever the two flags happen to agree.
func TestRealEarlyCloseAllowed_IsIndependentOfPaperFlag(t *testing.T) {
	var cfg config.Config

	cfg.Trading.AllowRLEarlyClose = false
	cfg.PaperTrading.RLEarlyClose = true
	if realEarlyCloseAllowed(&cfg) {
		t.Error("paper's early-close flag must NOT enable early close against real capital")
	}

	cfg.Trading.AllowRLEarlyClose = true
	cfg.PaperTrading.RLEarlyClose = false
	if !realEarlyCloseAllowed(&cfg) {
		t.Error("real's own flag must enable early close regardless of paper's")
	}
}

// Off by default (Go's zero value), so a config that never mentions the key ignores the model's
// early-close action rather than acting on it against real money.
func TestRealEarlyCloseAllowed_DefaultsOff(t *testing.T) {
	if realEarlyCloseAllowed(&config.Config{}) {
		t.Error("allow_rl_early_close must default to false")
	}
}

// The restart handler's exit code and the compose restart policy have to agree, and getting that
// pair wrong has now broken real trading twice in different directions:
//
//	exit(0) + on-failure:5      -> a panel Resume left the container Exited(0) forever
//	exit(1) + on-failure:5      -> every panel restart burned a retry; six exhausted the cap and
//	                               the trader stayed dead with two real positions open and losing
//	exit(0) + unless-stopped    -> correct: an operator restart always relaunches, while the
//	                               startup guards still exit(1) and stop a genuine crash loop
//
// os.Exit cannot be exercised from a test, so this asserts the pairing at the source level. That
// is weaker than a behavioural test, but it is the value that actually regressed — twice — and a
// mismatch here is invisible until real money is already exposed.
func TestRestartHandlerExitCodeMatchesRestartPolicy(t *testing.T) {
	handler, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	if !strings.Contains(string(handler), "os.Exit(0)") {
		t.Error("handleRestart must exit 0: unless-stopped relaunches a clean exit, and a nonzero " +
			"exit would count toward any capped policy as if the operator's restart were a crash")
	}

	compose, err := os.ReadFile("../../../docker-compose.yml")
	if err != nil {
		t.Skipf("docker-compose.yml not readable from here: %v", err)
	}
	traderIdx := strings.Index(string(compose), "\n  trader:")
	if traderIdx < 0 {
		t.Fatal("could not locate the trader service in docker-compose.yml")
	}
	// Read only as far as the next service, so this cannot accidentally match a neighbour's policy.
	rest := string(compose)[traderIdx+1:]
	if next := strings.Index(rest[1:], "\n  "); next > 0 {
		if end := strings.Index(rest, "\n\n"); end > 0 {
			rest = rest[:end]
		}
	}
	if strings.Contains(rest, "restart: on-failure") {
		t.Error("trader must not use a capped restart policy: the panel's Restart button is a " +
			"process exit, so a cap counts operator actions as failures and eventually refuses " +
			"to bring real trading back up")
	}
	if !strings.Contains(rest, "restart: unless-stopped") {
		t.Error("trader should use restart: unless-stopped so an operator-requested restart always relaunches")
	}
}

// TestEnginesAreMarkedReconciledExternally guards a field whose absence is silent (2026-09-10):
// dropping ReconciledExternally from main()'s engine literal makes every engine start its OWN
// reconciliation loop again, alongside the shared driver — restoring exactly the per-token account
// polling that hit OKX's rate limit (CLAUDE.md §38.2), with no error and no behavioral difference
// except the call volume.
//
// Asserted against the source rather than a constructor because the engine literal lives inline in
// main(); this is the same class of guard as buildRealTraderClamps' own tests, which exist because a
// field silently dropped from a large struct literal is precisely what left every real position
// uncapped (§23).
func TestEnginesAreMarkedReconciledExternally(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(src), "ReconciledExternally: true") {
		t.Error("cmd/trader must set ReconciledExternally on every engine: without it each engine " +
			"restarts its own reconciliation poll and the shared driver's whole purpose is lost")
	}
	if !strings.Contains(string(src), "reconcileDriver.Run(ctx)") {
		t.Error("cmd/trader must run the shared ReconcileDriver: with ReconciledExternally set and " +
			"no driver running, nothing reconciles real positions at all")
	}
}
