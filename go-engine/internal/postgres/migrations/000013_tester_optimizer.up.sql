-- Automatic per-kind optimization loop for cmd/strategy-tester (2026-08-31 request): the tester
-- service itself proposes new parameter candidates, judges them by win rate + PnL once they've
-- accumulated enough trades, and creates the next version from whichever version (including the
-- origin) currently scores best -- entirely independent of cmd/strategy-optimizer/production's
-- optimizer-service study namespace (a distinct study_id prefix, "tester:{kind}", guarantees no
-- Optuna study can ever collide between the two callers -- see CLAUDE.md's optimizer-service doc
-- comment: studies are keyed by an opaque caller-chosen string with no shared state between keys).

-- trial_id is the Optuna sidecar's opaque per-candidate trial number (optimizer-service's
-- study.ask() return value) for the version that WAS created as a candidate from that trial --
-- NULL for version 1 (the origin, never proposed by Optuna) and for any version created manually
-- from the panel (CLAUDE.md request: manual edits stay a distinct path from automatic proposals).
ALTER TABLE tester_strategy_versions ADD COLUMN IF NOT EXISTS trial_id INT;
ALTER TABLE tester_strategy_versions ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'manual'
    CHECK (source IN ('origin', 'manual', 'optimizer'));

-- One row per kind: which version (if any) is the optimizer's current in-flight candidate. Kept
-- separate from tester_strategy_versions.enabled, which the operator can freely override from the
-- panel (CLAUDE.md request: "Make live" stays a manual action) without confusing the optimizer's
-- own bookkeeping of what it is currently waiting to judge.
CREATE TABLE IF NOT EXISTS tester_optimizer_state (
    kind TEXT PRIMARY KEY,
    candidate_version_id BIGINT REFERENCES tester_strategy_versions(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
