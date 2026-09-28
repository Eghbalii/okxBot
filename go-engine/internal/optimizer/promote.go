package optimizer

import (
	"context"
	"fmt"

	"github.com/eghbalii/okxBot/go-engine/internal/port"
)

// Promote turns a 'backtest_passed' candidate into a live production strategy: a new
// strategies row cloned from its kind's locked origin (§11.3's model — never edits the origin
// itself), holding the candidate's own tuned Config, plus a strategy_assignments row binding it
// to the candidate's (inst_id, bar) under mode="paper" and the candidate's own exchange — this is
// what actually makes it start paper trading, since PaperTrader only evaluates strategies with a
// live assignment row (CLAUDE.md §11.3).
//
// If the lineage already has a DIFFERENT 'paper_active' candidate, this call also demotes it (via
// Store.PromoteCandidate's single transaction) and disables its OWN assignment row — otherwise
// two versions of the same lineage would both be live simultaneously, silently doubling the
// slot's trade count and corrupting the per-strategy stats this whole pipeline exists to produce.
//
// leverage is the risk profile's own ceiling (10 for "low", 100 for "high" today), used only to
// render the display name's R<leverage> component — purely cosmetic, the real leverage cap is
// enforced by risk.max_leverage/conductor.Clamps at trade time, not by anything in this name.
func Promote(ctx context.Context, store *Store, repo port.Repository, candidateID int64, leverage int) (int64, error) {
	c, err := store.GetCandidate(ctx, candidateID)
	if err != nil {
		return 0, err
	}
	if c.Status != "backtest_passed" {
		return 0, fmt.Errorf("candidate %d is %q, not backtest_passed — refusing to promote", candidateID, c.Status)
	}
	if c.Source == "origin" {
		// EnsureOriginCandidate's per-lineage placeholder row (empty {} config, never run through
		// Validate) — always carries status='backtest_passed' by construction, but promoting it
		// would just re-clone the origin with no tuning at all. Never a real result.
		return 0, fmt.Errorf("candidate %d is the lineage's origin placeholder, not a real tuned result — refusing to promote", candidateID)
	}

	previous, hadPrevious, err := store.ActiveCandidate(ctx, c.Lineage)
	if err != nil {
		return 0, err
	}

	originID, err := findOriginStrategyID(ctx, repo, c.Lineage.Kind)
	if err != nil {
		return 0, err
	}

	name := c.DisplayName(leverage)
	strategyID, err := repo.CreateStrategy(ctx, port.StrategyConfig{
		Name:       name,
		Kind:       c.Lineage.Kind,
		Config:     c.Config,
		Enabled:    true,
		IsOrigin:   false,
		ClonedFrom: &originID,
	})
	if err != nil {
		return 0, fmt.Errorf("create strategy for candidate %d: %w", candidateID, err)
	}

	if _, err := repo.CreateAssignment(ctx, port.StrategyAssignment{
		StrategyID: strategyID,
		InstID:     c.Lineage.InstID,
		Bar:        c.Lineage.Bar,
		Enabled:    true,
		Mode:       "paper",
		Exchange:   c.Lineage.Exchange,
	}); err != nil {
		return 0, fmt.Errorf("create assignment for candidate %d's strategy %d: %w", candidateID, strategyID, err)
	}

	if err := store.PromoteCandidate(ctx, candidateID, strategyID); err != nil {
		return 0, fmt.Errorf("promote candidate %d: %w", candidateID, err)
	}

	// Record this promotion on the lineage's parameter-change timeline (2026-09-28) — the same
	// durable table/panel modal §16.7 already built for manual panel edits, reused rather than
	// rebuilt: a generation-over-generation promotion IS a parameter change (old tuned values ->
	// new tuned values), just sourced from the optimizer instead of an operator's hand edit. Only
	// recorded when there's a PREVIOUS promoted candidate to diff against — a lineage's first-ever
	// promotion has no "old" config to show, which RecordParamChange already represents as a nil
	// OldConfig. Best-effort: a failure here must not undo the promotion that already succeeded.
	var oldConfig []byte
	if hadPrevious {
		oldConfig = previous.Config
	}
	// Best-effort: Promote has no logger of its own, and a bookkeeping-timeline write failing here
	// must never undo or fail the promotion itself, which already succeeded above.
	_, _ = repo.RecordParamChange(ctx, port.ParamChange{
		StrategyID: strategyID,
		InstID:     c.Lineage.InstID,
		OldConfig:  oldConfig,
		NewConfig:  c.Config,
		Source:     "optimizer",
	})

	// Disable the previously-active candidate's own assignment so it stops trading — its
	// strategies row and history stay in the database (nothing is deleted, matching this
	// pipeline's own "backtest_rejected rows are kept for audit" posture), it just stops being
	// evaluated going forward.
	if hadPrevious && previous.StrategyID != nil {
		if err := disableAssignmentFor(ctx, repo, *previous.StrategyID, c.Lineage.InstID, c.Lineage.Bar, c.Lineage.Exchange); err != nil {
			return 0, fmt.Errorf("disable previous candidate %d's assignment: %w", previous.ID, err)
		}
	}

	// Also disable the ORIGIN's own assignment for this exact lineage (2026-09-28 fix) — the
	// hadPrevious branch above only covers a lineage's SECOND-and-later promotion, because the
	// origin is never itself a 'paper_active' candidate ActiveCandidate can see: it's a separate,
	// always-enabled bootstrap assignment (ensureDefaultAssignment/SeedOrigins) that predates the
	// optimizer pipeline entirely and that Promote() never otherwise touches. Without this, a
	// lineage's FIRST-ever promotion left both the origin AND the new clone trading side by side
	// indefinitely — found live: origin and clone assignments both enabled for the same
	// (kind, inst, bar), producing duplicate trades from the same setup and corrupting the
	// per-strategy stats this whole pipeline exists to produce. Unconditional and idempotent: if
	// the origin's assignment is already disabled (or never existed), this is a no-op.
	if err := disableAssignmentFor(ctx, repo, originID, c.Lineage.InstID, c.Lineage.Bar, c.Lineage.Exchange); err != nil {
		return 0, fmt.Errorf("disable origin assignment for candidate %d's lineage: %w", candidateID, err)
	}

	return strategyID, nil
}

func findOriginStrategyID(ctx context.Context, repo port.Repository, kind string) (int64, error) {
	strategies, err := repo.ListStrategies(ctx, "", false)
	if err != nil {
		return 0, fmt.Errorf("list strategies to find origin for kind %q: %w", kind, err)
	}
	for _, s := range strategies {
		if s.IsOrigin && s.Kind == kind {
			return s.ID, nil
		}
	}
	return 0, fmt.Errorf("no origin strategy row for kind %q — has strategy.SeedOrigins run?", kind)
}

func disableAssignmentFor(ctx context.Context, repo port.Repository, strategyID int64, instID, bar, exchange string) error {
	assignments, err := repo.ListAssignments(ctx, instID, false, "paper", exchange)
	if err != nil {
		return err
	}
	for _, a := range assignments {
		if a.StrategyID == strategyID && a.Bar == bar {
			return repo.SetAssignmentEnabled(ctx, a.ID, false)
		}
	}
	// Not found is not an error: the previous candidate may have been promoted before this
	// package's own assignment bookkeeping existed, or its assignment was already disabled
	// manually from the panel. Nothing to demote is a valid, quiet outcome.
	return nil
}
