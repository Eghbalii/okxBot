package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/backtest"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase/conductor"
)

// SuggestReporter is the sidecar surface the loop needs — narrowed to an interface (satisfied by
// *SidecarClient) so tests can fake it without a real optimizer-service process.
type SuggestReporter interface {
	Suggest(ctx context.Context, studyID string, specs []SidecarParamSpec, n int) ([]SuggestedCandidate, error)
	Report(ctx context.Context, studyID string, trialID int, score float64) error
}

// BacktestRunner is the internal/backtest surface the loop needs to validate one candidate — a
// narrow interface so this package never imports a concrete Postgres connection itself, and so
// tests can fake a backtest result without real candle history.
type BacktestRunner interface {
	RunBacktest(ctx context.Context, cfg backtest.Config) (backtest.Result, error)
}

// BacktestFunc adapts a plain function to BacktestRunner — the production wiring is
// `func(ctx, cfg) (backtest.Result, error) { return (&backtest.Runner{...}).Run(ctx) }`, since
// backtest.Runner itself carries no state between calls worth keeping across candidates.
type BacktestFunc func(ctx context.Context, cfg backtest.Config) (backtest.Result, error)

func (f BacktestFunc) RunBacktest(ctx context.Context, cfg backtest.Config) (backtest.Result, error) {
	return f(ctx, cfg)
}

// BacktestParams bundles what the loop needs to build a backtest.Config for one lineage, sourced
// from the lineage's RiskProfileConfig (leverage/clamps) — kept separate from Store/
// ValidationConfig since these are process-level wiring values (leverage ceiling, clamps), not
// per-candidate promotion thresholds.
type BacktestParams struct {
	InitialUSD     decimal.Decimal
	MaxLeverage    decimal.Decimal
	PositionSlots  int
	MaxPositionPct decimal.Decimal
	Clamps         conductor.Clamps
	CandleWindow   int
	Lookback       time.Duration
}

// Loop drives one full propose -> backtest -> validate -> promote cycle per Lineage. A single
// instance is shared across every configured lineage; Tick is called once per lineage per
// scheduler interval.
type Loop struct {
	Store    *Store
	Sidecar  SuggestReporter
	Backtest BacktestRunner
	Repo     port.Repository
	Logger   *slog.Logger

	// Now returns the current time — a field, not time.Now() directly, so tests can pin it.
	Now func() time.Time
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now().UTC()
}

// Tick runs one cycle for lineage l: if an in-flight backtest candidate exists, judge it;
// otherwise propose a new one from Optuna and start backtesting it. params carries the
// process-level backtest wiring (leverage, clamps, account size) for l's risk profile; cfg is
// l's own panel-editable promotion thresholds.
func (l *Loop) Tick(ctx context.Context, lin Lineage, params BacktestParams, cfg ValidationConfig) error {
	inFlight, err := l.Store.GetOptimizerState(ctx, lin)
	if err != nil {
		return fmt.Errorf("get optimizer state for %+v: %w", lin, err)
	}
	if inFlight != nil {
		return l.judgeCandidate(ctx, lin, *inFlight, params, cfg)
	}
	return l.proposeCandidate(ctx, lin, params)
}

// proposeCandidate asks the sidecar for one new candidate, immediately runs it through the
// backtest, records the result, and — if it fails validation — clears the in-flight slot so the
// NEXT tick proposes again rather than leaving a rejected candidate parked as "in flight" forever.
// A passing candidate is left in 'backtest_passed' for a separate promotion step to pick up
// (kept separate from this function so promoting is an explicit, auditable action rather than
// something that happens invisibly inside the same call that validated it).
func (l *Loop) proposeCandidate(ctx context.Context, lin Lineage, params BacktestParams) error {
	factory, ok := strategy.Factories[lin.Kind]
	if !ok {
		return fmt.Errorf("unknown strategy kind %q", lin.Kind)
	}
	specs := factory().Params()
	if len(specs) == 0 {
		// Nothing to tune — a pure crossover with no ParamSpecs. Nothing to propose, so this
		// lineage sits idle rather than erroring every tick.
		return nil
	}

	originID, err := l.Store.EnsureOriginCandidate(ctx, lin)
	if err != nil {
		return err
	}

	sidecarSpecs := make([]SidecarParamSpec, 0, len(specs))
	for _, s := range specs {
		min, _ := s.Min.Float64()
		max, _ := s.Max.Float64()
		sidecarSpecs = append(sidecarSpecs, SidecarParamSpec{Name: s.Name, Min: min, Max: max})
	}

	suggested, err := l.Sidecar.Suggest(ctx, StudyID(lin), sidecarSpecs, 1)
	if err != nil {
		return fmt.Errorf("suggest candidate for %+v: %w", lin, err)
	}
	if len(suggested) == 0 {
		return fmt.Errorf("sidecar returned no candidates for %+v", lin)
	}
	sc := suggested[0]

	configJSON, err := paramsToJSON(sc.Params)
	if err != nil {
		return fmt.Errorf("marshal candidate params: %w", err)
	}

	origin, err := l.Store.GetCandidate(ctx, originID)
	if err != nil {
		return err
	}
	candidateID, err := l.Store.CreateCandidate(ctx, lin, configJSON, originID, origin.Generation+1, 0, 0, "optimizer", &sc.TrialID)
	if err != nil {
		return err
	}
	if err := l.Store.SetOptimizerState(ctx, lin, &candidateID); err != nil {
		return err
	}

	return l.runAndRecord(ctx, lin, candidateID, sc.Params, params)
}

// judgeCandidate re-checks whether the currently in-flight candidate has already been backtested
// (RecordBacktestResult sets status away from 'proposed'/'backtesting' the moment a run
// completes, so a normal Tick never actually finds a candidate stuck mid-run — this only fires
// if a PREVIOUS tick's process died between SetOptimizerState and runAndRecord finishing, which
// judgeCandidate resolves by simply re-running the backtest, since the candidate itself is still
// only a proposed param set with no real-money side effect from re-running it).
func (l *Loop) judgeCandidate(ctx context.Context, lin Lineage, candidateID int64, params BacktestParams, cfg ValidationConfig) error {
	c, err := l.Store.GetCandidate(ctx, candidateID)
	if err != nil {
		return err
	}
	switch c.Status {
	case "backtest_passed":
		// Already judged and passed. Normally auto-promoted the moment runAndRecord judged it
		// (below), but this branch is reached when a PREVIOUS process died between recording the
		// result and promoting it — so promote here too, guarded by ActiveCandidate so a candidate
		// that's already paper_active (the common case) is never re-promoted pointlessly.
		if active, hasActive, err := l.Store.ActiveCandidate(ctx, lin); err != nil {
			return err
		} else if !hasActive || active.ID != c.ID {
			leverage := int(params.MaxLeverage.IntPart())
			if _, err := Promote(ctx, l.Store, l.Repo, c.ID, leverage); err != nil {
				l.Logger.Error("auto-promote failed", "candidateId", c.ID, "lineage", lin, "error", err)
			}
		}
		return l.Store.SetOptimizerState(ctx, lin, nil)
	case "backtest_rejected":
		// Already judged — clear the in-flight slot so the next tick proposes fresh rather than
		// re-judging the same candidate forever.
		return l.Store.SetOptimizerState(ctx, lin, nil)
	}

	var raw map[string]float64
	if err := json.Unmarshal(c.Config, &raw); err != nil {
		return fmt.Errorf("parse candidate %d config: %w", candidateID, err)
	}
	candidateParams := make(map[string]decimal.Decimal, len(raw))
	for k, v := range raw {
		candidateParams[k] = decimal.NewFromFloat(v)
	}
	return l.runAndRecord(ctx, lin, candidateID, candidateParams, params)
}

// runAndRecord replays candidateParams through the existing internal/backtest.Runner alongside
// backtest.BaselineKind (so significance is measured against the null strategy, per
// docs/RL_V8_PLAN.md's own "reading a raw ranking without this is one statistical cloud"
// finding), validates the result, records it, reports the score back to the sidecar (closing the
// ask/tell loop so Optuna learns from this trial), and clears the in-flight slot.
func (l *Loop) runAndRecord(ctx context.Context, lin Lineage, candidateID int64, candidateParams map[string]decimal.Decimal, params BacktestParams) error {
	if err := l.Store.SetBacktesting(ctx, candidateID); err != nil {
		return err
	}

	to := l.now()
	from := to.Add(-params.Lookback)

	cfg := backtest.Config{
		Exchange:       lin.Exchange,
		InstIDs:        []string{lin.InstID},
		Bars:           []string{lin.Bar},
		Kinds:          []string{backtest.BaselineKind, lin.Kind},
		From:           from,
		To:             to,
		InitialUSD:     params.InitialUSD,
		MaxLeverage:    params.MaxLeverage,
		PositionSlots:  params.PositionSlots,
		MaxPositionPct: params.MaxPositionPct,
		Clamps:         params.Clamps,
		CandleWindow:   params.CandleWindow,
		Params:         candidateParams,
	}

	result, err := l.Backtest.RunBacktest(ctx, cfg)
	if err != nil {
		return fmt.Errorf("run backtest for candidate %d (%+v): %w", candidateID, lin, err)
	}

	vc, verr := l.candidateValidationConfig(ctx, lin)
	if verr != nil {
		return verr
	}
	verdict := Validate(result, lin.Kind, vc)

	stats := result.ByStrategy[lin.Kind]
	tradeCount, winRate, pnl := 0, decimal.Zero, decimal.Zero
	if stats != nil {
		tradeCount = stats.Trades
		winRate = decimal.NewFromFloat(stats.WinRate * 100)
		pnl = decimal.NewFromFloat(stats.PnLUSD)
	}
	var sigT *decimal.Decimal
	if verdict.HasSignificance {
		v := verdict.SignificanceT
		sigT = &v
	}

	if err := l.Store.RecordBacktestResult(ctx, candidateID, verdict.Passed, verdict.Reason,
		tradeCount, winRate, pnl, result.Resets, sigT, from, to); err != nil {
		return err
	}

	// A candidate that clears every validation threshold goes live automatically — the pipeline's
	// whole purpose is "find something that actually works and trade it", and requiring a human to
	// click Promote on every one of the thousands of lineages this loop screens defeats that
	// purpose (2026-09-28, explicit operator instruction after finding a 65-trade/53.8%-win-rate/
	// +$30 candidate sitting unpromoted with nobody having ever seen it). Promote() itself already
	// handles "a different candidate for this lineage is currently paper_active" by demoting it in
	// the same transaction (§21's fix), so calling it here on every pass is safe to repeat.
	if verdict.Passed {
		leverage := int(params.MaxLeverage.IntPart())
		if _, err := Promote(ctx, l.Store, l.Repo, candidateID, leverage); err != nil {
			// Promotion failing (e.g. the kind's origin row hasn't been seeded yet) must not lose
			// the backtest result itself — it stays recorded as backtest_passed and the NEXT tick
			// that revisits this lineage will try to promote it again.
			l.Logger.Error("auto-promote failed", "candidateId", candidateID, "lineage", lin, "error", err)
		}
	}

	// Report the score Optuna should optimize toward: PnL per trade, the same metric
	// docs/RL_V8_PLAN.md's own screening ranks by, rather than win rate alone (a high win rate at
	// a poor reward:risk is a losing strategy — RL_V8_PLAN.md measured stoch_cross at 60% win with
	// negative PnL for exactly this reason).
	score := 0.0
	if stats != nil && stats.Trades > 0 {
		score = stats.PnLPerTrade
	}
	c, err := l.Store.GetCandidate(ctx, candidateID)
	if err != nil {
		return err
	}
	if c.TrialID != nil {
		if err := l.Sidecar.Report(ctx, StudyID(lin), *c.TrialID, score); err != nil {
			l.Logger.Warn("failed to report score to sidecar", "candidateId", candidateID, "error", err)
		}
	}

	return l.Store.SetOptimizerState(ctx, lin, nil)
}

// SweepUnpromoted promotes the BEST 'backtest_passed' candidate (by realized PnL — the same
// metric runAndRecord already reports to Optuna as its optimization score) for every lineage that
// has no currently 'paper_active' candidate — a one-time catch-up for candidates that passed
// validation BEFORE auto-promotion existed (2026-09-28) and were left sitting unpromoted with
// nobody ever seeing them (the exact bug that prompted auto-promotion: a 65-trade/53.8%-win-rate/
// +$30 candidate on TAO found completely unused). A lineage can have accumulated several
// backtest_passed candidates across different ticks before this existed — promoting whichever one
// happened to be scanned first, rather than the best one, would waste the sweep's one shot at
// picking well. leverageFor resolves a lineage's risk profile to its display leverage, mirroring
// the scheduler's own params.MaxLeverage. Runs once at process startup, before the scheduler's own
// tick loop begins — going forward, runAndRecord's own auto-promote keeps this list from ever
// growing again.
func (l *Loop) SweepUnpromoted(ctx context.Context, leverageFor func(riskProfile string) int) (promoted int, err error) {
	candidates, err := l.Store.ListCandidatesByStatus(ctx, "backtest_passed", 100000)
	if err != nil {
		return 0, fmt.Errorf("list backtest_passed candidates for sweep: %w", err)
	}

	best := make(map[Lineage]Candidate, len(candidates))
	for _, c := range candidates {
		// EnsureOriginCandidate stamps every lineage's placeholder row as status='backtest_passed'
		// directly (it's created before Optuna ever proposes anything real, so it has to start in
		// SOME status) with an empty {} config and never runs it through Validate — it is not a
		// real result and must never be promoted as one. Without this filter, a lineage that has
		// never produced a genuinely passing candidate gets its empty placeholder "promoted"
		// instead, which just re-creates a clone of the origin with no tuning at all — pointless,
		// and it does it by re-touching that lineage's strategy_assignments right after this same
		// startup pass already disabled the origin, undoing the cleanup for that lineage.
		if c.Source == "origin" {
			continue
		}
		cur, ok := best[c.Lineage]
		if !ok || betterCandidate(c, cur) {
			best[c.Lineage] = c
		}
	}

	for lin, c := range best {
		active, hasActive, err := l.Store.ActiveCandidate(ctx, lin)
		if err != nil {
			l.Logger.Error("sweep: failed to check active candidate", "lineage", lin, "error", err)
			continue
		}
		if hasActive {
			// Either this candidate is already the live one, or a DIFFERENT one is already
			// paper_active and presumably already trading — either way, nothing to promote here.
			// Only lineages with NO active candidate at all get swept.
			_ = active
			continue
		}
		if _, err := Promote(ctx, l.Store, l.Repo, c.ID, leverageFor(lin.RiskProfile)); err != nil {
			l.Logger.Error("sweep: promote failed", "candidateId", c.ID, "lineage", lin, "error", err)
			continue
		}
		promoted++
	}
	return promoted, nil
}

// betterCandidate ranks a by realized PnL, the same metric runAndRecord already optimizes toward
// via the sidecar score — a candidate with no recorded PnL sorts last.
func betterCandidate(a, b Candidate) bool {
	av, bv := decimal.Zero, decimal.Zero
	if a.BacktestRealizedPnL != nil {
		av = *a.BacktestRealizedPnL
	}
	if b.BacktestRealizedPnL != nil {
		bv = *b.BacktestRealizedPnL
	}
	return av.GreaterThan(bv)
}

func (l *Loop) candidateValidationConfig(ctx context.Context, lin Lineage) (ValidationConfig, error) {
	return l.Store.GetValidationConfig(ctx, lin.RiskProfile)
}

func paramsToJSON(params map[string]decimal.Decimal) (json.RawMessage, error) {
	raw := make(map[string]float64, len(params))
	for k, v := range params {
		f, _ := v.Float64()
		raw[k] = f
	}
	return json.Marshal(raw)
}
