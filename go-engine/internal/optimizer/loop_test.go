package optimizer

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/backtest"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/postgres"
)

// fakeSidecar is a scriptable SuggestReporter — no real optimizer-service process needed.
type fakeSidecar struct {
	suggestResult []SuggestedCandidate
	suggestErr    error
	reported      []reportedScore
}

type reportedScore struct {
	studyID string
	trialID int
	score   float64
}

func (f *fakeSidecar) Suggest(ctx context.Context, studyID string, specs []SidecarParamSpec, n int) ([]SuggestedCandidate, error) {
	return f.suggestResult, f.suggestErr
}

func (f *fakeSidecar) Report(ctx context.Context, studyID string, trialID int, score float64) error {
	f.reported = append(f.reported, reportedScore{studyID, trialID, score})
	return nil
}

// fakeBacktest returns a scripted backtest.Result regardless of the Config it's given, and
// records every call so a test can assert what was actually asked for.
type fakeBacktest struct {
	result backtest.Result
	err    error
	calls  []backtest.Config
}

func (f *fakeBacktest) RunBacktest(ctx context.Context, cfg backtest.Config) (backtest.Result, error) {
	f.calls = append(f.calls, cfg)
	return f.result, f.err
}

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping Postgres-backed optimizer tests")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewStore(pool)
}

// testRepo builds a real *postgres.Repository against the same test database, for auto-promote
// tests — Promote() needs port.Repository to create a strategy/assignment, and a fake here would
// hide exactly the kind of nil-Repo crash this project already shipped once (Loop.Repo was left
// unset in every pre-existing test, so the auto-promote path added 2026-09-28 was never actually
// exercised until this caught it panicking on findOriginStrategyID's nil repo.ListStrategies).
func testRepo(t *testing.T) port.Repository {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set — skipping Postgres-backed optimizer tests")
	}
	repo, err := postgres.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect repo to test database: %v", err)
	}
	t.Cleanup(repo.Close)
	return repo
}

// seedOrigin creates the locked origin strategy row Promote()'s findOriginStrategyID needs for
// kind — without it, promotion of an otherwise-valid candidate fails with "no origin strategy row
// for kind ... — has strategy.SeedOrigins run?", which is correct behavior in production (that
// seeding always runs at every service's own startup) but not something these tests do implicitly.
func seedOrigin(t *testing.T, repo port.Repository, kind string) int64 {
	t.Helper()
	id, err := repo.CreateStrategy(context.Background(), port.StrategyConfig{
		Name: kind, Kind: kind, Config: []byte("{}"), Enabled: true, IsOrigin: true,
	})
	if err != nil {
		t.Fatalf("seed origin strategy for kind %q: %v", kind, err)
	}
	return id
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testLineage() Lineage {
	return Lineage{Kind: "rsi_sma", InstID: "BTC", Bar: "5m", Exchange: "okx", RiskProfile: "low"}
}

// TestLoop_ProposeAndPass runs a full propose -> backtest -> validate -> pass cycle with a
// scripted good result, asserting the candidate ends up 'backtest_passed' and the sidecar
// received a Report call closing the ask/tell loop.
func TestLoop_ProposeAndPass(t *testing.T) {
	store := testStore(t)
	repo := testRepo(t)
	ctx := context.Background()
	lin := testLineage()
	seedOrigin(t, repo, "rsi_sma")

	sidecar := &fakeSidecar{suggestResult: []SuggestedCandidate{{TrialID: 42, Params: map[string]decimal.Decimal{}}}}
	bt := &fakeBacktest{result: goodResult("rsi_sma")}

	loop := &Loop{Store: store, Sidecar: sidecar, Backtest: bt, Repo: repo, Logger: testLogger()}

	if err := loop.Tick(ctx, lin, testParams(), testValidationConfig()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The in-flight slot must be cleared after a completed judgement — otherwise the lineage would
	// be stuck "backtesting" forever.
	inFlight, err := store.GetOptimizerState(ctx, lin)
	if err != nil {
		t.Fatalf("GetOptimizerState: %v", err)
	}
	if inFlight != nil {
		t.Fatalf("expected in-flight slot cleared, got candidate %d", *inFlight)
	}

	if len(sidecar.reported) != 1 || sidecar.reported[0].trialID != 42 {
		t.Fatalf("expected exactly one Report call for trial 42, got %+v", sidecar.reported)
	}

	// The candidate is auto-promoted the instant it passes (2026-09-28), so its final status is
	// paper_active, not backtest_passed — RecordBacktestResult sets backtest_passed first, but
	// runAndRecord's own auto-promote call immediately advances it further within the same Tick.
	candidates, err := store.ListCandidatesForLineage(ctx, lin)
	if err != nil {
		t.Fatalf("ListCandidatesForLineage: %v", err)
	}
	var found bool
	for _, c := range candidates {
		if c.Status == "paper_active" && c.TrialID != nil && *c.TrialID == 42 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a paper_active candidate for trial 42, got %+v", candidates)
	}

	// The candidate that just passed must be auto-promoted (2026-09-28: promotion used to require
	// a manual click on every one of thousands of lineages, which left real passing candidates —
	// including a 65-trade/53.8%-win-rate one — sitting unused with nobody ever seeing them).
	active, hasActive, err := store.ActiveCandidate(ctx, lin)
	if err != nil {
		t.Fatalf("ActiveCandidate: %v", err)
	}
	if !hasActive {
		t.Fatal("expected the passing candidate to be auto-promoted to paper_active, got none active")
	}
	if active.TrialID == nil || *active.TrialID != 42 {
		t.Fatalf("expected trial 42's candidate to be the active one, got %+v", active)
	}
}

// TestLoop_SweepUnpromoted_SkipsOriginPlaceholder is a regression test for a real bug found while
// deploying auto-promotion: EnsureOriginCandidate stamps every lineage's placeholder row with
// status='backtest_passed' and an empty {} config directly (it's created before Optuna ever
// proposes anything real), and the FIRST version of SweepUnpromoted did not filter it out — for
// any lineage with no genuinely-passing candidate, it "promoted" that empty placeholder instead,
// which pointlessly re-cloned the origin and re-touched that lineage's strategy_assignments right
// after the same startup pass had just disabled the origin's own assignment, undoing the cleanup.
func TestLoop_SweepUnpromoted_SkipsOriginPlaceholder(t *testing.T) {
	store := testStore(t)
	repo := testRepo(t)
	ctx := context.Background()
	lin := Lineage{Kind: "rsi_sma", InstID: "SWEEPTEST", Bar: "5m", Exchange: "okx", RiskProfile: "low"}
	seedOrigin(t, repo, "rsi_sma")

	// Simulate exactly what EnsureOriginCandidate does on its own, with no real candidate ever
	// having passed for this lineage — the state every lineage starts in before Optuna proposes
	// anything, and the state a lineage with nothing genuinely tunable stays in forever.
	if _, err := store.EnsureOriginCandidate(ctx, lin); err != nil {
		t.Fatalf("EnsureOriginCandidate: %v", err)
	}

	loop := &Loop{Store: store, Repo: repo, Logger: testLogger()}
	promoted, err := loop.SweepUnpromoted(ctx, func(string) int { return 10 })
	if err != nil {
		t.Fatalf("SweepUnpromoted: %v", err)
	}
	if promoted != 0 {
		t.Fatalf("expected the sweep to promote 0 candidates (only an origin placeholder exists), got %d", promoted)
	}

	if _, hasActive, err := store.ActiveCandidate(ctx, lin); err != nil {
		t.Fatalf("ActiveCandidate: %v", err)
	} else if hasActive {
		t.Fatal("expected no active candidate for a lineage with only an origin placeholder")
	}
}

// TestLoop_ProposeAndReject mirrors the pass case with a bad result, asserting the candidate is
// rejected AND the in-flight slot is still cleared (so the next tick proposes fresh rather than
// getting stuck on a rejected candidate).
func TestLoop_ProposeAndReject(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	lin := testLineage()

	sidecar := &fakeSidecar{suggestResult: []SuggestedCandidate{{TrialID: 7, Params: map[string]decimal.Decimal{}}}}
	bt := &fakeBacktest{result: badResult("rsi_sma")}

	loop := &Loop{Store: store, Sidecar: sidecar, Backtest: bt, Logger: testLogger()}

	if err := loop.Tick(ctx, lin, testParams(), testValidationConfig()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	inFlight, err := store.GetOptimizerState(ctx, lin)
	if err != nil {
		t.Fatalf("GetOptimizerState: %v", err)
	}
	if inFlight != nil {
		t.Fatalf("expected in-flight slot cleared after rejection, got candidate %d", *inFlight)
	}

	candidates, err := store.ListCandidatesForLineage(ctx, lin)
	if err != nil {
		t.Fatalf("ListCandidatesForLineage: %v", err)
	}
	var found bool
	for _, c := range candidates {
		if c.Status == "backtest_rejected" && c.TrialID != nil && *c.TrialID == 7 {
			if c.BacktestRejectReason == nil || *c.BacktestRejectReason == "" {
				t.Fatal("rejected candidate should carry a non-empty reject reason")
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a backtest_rejected candidate for trial 7, got %+v", candidates)
	}
}

// TestLoop_ProposeIncrementsGenerationAcrossRejectedAttempts is the regression test for the
// 2026-09-28 generation-numbering fix: every candidate proposeCandidate creates for one lineage
// used to land on origin.Generation+1 (always 2) no matter how many were tried before, which is
// what forced DisplayName's id-suffix workaround in the previous session. Runs three full
// propose->reject cycles for the SAME lineage and asserts each one's generation is strictly higher
// than the last — proving the number comes from the lineage's own history, not a constant.
func TestLoop_ProposeIncrementsGenerationAcrossRejectedAttempts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	// InstID includes the current time so a re-run against a database that still holds a previous
	// run's rows for this exact test (this package's tests share one database with no per-test
	// rollback, unlike a typical isolated-transaction test setup) starts a genuinely fresh lineage
	// instead of silently accumulating trial-id collisions across runs — found exactly this way
	// while verifying this test: a second run's generations [5 6 7] got appended onto a first run's
	// leftover [2 3 4] because both used the same hardcoded trial ids against the same lineage.
	lin := Lineage{Kind: "rsi_sma", InstID: fmt.Sprintf("GENTEST%d", time.Now().UnixNano()), Bar: "5m", Exchange: "okx", RiskProfile: "low"}

	var generations []int
	for i, trialID := range []int{101, 102, 103} {
		sidecar := &fakeSidecar{suggestResult: []SuggestedCandidate{{TrialID: trialID, Params: map[string]decimal.Decimal{}}}}
		bt := &fakeBacktest{result: badResult("rsi_sma")} // reject every time, so nothing gets promoted/removed
		loop := &Loop{Store: store, Sidecar: sidecar, Backtest: bt, Logger: testLogger()}

		if err := loop.Tick(ctx, lin, testParams(), testValidationConfig()); err != nil {
			t.Fatalf("Tick #%d: %v", i, err)
		}

		candidates, err := store.ListCandidatesForLineage(ctx, lin)
		if err != nil {
			t.Fatalf("ListCandidatesForLineage after tick #%d: %v", i, err)
		}
		var found bool
		for _, c := range candidates {
			if c.TrialID != nil && *c.TrialID == trialID {
				generations = append(generations, c.Generation)
				found = true
			}
		}
		if !found {
			t.Fatalf("tick #%d: no candidate found for trial %d", i, trialID)
		}
	}

	if len(generations) != 3 {
		t.Fatalf("expected 3 recorded generations, got %v", generations)
	}
	for i := 1; i < len(generations); i++ {
		if generations[i] <= generations[i-1] {
			t.Fatalf("generation did not strictly increase across proposals: %v (bug: every candidate landed on origin.Generation+1 regardless of history)", generations)
		}
	}
}

// TestStore_MaxGeneration_NoCandidatesYetReturnsOne asserts the baseline case: a lineage with only
// an origin placeholder (never a real candidate proposed) reports generation 1, matching the
// origin row's own generation — so the FIRST real proposal correctly lands on generation 2.
func TestStore_MaxGeneration_NoCandidatesYetReturnsOne(t *testing.T) {
	store := testStore(t)
	repo := testRepo(t)
	ctx := context.Background()
	lin := Lineage{Kind: "rsi_sma", InstID: "MAXGENTEST1", Bar: "5m", Exchange: "okx", RiskProfile: "low"}
	seedOrigin(t, repo, "rsi_sma")

	if _, err := store.EnsureOriginCandidate(ctx, lin); err != nil {
		t.Fatalf("EnsureOriginCandidate: %v", err)
	}

	gen, err := store.MaxGeneration(ctx, lin)
	if err != nil {
		t.Fatalf("MaxGeneration: %v", err)
	}
	if gen != 1 {
		t.Fatalf("MaxGeneration for a lineage with only an origin placeholder: want 1, got %d", gen)
	}
}

// TestStore_MaxGeneration_ReflectsTheHighestCandidateEverCreated asserts MaxGeneration is a real
// MAX() over the lineage's history, not just the most recently inserted row's generation — the
// distinction matters if an older row is ever re-touched (e.g. a manual edit bumping updated_at)
// without changing id ordering.
func TestStore_MaxGeneration_ReflectsTheHighestCandidateEverCreated(t *testing.T) {
	store := testStore(t)
	repo := testRepo(t)
	ctx := context.Background()
	lin := Lineage{Kind: "rsi_sma", InstID: "MAXGENTEST2", Bar: "5m", Exchange: "okx", RiskProfile: "low"}
	seedOrigin(t, repo, "rsi_sma")

	originID, err := store.EnsureOriginCandidate(ctx, lin)
	if err != nil {
		t.Fatalf("EnsureOriginCandidate: %v", err)
	}
	for _, gen := range []int{2, 5, 3} { // deliberately out of order — MAX must not assume monotonic insertion
		if _, err := store.CreateCandidate(ctx, lin, []byte("{}"), originID, gen, 0, 0, "optimizer", nil); err != nil {
			t.Fatalf("CreateCandidate (generation %d): %v", gen, err)
		}
	}

	got, err := store.MaxGeneration(ctx, lin)
	if err != nil {
		t.Fatalf("MaxGeneration: %v", err)
	}
	if got != 5 {
		t.Fatalf("MaxGeneration: want 5 (the highest ever created, inserted out of order), got %d", got)
	}
}

// TestLoop_DoesNotReplaceActiveCandidateWithTooFewLiveTrades is the regression test for the
// 2026-09-28 operator decision: a new candidate passing its backtest must NOT replace a lineage's
// currently-active candidate until that active candidate has accumulated at least
// MinLiveTradesBeforeReplace real closed live trades of its own. Promotes a first candidate
// (0 live trades, matching a freshly-promoted strategy), then runs a second passing candidate
// through the same lineage and asserts the FIRST one is still the active one — a backtest pass
// alone must not be enough.
func TestLoop_DoesNotReplaceActiveCandidateWithTooFewLiveTrades(t *testing.T) {
	store := testStore(t)
	repo := testRepo(t)
	ctx := context.Background()
	const kind = "adx_dmi_quality"
	lin := Lineage{Kind: kind, InstID: "GATETEST1", Bar: "5m", Exchange: "okx", RiskProfile: "low"}
	seedOrigin(t, repo, kind)

	// First candidate: promote it directly (bypassing Tick, matching promote_test.go's own
	// pattern) so it becomes the lineage's active candidate with zero live trades.
	originID, err := store.EnsureOriginCandidate(ctx, lin)
	if err != nil {
		t.Fatalf("EnsureOriginCandidate: %v", err)
	}
	firstID, err := store.CreateCandidate(ctx, lin, []byte(`{}`), originID, 2, 0, 0, "optimizer", nil)
	if err != nil {
		t.Fatalf("create first candidate: %v", err)
	}
	now := time.Now()
	if err := store.RecordBacktestResult(ctx, firstID, true, "", 50, decimal.NewFromInt(60), decimal.NewFromInt(10), 0, nil, now.Add(-30*24*time.Hour), now); err != nil {
		t.Fatalf("record backtest result for first candidate: %v", err)
	}
	if _, err := Promote(ctx, store, repo, firstID, 10); err != nil {
		t.Fatalf("promote first candidate: %v", err)
	}

	// Second candidate goes through the real Tick path with a passing backtest — a genuinely
	// better result than the first (higher win rate/PnL), which under the OLD behavior would have
	// replaced the active candidate immediately.
	sidecar := &fakeSidecar{suggestResult: []SuggestedCandidate{{TrialID: 555, Params: map[string]decimal.Decimal{}}}}
	bt := &fakeBacktest{result: goodResult(kind)}
	loop := &Loop{Store: store, Sidecar: sidecar, Backtest: bt, Repo: repo, Logger: testLogger()}
	if err := loop.Tick(ctx, lin, testParams(), testValidationConfig()); err != nil {
		t.Fatalf("Tick for second candidate: %v", err)
	}

	active, hasActive, err := store.ActiveCandidate(ctx, lin)
	if err != nil {
		t.Fatalf("ActiveCandidate: %v", err)
	}
	if !hasActive {
		t.Fatal("expected an active candidate")
	}
	if active.ID != firstID {
		t.Fatalf("expected the FIRST candidate (id %d) to remain active since it has 0 live trades, got candidate %d active instead — a passing backtest alone replaced it", firstID, active.ID)
	}

	// The second candidate must still be visible as backtest_passed, not silently discarded —
	// operator can promote it manually, or it stays eligible once the active one earns enough live
	// trades.
	second, err := store.GetCandidate(ctx, sidecarTrialCandidateID(t, store, ctx, lin, 555))
	if err != nil {
		t.Fatalf("get second candidate: %v", err)
	}
	if second.Status != "backtest_passed" {
		t.Fatalf("expected the second (blocked) candidate to stay backtest_passed, got %q", second.Status)
	}
}

// TestLoop_ReplacesActiveCandidateOnceItHasEnoughLiveTrades confirms the gate is not permanent —
// once the active candidate has accumulated MinLiveTradesBeforeReplace closed live trades, a
// passing new candidate DOES replace it, matching the operator's own "wait for real data, then
// compare and replace" instruction.
func TestLoop_ReplacesActiveCandidateOnceItHasEnoughLiveTrades(t *testing.T) {
	store := testStore(t)
	repo := testRepo(t)
	ctx := context.Background()
	const kind = "double_top_bottom"
	lin := Lineage{Kind: kind, InstID: "GATETEST2", Bar: "5m", Exchange: "okx", RiskProfile: "low"}
	seedOrigin(t, repo, kind)

	originID, err := store.EnsureOriginCandidate(ctx, lin)
	if err != nil {
		t.Fatalf("EnsureOriginCandidate: %v", err)
	}
	firstID, err := store.CreateCandidate(ctx, lin, []byte(`{}`), originID, 2, 0, 0, "optimizer", nil)
	if err != nil {
		t.Fatalf("create first candidate: %v", err)
	}
	now := time.Now()
	if err := store.RecordBacktestResult(ctx, firstID, true, "", 50, decimal.NewFromInt(60), decimal.NewFromInt(10), 0, nil, now.Add(-30*24*time.Hour), now); err != nil {
		t.Fatalf("record backtest result for first candidate: %v", err)
	}
	firstStrategyID, err := Promote(ctx, store, repo, firstID, 10)
	if err != nil {
		t.Fatalf("promote first candidate: %v", err)
	}

	// Give the first (active) candidate's strategy exactly the configured minimum of real closed
	// live paper trades — testValidationConfig's own MinLiveTradesBeforeReplace, set below.
	vc := testValidationConfigWithLiveGate(30)
	if err := store.SaveValidationConfig(ctx, vc); err != nil {
		t.Fatalf("save validation config: %v", err)
	}
	for i := 0; i < 30; i++ {
		seedClosedLiveTrade(t, repo, firstStrategyID, lin.InstID, lin.Exchange)
	}

	sidecar := &fakeSidecar{suggestResult: []SuggestedCandidate{{TrialID: 556, Params: map[string]decimal.Decimal{}}}}
	bt := &fakeBacktest{result: goodResult(kind)}
	loop := &Loop{Store: store, Sidecar: sidecar, Backtest: bt, Repo: repo, Logger: testLogger()}
	if err := loop.Tick(ctx, lin, testParams(), vc); err != nil {
		t.Fatalf("Tick for second candidate: %v", err)
	}

	active, hasActive, err := store.ActiveCandidate(ctx, lin)
	if err != nil {
		t.Fatalf("ActiveCandidate: %v", err)
	}
	if !hasActive {
		t.Fatal("expected an active candidate")
	}
	if active.ID == firstID {
		t.Fatalf("expected the SECOND candidate to replace the first now that it has %d live trades (>= the configured minimum), but the first candidate (id %d) is still active", 30, firstID)
	}
	if active.TrialID == nil || *active.TrialID != 556 {
		t.Fatalf("expected trial 556's candidate to be the new active one, got %+v", active)
	}
}

// sidecarTrialCandidateID finds the candidate id matching trialID for lin — a small test helper
// since ListCandidatesForLineage is the only lookup that can find a specific trial's candidate.
func sidecarTrialCandidateID(t *testing.T, store *Store, ctx context.Context, lin Lineage, trialID int) int64 {
	t.Helper()
	candidates, err := store.ListCandidatesForLineage(ctx, lin)
	if err != nil {
		t.Fatalf("ListCandidatesForLineage: %v", err)
	}
	for _, c := range candidates {
		if c.TrialID != nil && *c.TrialID == trialID {
			return c.ID
		}
	}
	t.Fatalf("no candidate found for trial %d", trialID)
	return 0
}

// testValidationConfigWithLiveGate mirrors testValidationConfig but with an explicit
// MinLiveTradesBeforeReplace, since the package default test config leaves it at zero.
func testValidationConfigWithLiveGate(minLiveTrades int) ValidationConfig {
	vc := testValidationConfig()
	vc.MinLiveTradesBeforeReplace = minLiveTrades
	return vc
}

// seedClosedLiveTrade inserts one closed, profitable, baseline paper order for strategyID —
// exactly what StrategyStatsFor counts toward a strategy's live trade total.
func seedClosedLiveTrade(t *testing.T, repo port.Repository, strategyID int64, instID, exchange string) {
	t.Helper()
	id, err := repo.OpenPaperOrder(context.Background(), port.PaperOrder{
		InstID: instID, Exchange: exchange, StrategyID: &strategyID, Side: "buy",
		EntryPx: decimal.NewFromInt(100), Size: decimal.NewFromInt(10), Leverage: decimal.NewFromInt(1),
		Mode: "paper", Variant: "baseline",
	})
	if err != nil {
		t.Fatalf("seed closed live trade (open): %v", err)
	}
	if err := repo.ClosePaperOrder(context.Background(), id, decimal.NewFromInt(101), "tp", decimal.NewFromInt(1), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("seed closed live trade (close): %v", err)
	}
}

// TestLoop_BacktestConfigCarriesBaselineAndLineage asserts the loop asks internal/backtest for
// EXACTLY the lineage's own instrument/bar/exchange plus the coin_flip baseline — a mutation here
// (e.g. forgetting BaselineKind, or leaking another instrument in) would silently disable the
// statistical-significance check or contaminate one lineage's backtest with another's data.
func TestLoop_BacktestConfigCarriesBaselineAndLineage(t *testing.T) {
	store := testStore(t)
	repo := testRepo(t)
	ctx := context.Background()
	lin := Lineage{Kind: "pmax", InstID: "ETH", Bar: "15m", Exchange: "mexc", RiskProfile: "high"}
	seedOrigin(t, repo, "pmax")

	sidecar := &fakeSidecar{suggestResult: []SuggestedCandidate{{TrialID: 1, Params: map[string]decimal.Decimal{}}}}
	bt := &fakeBacktest{result: goodResult("pmax")}

	loop := &Loop{Store: store, Sidecar: sidecar, Backtest: bt, Repo: repo, Logger: testLogger()}
	if err := loop.Tick(ctx, lin, testParams(), testValidationConfig()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(bt.calls) != 1 {
		t.Fatalf("expected exactly one backtest call, got %d", len(bt.calls))
	}
	cfg := bt.calls[0]
	if cfg.Exchange != "mexc" {
		t.Errorf("Exchange = %q, want mexc", cfg.Exchange)
	}
	if len(cfg.InstIDs) != 1 || cfg.InstIDs[0] != "ETH" {
		t.Errorf("InstIDs = %v, want [ETH]", cfg.InstIDs)
	}
	if len(cfg.Bars) != 1 || cfg.Bars[0] != "15m" {
		t.Errorf("Bars = %v, want [15m]", cfg.Bars)
	}
	wantKinds := map[string]bool{backtest.BaselineKind: true, "pmax": true}
	if len(cfg.Kinds) != 2 || !wantKinds[cfg.Kinds[0]] || !wantKinds[cfg.Kinds[1]] {
		t.Errorf("Kinds = %v, want [coin_flip pmax] in some order", cfg.Kinds)
	}
}

func testParams() BacktestParams {
	return BacktestParams{
		InitialUSD:    decimal.NewFromInt(40),
		MaxLeverage:   decimal.NewFromInt(10),
		PositionSlots: 1,
		CandleWindow:  100,
		Lookback:      30 * 24 * time.Hour,
	}
}

func testValidationConfig() ValidationConfig {
	return ValidationConfig{
		RiskProfile:      "low",
		MinTrades:        30,
		MinWinRatePct:    decimal.NewFromInt(45),
		MinRealizedPnL:   decimal.NewFromInt(0),
		MinSignificanceT: decimal.NewFromInt(2),
		MaxResets:        2,
	}
}

func goodResult(kind string) backtest.Result {
	return backtest.Result{
		ByStrategy: map[string]*backtest.KindStats{
			kind: {Kind: kind, Trades: 50, Wins: 30, WinRate: 0.6, PnLUSD: 12.5, PnLPerTrade: 0.25},
		},
		Significance: []backtest.Significance{{Kind: kind, T: 2.5, Significant: true}},
	}
}

func badResult(kind string) backtest.Result {
	return backtest.Result{
		ByStrategy: map[string]*backtest.KindStats{
			kind: {Kind: kind, Trades: 5, Wins: 1, WinRate: 0.2, PnLUSD: -5},
		},
	}
}
