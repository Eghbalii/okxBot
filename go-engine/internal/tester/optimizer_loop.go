package tester

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eghbalii/okxBot/go-engine/internal/optimizer"
	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// SuggestReporter is the Optuna sidecar surface the loop needs — satisfied by
// *optimizer.SidecarClient (reused as-is: it is a stateless, side-effect-free HTTP wrapper with no
// dependency on production's database or config, see optimizer_service/api.py's own doc comment
// that studies are keyed by an opaque caller-chosen string). Narrowed to an interface so tests can
// fake it without a real optimizer-service process.
type SuggestReporter interface {
	Suggest(ctx context.Context, studyID string, specs []optimizer.SidecarParamSpec, n int) ([]optimizer.Candidate, error)
	Report(ctx context.Context, studyID string, trialID int, score float64) error
}

// OptimizerLoop is cmd/strategy-tester's automatic per-kind optimization loop (2026-08-31
// request): on each Tick, for every registered kind it either judges that kind's in-flight
// candidate (once it has accumulated MinTradesToScore trades) or proposes a new one, basing every
// new candidate on whichever version currently scores best across the kind's ENTIRE history
// (origin included) — never just the most recent version — per the operator's explicit "هر سری که
// میخواد ورژن جدید بسازه یدور تمام ورژن قدیمی ها رو بخونه نتایجشون رو و آپدیت جدید رو از روی اون
// بسازه". A new candidate is created already enabled, since this service has no shadow/A-B
// mechanic (CLAUDE.md: cmd/strategy-tester trades one version per kind live at a time) — the
// candidate immediately starts accumulating the trades that will judge it.
//
// Deliberately independent of internal/optimizer/cmd/strategy-optimizer end to end: separate
// storage (Store, tester_strategy_versions/tester_optimizer_state), separate Optuna study
// namespace (StudyID), separate scoring rule (win rate AND PnL together, MinTradesToScore=30 vs.
// production's configurable floor) — nothing in this file can read, write, or otherwise affect
// production's strategies/strategy_assignments/paper_orders tables or its own optimizer's studies.
type OptimizerLoop struct {
	store   *Store
	sidecar SuggestReporter
	logger  *slog.Logger
}

// NewOptimizerLoop constructs a loop. sidecar is typically optimizer.NewSidecarClient(url) —
// pointed at the SAME optimizer-service deployment production's cmd/strategy-optimizer uses (the
// operator's own call: "optimizer-service... کلا برای همین سرویس هست... از همون استفاده کن") —
// safe because Optuna studies are isolated per study_id string with no shared mutable state.
func NewOptimizerLoop(store *Store, sidecar SuggestReporter, logger *slog.Logger) *OptimizerLoop {
	return &OptimizerLoop{store: store, sidecar: sidecar, logger: logger}
}

// Tick runs one pass over every kind, called on the scheduler's interval (config
// tester.optimize.check_interval, default 1h per the operator's explicit "لوپ رو هر ۱ ساعت اجرا
// کنیم تا ببینه کی اماده هست"). onVersionCreated lets the caller reload its live strategies after
// a new version is created/enabled — mirrors handleCreateVersion's existing reloadStrategies call.
func (l *OptimizerLoop) Tick(ctx context.Context, kinds []string, onVersionCreated func(ctx context.Context) error) {
	for _, kind := range kinds {
		if err := l.tickKind(ctx, kind); err != nil {
			l.logger.Error("optimizer loop: tick failed for kind", "kind", kind, "error", err)
			continue
		}
	}
	if onVersionCreated != nil {
		if err := onVersionCreated(ctx); err != nil {
			l.logger.Error("optimizer loop: failed to reload strategies after tick", "error", err)
		}
	}
}

func (l *OptimizerLoop) tickKind(ctx context.Context, kind string) error {
	candidateID, err := l.store.CandidateVersionID(ctx, kind)
	if err != nil {
		return fmt.Errorf("get candidate for kind %q: %w", kind, err)
	}

	if candidateID != nil {
		return l.judgeCandidate(ctx, kind, *candidateID)
	}
	return l.proposeCandidate(ctx, kind)
}

// judgeCandidate checks whether kind's in-flight candidate has accumulated enough trades to be
// judged. If not, it leaves the candidate in flight for a later tick. If so, it reports the
// candidate's score to the sidecar (closing the Optuna ask/tell loop for this trial) and clears
// the in-flight marker — the NEXT tick's proposeCandidate call re-reads every version's score
// (including this one, now with enough trades to count) and proposes fresh from whichever wins.
func (l *OptimizerLoop) judgeCandidate(ctx context.Context, kind string, candidateID int64) error {
	v, err := l.store.GetVersion(ctx, candidateID)
	if err != nil {
		return fmt.Errorf("get candidate version %d: %w", candidateID, err)
	}
	stats, err := l.store.VersionStatsFor(ctx, candidateID)
	if err != nil {
		return fmt.Errorf("stats for candidate version %d: %w", candidateID, err)
	}
	score := scoreFromStats(kind, candidateID, stats)
	if !score.Eligible() {
		l.logger.Info("optimizer loop: candidate not ready yet", "kind", kind, "versionId", candidateID,
			"trades", score.TradeCount, "needed", MinTradesToScore)
		return nil
	}

	if v.TrialID != nil {
		if err := l.sidecar.Report(ctx, StudyID(kind), *v.TrialID, sidecarScore(score)); err != nil {
			l.logger.Warn("optimizer loop: failed to report candidate score to sidecar",
				"kind", kind, "versionId", candidateID, "error", err)
		}
	}
	if err := l.store.SetCandidateVersionID(ctx, kind, nil); err != nil {
		return fmt.Errorf("clear candidate for kind %q: %w", kind, err)
	}
	l.logger.Info("optimizer loop: candidate judged", "kind", kind, "versionId", candidateID,
		"trades", score.TradeCount, "winRatePct", score.WinRatePct.StringFixed(1), "realizedPnl", score.RealizedPnL.StringFixed(2))
	return nil
}

// sidecarScore maps a VersionScore onto Optuna's [0,1] maximize objective. Win rate alone
// (production's convention, internal/optimizer/scoring.go's Score()) would ignore the PnL half of
// this loop's own comparison rule, so this blends both: win rate as a fraction, nudged by whether
// PnL was positive or negative, keeping the result in [0,1] the same way production's Score() does
// (a candidate with zero trades is handled by the eligibility gate above, never reaches here).
func sidecarScore(s VersionScore) float64 {
	winFrac, _ := s.WinRatePct.Div(decimal.NewFromInt(100)).Float64()
	if s.RealizedPnL.IsNegative() {
		winFrac -= 0.1
	} else if s.RealizedPnL.IsPositive() {
		winFrac += 0.1
	}
	if winFrac < 0 {
		return 0
	}
	if winFrac > 1 {
		return 1
	}
	return winFrac
}

// proposeCandidate finds the best-scoring version of kind across its ENTIRE history (origin
// included), asks the sidecar for one new candidate parameter set nudged from that version's own
// params, and creates+enables it as the kind's next version.
func (l *OptimizerLoop) proposeCandidate(ctx context.Context, kind string) error {
	versions, err := l.store.VersionsForKind(ctx, kind)
	if err != nil {
		return fmt.Errorf("list versions for kind %q: %w", kind, err)
	}
	if len(versions) == 0 {
		return fmt.Errorf("no versions found for kind %q (origin not seeded?)", kind)
	}

	scores := make([]VersionScore, 0, len(versions))
	byID := make(map[int64]Version, len(versions))
	for _, v := range versions {
		byID[v.ID] = v
		stats, err := l.store.VersionStatsFor(ctx, v.ID)
		if err != nil {
			return fmt.Errorf("stats for version %d: %w", v.ID, err)
		}
		scores = append(scores, scoreFromStats(kind, v.ID, stats))
	}
	best, ok := BestScore(scores)
	if !ok {
		return fmt.Errorf("no scoreable versions for kind %q", kind)
	}
	baseVersion := byID[best.VersionID]

	factory, ok := strategy.Factories[kind]
	if !ok {
		return fmt.Errorf("unknown strategy kind %q", kind)
	}
	specs := factory().Params()
	if len(specs) == 0 {
		// Nothing tunable (e.g. a pure crossover with no numeric params) — this kind can never
		// produce a different candidate, so the loop has nothing to do for it. Not an error.
		l.logger.Info("optimizer loop: kind has no tunable params, skipping", "kind", kind)
		return nil
	}
	sidecarSpecs := make([]optimizer.SidecarParamSpec, 0, len(specs))
	for _, s := range specs {
		min, _ := s.Min.Float64()
		max, _ := s.Max.Float64()
		sidecarSpecs = append(sidecarSpecs, optimizer.SidecarParamSpec{Name: s.Name, Min: min, Max: max})
	}

	candidates, err := l.sidecar.Suggest(ctx, StudyID(kind), sidecarSpecs, 1)
	if err != nil {
		return fmt.Errorf("suggest candidate for kind %q: %w", kind, err)
	}
	if len(candidates) == 0 {
		return fmt.Errorf("sidecar returned no candidates for kind %q", kind)
	}
	candidate := candidates[0]

	specByName := make(map[string]strategy.ParamSpec, len(specs))
	for _, s := range specs {
		specByName[s.Name] = s
	}
	params := make(map[string]float64, len(candidate.Params))
	for name, value := range candidate.Params {
		v := value
		if spec, ok := specByName[name]; ok {
			v = strategy.ClampParam(spec, v)
		}
		f, _ := v.Float64()
		params[name] = f
	}
	configJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal candidate params for kind %q: %w", kind, err)
	}

	trialID := candidate.TrialID
	newID, err := l.store.CreateVersion(ctx, kind, configJSON, baseVersion.ID, "optimizer", &trialID)
	if err != nil {
		return fmt.Errorf("create candidate version for kind %q: %w", kind, err)
	}
	if err := l.store.SetCandidateVersionID(ctx, kind, &newID); err != nil {
		return fmt.Errorf("record candidate for kind %q: %w", kind, err)
	}
	l.logger.Info("optimizer loop: proposed new candidate", "kind", kind, "versionId", newID,
		"basedOn", baseVersion.ID, "baseVersion", baseVersion.Version, "trialId", trialID)
	return nil
}

// StartScheduler runs Tick on interval until ctx is done — mirrors cmd/strategy-optimizer's own
// scheduler loop shape.
func (l *OptimizerLoop) StartScheduler(ctx context.Context, interval time.Duration, kinds []string, onVersionCreated func(ctx context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.Tick(ctx, kinds, onVersionCreated)
		}
	}
}
