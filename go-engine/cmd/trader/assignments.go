package main

import (
	"log/slog"

	"github.com/eghbalii/okxBot/go-engine/internal/strategy"
)

// buildAssignmentStrategy turns one stored strategy row into a runnable Strategy, reporting whether
// the assignment should be used.
//
// Extracted so the decision this function encodes — an unbuildable strategy is SKIPPED, never
// fatal — is unit-testable without a live Postgres. That distinction took real trading down for
// eight hours on 2026-09-13 (CLAUDE.md §47), and the whole point of pulling it out is that the
// regression test can reproduce the exact production failure rather than approximate it.
//
// ok=false means "do not trade this assignment". It never means "stop the service": the remaining
// assignments are runnable, and a strategy that cannot be built simply does not trade, which is the
// same outcome as it being disabled — reached without an outage.
func buildAssignmentStrategy(kind string, config []byte, logger *slog.Logger, instID string, assignmentID, strategyID int64) (strategy.Strategy, bool) {
	s, err := strategy.FromConfig(kind, config)
	if err != nil {
		logger.Error("skipping unusable strategy assignment — this strategy will not trade",
			"instId", instID, "assignmentId", assignmentID, "strategyId", strategyID,
			"kind", kind, "error", err)
		return nil, false
	}
	return s, true
}
