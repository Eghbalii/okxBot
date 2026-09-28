package optimizer

import (
	"context"
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
