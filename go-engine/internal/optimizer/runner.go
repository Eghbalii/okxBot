package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/eghbalii/okxBot/go-engine/internal/port"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
	"github.com/eghbalii/okxBot/go-engine/internal/usecase"
)

// SuggestReporter is the sidecar HTTP surface a Run needs — narrowed to an interface (satisfied
// by *SidecarClient) so tests can fake it without a real optimizer-service process.
type SuggestReporter interface {
	Suggest(ctx context.Context, studyID string, specs []SidecarParamSpec, n int) ([]Candidate, error)
	Report(ctx context.Context, studyID string, trialID int, score float64) error
}

// RunStatus is a run's current progress, served by GET /status (CLAUDE.md §16 decision 3).
type RunStatus struct {
	RunID           string    `json:"runId"`
	InstID          string    `json:"instId"`
	Kind            string    `json:"kind"`
	Bar             string    `json:"bar"`
	StartedAt       time.Time `json:"startedAt"`
	Deadline        time.Time `json:"deadline"`
	Done            bool      `json:"done"`
	CandidateCount  int       `json:"candidateCount"`
	TrialsCompleted int       `json:"trialsCompleted"`
	BestTrialID     int       `json:"bestTrialId,omitempty"`
	BestWinRatePct  string    `json:"bestWinRatePct,omitempty"`
	Persisted       bool      `json:"persisted"`
	PersistedStrat  int64     `json:"persistedStrategyId,omitempty"`
	Outcome         string    `json:"outcome,omitempty"` // human-readable summary once Done
}

// RunConfig bundles the tunables a Run needs (CLAUDE.md §16 decision 3's config surface).
type RunConfig struct {
	RunDuration           time.Duration
	MinTradesPerCandidate int
	MinImprovementPct     decimal.Decimal
	MinWinRatePctFloor    decimal.Decimal
	Bar                   string
	CandleWindow          int
	BatchSize             int
	TrialTTLBuffer        time.Duration
	// MaxLossPct caps a trial's SL distance from entry, as a fraction of entry price (2026-08-31
	// request, CLAUDE.md §19.2/config.Optimizer.MaxLossPct) — trials are unleveraged (§16.3), so
	// this is applied directly as a price-distance bound in signalPrices. Zero disables it.
	MaxLossPct decimal.Decimal
}

// Run is one time-boxed optimization pass for a single (InstID, Kind) target (CLAUDE.md §16.3).
type Run struct {
	ID     string
	InstID string
	Kind   string
	Bar    string

	cfg     RunConfig
	sidecar SuggestReporter
	store   *TrialStore
	repo    port.Repository
	logger  *slog.Logger

	studyID string
	specs   []strategy.ParamSpec
	specMap map[string]strategy.ParamSpec

	deadline  time.Time
	startedAt time.Time

	mu         sync.Mutex
	candidates map[int]*candidateState // keyed by sidecar TrialID
	done       bool
	status     RunStatus
}

// candidateState pairs a candidate's params with its running trial tally and whether it
// currently has an open trial in flight.
type candidateState struct {
	result CandidateResult
	active bool
}

// NewRun constructs a Run. origin is the base (unconfigured) strategy for kind, used to discover
// its tunable ParamSpecs — the search space handed to the sidecar.
func NewRun(runID, instID, kind string, origin strategy.Strategy, cfg RunConfig, sidecar SuggestReporter, store *TrialStore, repo port.Repository, logger *slog.Logger) *Run {
	specs := origin.Params()
	specMap := make(map[string]strategy.ParamSpec, len(specs))
	for _, s := range specs {
		specMap[s.Name] = s
	}
	now := time.Now().UTC()
	r := &Run{
		ID:         runID,
		InstID:     instID,
		Kind:       kind,
		Bar:        cfg.Bar,
		cfg:        cfg,
		sidecar:    sidecar,
		store:      store,
		repo:       repo,
		logger:     logger,
		studyID:    StudyID(instID, kind),
		specs:      specs,
		specMap:    specMap,
		startedAt:  now,
		deadline:   now.Add(cfg.RunDuration),
		candidates: make(map[int]*candidateState),
	}
	r.status = RunStatus{RunID: runID, InstID: instID, Kind: kind, Bar: cfg.Bar, StartedAt: now, Deadline: r.deadline}
	return r
}

// Status returns a snapshot of the run's current progress.
func (r *Run) Status() RunStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.status
	st.CandidateCount = len(r.candidates)
	trials := 0
	for _, c := range r.candidates {
		trials += c.result.TradeCount()
	}
	st.TrialsCompleted = trials
	return st
}

// Expired reports whether this run's time box has closed. Uses !Before rather than After so a
// zero-duration run (deadline == startedAt, used by tests) is treated as expired the instant it's
// checked rather than depending on enough wall-clock time passing between NewRun and the check to
// make deadline strictly in the past — on some platforms two time.Now() calls close together can
// read back equal, which made an After-based check flaky.
func (r *Run) Expired() bool {
	return !time.Now().UTC().Before(r.deadline)
}

// EnsureCandidates tops up this run's candidate pool from the sidecar up to cfg.BatchSize
// in-flight candidates (CLAUDE.md §16.3 step 1, refilled as trials complete).
func (r *Run) EnsureCandidates(ctx context.Context) error {
	r.mu.Lock()
	need := r.cfg.BatchSize - len(r.candidates)
	r.mu.Unlock()
	if need <= 0 || r.Expired() {
		return nil
	}

	specs := make([]SidecarParamSpec, 0, len(r.specs))
	for _, s := range r.specs {
		min, _ := s.Min.Float64()
		max, _ := s.Max.Float64()
		specs = append(specs, SidecarParamSpec{Name: s.Name, Min: min, Max: max})
	}

	proposed, err := r.sidecar.Suggest(ctx, r.studyID, specs, need)
	if err != nil {
		return fmt.Errorf("suggest candidates for study %s: %w", r.studyID, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range proposed {
		if _, exists := r.candidates[c.TrialID]; exists {
			continue
		}
		params := make(map[string]decimal.Decimal, len(c.Params))
		for k, v := range c.Params {
			d := v
			if spec, ok := r.specMap[k]; ok {
				d = strategy.ClampParam(spec, d)
			}
			params[k] = d
		}
		r.candidates[c.TrialID] = &candidateState{result: CandidateResult{TrialID: c.TrialID, Params: params}}
	}
	return nil
}

// candidateStrategy builds a live strategy.Strategy for one candidate's params via the existing
// factory/WithParams path (CLAUDE.md §16.3 step 2: reuse strategy.Factories/WithParams, do not
// reimplement strategy logic).
func candidateStrategy(kind string, params map[string]decimal.Decimal) (strategy.Strategy, error) {
	factory, ok := strategy.Factories[kind]
	if !ok {
		return nil, fmt.Errorf("unknown strategy kind %q", kind)
	}
	return factory().WithParams(params), nil
}

// EvaluateCandle runs every in-pool candidate that doesn't already have an open trial against the
// freshly-closed candle window, opening a new trial in Redis for any that emit a signal (CLAUDE.md
// §16.3 steps 2-4).
func (r *Run) EvaluateCandle(ctx context.Context, window []domain.Candle, price decimal.Decimal) {
	if r.Expired() {
		return
	}
	r.mu.Lock()
	type work struct {
		trialID int
		params  map[string]decimal.Decimal
	}
	var todo []work
	for id, c := range r.candidates {
		if c.active {
			continue // already has an open trial; wait for it to touch before opening another
		}
		todo = append(todo, work{trialID: id, params: c.result.Params})
	}
	r.mu.Unlock()

	for _, w := range todo {
		s, err := candidateStrategy(r.Kind, w.params)
		if err != nil {
			r.logger.Warn("failed to build candidate strategy", "kind", r.Kind, "trialId", w.trialID, "error", err)
			continue
		}
		// Deliberately the single-timeframe path (plain Evaluate, not strategy.EvaluateWith): a
		// tuning run evaluates one kind on one bar (CLAUDE.md §16.3), and this Run only maintains
		// that bar's window. Handing a MultiTimeframeStrategy a MarketView containing just its own
		// bar would let it silently score as if higher-timeframe confirmation were unavailable,
		// which is a different strategy than the one that would run in production — better to use
		// the interface that honestly reflects what this loop can supply. Optimizing a
		// multi-timeframe strategy needs the runner to maintain those bars first.
		signal, err := s.Evaluate(window)
		if err != nil {
			r.logger.Warn("candidate strategy evaluation failed", "kind", r.Kind, "trialId", w.trialID, "error", err)
			continue
		}
		if signal.Side == strategy.Hold {
			continue
		}

		slPx, tpPx := signalPrices(price, signal, r.cfg.MaxLossPct)
		trial := OpenTrial{
			StudyID:  r.studyID,
			TrialID:  w.trialID,
			InstID:   r.InstID,
			Side:     string(signal.Side),
			EntryPx:  price,
			SLPx:     slPx,
			TPPx:     tpPx,
			OpenedAt: time.Now().UTC(),
		}
		ttl := time.Until(r.deadline) + r.cfg.TrialTTLBuffer
		if ttl <= 0 {
			continue
		}
		if err := r.store.Open(ctx, r.ID, trial, ttl); err != nil {
			r.logger.Error("failed to open trial", "trialId", w.trialID, "error", err)
			continue
		}
		r.mu.Lock()
		if c, ok := r.candidates[w.trialID]; ok {
			c.active = true
		}
		r.mu.Unlock()
	}
}

// signalPrices resolves a signal's SL/TP percentages into prices. maxLossPct caps the SL distance
// from entry (2026-08-31 request, CLAUDE.md §19.2) — trials are unleveraged (no leverage field on
// OpenTrial, §16.3), so this is a direct price-distance cap rather than needing a leverage
// division like conductor.Clamps.maxSLDistPctFor does for production/tester positions. Only
// tightens an SL that would realize more than maxLossPct; never widens one, and never touches TP —
// profit is never capped, matching the other two SL-placing paths' identical rule. Zero disables
// the cap.
func signalPrices(entry decimal.Decimal, signal strategy.Signal, maxLossPct decimal.Decimal) (*decimal.Decimal, *decimal.Decimal) {
	var slPx, tpPx *decimal.Decimal
	direction := decimal.NewFromInt(1)
	if signal.Side == strategy.Sell {
		direction = decimal.NewFromInt(-1)
	}
	if signal.SLPct.IsPositive() {
		slPct := signal.SLPct
		if maxLossPct.IsPositive() && slPct.GreaterThan(maxLossPct) {
			slPct = maxLossPct
		}
		v := entry.Sub(direction.Mul(slPct).Mul(entry))
		slPx = &v
	}
	if signal.TPPct.IsPositive() {
		v := entry.Add(direction.Mul(signal.TPPct).Mul(entry))
		tpPx = &v
	}
	return slPx, tpPx
}

// CheckTick checks every open trial for InstID against price for an SL/TP touch (CLAUDE.md §16.3
// step 3), reusing usecase.SLTPTouchReason so the exact same domain logic PaperTrader uses judges
// these trials too. On a touch, records the outcome, reports it to the sidecar, deletes the Redis
// key, and clears the candidate's active flag so EvaluateCandle can re-open a fresh trial for it
// on the next candle close (CLAUDE.md §16.3 step 4) — as long as the run hasn't expired.
func (r *Run) CheckTick(ctx context.Context, price decimal.Decimal) {
	open, err := r.store.OpenForInst(ctx, r.ID, r.InstID)
	if err != nil {
		r.logger.Error("failed to list open trials", "instId", r.InstID, "error", err)
		return
	}
	for _, t := range open {
		reason, hit := usecase.SLTPTouchReason(t.Side, t.SLPx, t.TPPx, price)
		if !hit {
			continue
		}
		win := reason == "tp"

		closed, err := r.store.Close(ctx, r.ID, t)
		if err != nil {
			r.logger.Error("failed to close trial", "trialId", t.TrialID, "error", err)
			continue
		}
		if !closed {
			// Another caller already closed this exact trial (e.g. two ticks landing close
			// together both saw it open before either's Close took effect) — that caller already
			// recorded the outcome and reported it to the sidecar, so doing either again here would
			// double-count the trial's win/loss and would make Optuna's study.tell() reject a
			// second report for an already-COMPLETE trial.
			continue
		}

		r.mu.Lock()
		if c, ok := r.candidates[t.TrialID]; ok {
			if win {
				c.result.Wins++
			} else {
				c.result.Losses++
			}
			c.active = false
		}
		r.mu.Unlock()

		score := 0.0
		if win {
			score = 1.0
		}
		if err := r.sidecar.Report(ctx, r.studyID, t.TrialID, score); err != nil {
			r.logger.Warn("failed to report trial outcome to sidecar", "trialId", t.TrialID, "error", err)
		}
	}
}

// Finalize closes the run: picks the best eligible candidate (if any), decides whether it clears
// the persistence bar against baseline evidence, and — if it does — persists it as a new
// sub-strategy row + assignment + param-change log entry (CLAUDE.md §16.3 step 5). Idempotent —
// repeated calls after Done just return the same status.
func (r *Run) Finalize(ctx context.Context) RunStatus {
	r.mu.Lock()
	if r.done {
		st := r.status
		r.mu.Unlock()
		return st
	}
	results := make([]CandidateResult, 0, len(r.candidates))
	for _, c := range r.candidates {
		results = append(results, c.result)
	}
	r.mu.Unlock()

	eligible := EligibleCandidates(results, r.cfg.MinTradesPerCandidate)
	st := r.Status()
	st.Done = true

	best, ok := BestCandidate(eligible)
	if !ok {
		st.Outcome = "inconclusive: no candidate reached the minimum trade count"
		r.finish(st)
		return st
	}
	st.BestTrialID = best.TrialID
	st.BestWinRatePct = best.WinRatePct().String()

	baseline, originID, err := r.currentBaseline(ctx)
	if err != nil {
		r.logger.Warn("failed to resolve baseline for run", "runId", r.ID, "error", err)
	}
	persist := ShouldPersist(best, baseline, r.cfg.MinImprovementPct, r.cfg.MinWinRatePctFloor)
	if baseline.Exists {
		st.Outcome = fmt.Sprintf("baseline win rate %s%%, candidate win rate %s%%, required margin %s pp",
			baseline.WinRatePct.String(), best.WinRatePct().String(), r.cfg.MinImprovementPct.String())
	} else {
		st.Outcome = fmt.Sprintf("no baseline available, candidate win rate %s%%, required floor %s%%",
			best.WinRatePct().String(), r.cfg.MinWinRatePctFloor.String())
	}

	if !persist {
		r.finish(st)
		return st
	}

	stratID, err := r.persistWinner(ctx, best, originID)
	if err != nil {
		r.logger.Error("failed to persist winning candidate", "runId", r.ID, "error", err)
		st.Outcome += " — winning candidate found but persistence failed: " + err.Error()
		r.finish(st)
		return st
	}
	st.Persisted = true
	st.PersistedStrat = stratID
	r.finish(st)
	return st
}

func (r *Run) finish(st RunStatus) {
	r.mu.Lock()
	r.done = true
	r.status = st
	r.mu.Unlock()
}

// currentBaseline resolves baseline win-rate evidence for (InstID, Kind) (CLAUDE.md §16.6):
// the currently-assigned/enabled strategy of this kind for this instrument, if one exists, via
// its StrategyStatsFor Wins/Losses. Also returns the kind's origin row id (needed as ClonedFrom
// when persisting a winner) regardless of whether a baseline was found.
func (r *Run) currentBaseline(ctx context.Context) (Baseline, int64, error) {
	origins, err := r.repo.ListStrategies(ctx, "", false)
	if err != nil {
		return Baseline{}, 0, fmt.Errorf("list strategies: %w", err)
	}
	var originID int64
	for _, s := range origins {
		if s.IsOrigin && s.Kind == r.Kind {
			originID = s.ID
			break
		}
	}
	if originID == 0 {
		return Baseline{}, 0, fmt.Errorf("no origin strategy row found for kind %q", r.Kind)
	}

	assignments, err := r.repo.ListAssignments(ctx, r.InstID, true, "paper")
	if err != nil {
		return Baseline{}, originID, fmt.Errorf("list assignments for %s: %w", r.InstID, err)
	}
	var assignedStrategyID int64
	for _, a := range assignments {
		if a.Bar != r.Bar {
			continue
		}
		cfg, err := r.repo.GetStrategy(ctx, a.StrategyID)
		if err != nil {
			continue
		}
		if cfg.Kind == r.Kind {
			assignedStrategyID = a.StrategyID
			break
		}
	}
	if assignedStrategyID == 0 {
		return Baseline{}, originID, nil
	}

	stats, err := r.repo.StrategyStatsFor(ctx, assignedStrategyID, "paper")
	if err != nil {
		return Baseline{}, originID, fmt.Errorf("stats for baseline strategy %d: %w", assignedStrategyID, err)
	}
	total := stats.Wins + stats.Losses
	if total == 0 {
		// No closed-trade history yet for the current assignment — nothing meaningful to compare
		// against, fall back to the absolute floor (ShouldPersist's Baseline.Exists=false path).
		return Baseline{}, originID, nil
	}
	// NOTE (2026-08-30): StrategyStatsFor now defines a win as positive realized PnL rather than
	// close_reason='tp', because the RL ratchet makes an 'sl' close frequently profitable. That
	// makes this baseline PnL-based while a trial's own Wins/Losses (scoring.go) stay strictly
	// SL/TP-touch based, per §16.1's requirement that trial judgment never depend on anything the
	// RL agent touched. The two sides therefore measure slightly different things: a trial must
	// clear a bar set by trades the RL agent may have managed. Left as-is deliberately — the
	// alternative (an RL-free baseline) needs its own query, and the optimizer is not currently
	// running on a schedule. Revisit before re-enabling scheduled optimization runs.
	winRate := decimal.NewFromInt(stats.Wins).Div(decimal.NewFromInt(total)).Mul(decimal.NewFromInt(100))
	return Baseline{Exists: true, WinRatePct: winRate}, originID, nil
}

func (r *Run) persistWinner(ctx context.Context, best CandidateResult, originID int64) (int64, error) {
	config, err := ParamsToConfig(best.Params)
	if err != nil {
		return 0, fmt.Errorf("marshal winning params: %w", err)
	}

	name := fmt.Sprintf("%s-optimized-%s-%d", r.Kind, r.InstID, time.Now().UTC().Unix())
	stratID, err := r.repo.CreateStrategy(ctx, port.StrategyConfig{
		Name:       name,
		InstIDs:    []string{r.InstID},
		Kind:       r.Kind,
		Config:     config,
		Enabled:    true,
		IsOrigin:   false,
		ClonedFrom: &originID,
	})
	if err != nil {
		return 0, fmt.Errorf("create winning strategy row: %w", err)
	}

	if _, err := r.repo.CreateAssignment(ctx, port.StrategyAssignment{
		StrategyID: stratID,
		InstID:     r.InstID,
		Bar:        r.Bar,
		Enabled:    true,
	}); err != nil {
		return stratID, fmt.Errorf("assign winning strategy: %w", err)
	}

	// Best-effort: the strategy/assignment above already succeeded, so a logging failure here
	// must not be reported as the run having failed.
	var oldConfig json.RawMessage
	if _, err := r.repo.RecordParamChange(ctx, port.ParamChange{
		StrategyID: stratID,
		InstID:     r.InstID,
		OldConfig:  oldConfig,
		NewConfig:  config,
		Source:     "optimizer",
	}); err != nil {
		r.logger.Warn("failed to record param change for winning candidate", "strategyId", stratID, "error", err)
	}

	return stratID, nil
}

// ParamsToConfig marshals a candidate's param map into the json.RawMessage shape
// strategy.FromConfig/port.StrategyConfig.Config expects (a flat {"name": number, ...} object).
func ParamsToConfig(params map[string]decimal.Decimal) (json.RawMessage, error) {
	raw := make(map[string]float64, len(params))
	for k, v := range params {
		f, _ := v.Float64()
		raw[k] = f
	}
	return json.Marshal(raw)
}
