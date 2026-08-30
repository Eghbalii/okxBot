-- Independent strategy-testing service (CLAUDE.md request 2026-08-30): a standalone paper-trading
-- copy that opens real positions against live prices to validate whether a strategy's signal is
-- worth anything BEFORE the RL agent ever sees it (independent, since RL sizing/SL-TP-adjust is a
-- separate question from "does this strategy fire a usable signal", same separation of concerns
-- as the strategy-optimizer's §16.1). Deliberately named and namespaced apart from every existing
-- table (strategies, strategy_assignments, paper_orders) so this service cannot collide with or
-- be mistaken for the production paper-trading path.
--
-- No FK to `strategies`: a tester version is versioned independently (see below) and must survive
-- a production strategy row being deleted/reset without cascading.
CREATE TABLE IF NOT EXISTS tester_strategy_versions (
    id BIGSERIAL PRIMARY KEY,
    -- Matches strategy.Factories' kind key (e.g. "grid_like") — NOT a FK, this only needs the
    -- string to build a live strategy.Strategy via the same factory/WithParams path production
    -- uses (CLAUDE.md §16.3's "reuse strategy.Factories, don't reimplement").
    kind TEXT NOT NULL,
    -- version is 1-based per kind: kind="grid_like" version=1, then version=2 after the first
    -- manual param edit, etc. Display name is derived as "{kind}_v{version}" rather than stored,
    -- so renaming the convention later doesn't require a backfill.
    version INT NOT NULL,
    config JSONB NOT NULL DEFAULT '{}',
    -- NULL for version 1 (the unmodified origin defaults). Set for every later version so the
    -- panel's "compare to parent" view has something to diff against without walking the whole
    -- chain — CLAUDE.md request: "click a new version's name to see the parent's params vs. this
    -- version's params".
    parent_version_id BIGINT REFERENCES tester_strategy_versions(id),
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (kind, version)
);

CREATE INDEX IF NOT EXISTS idx_tester_strategy_versions_kind
    ON tester_strategy_versions (kind, version);

-- Virtual positions this service opens, tracked to completion exactly like paper_orders but
-- entirely separate storage: this must never be joinable with or mistaken for real paper-trading
-- data, since its whole purpose is judging a strategy version in isolation.
CREATE TABLE IF NOT EXISTS tester_orders (
    id BIGSERIAL PRIMARY KEY,
    inst_id TEXT NOT NULL,
    version_id BIGINT NOT NULL REFERENCES tester_strategy_versions(id),
    bar TEXT NOT NULL,
    side TEXT NOT NULL CHECK (side IN ('buy', 'sell')),
    entry_px NUMERIC NOT NULL,
    sl_px NUMERIC,
    tp_px NUMERIC,
    size NUMERIC NOT NULL,
    leverage NUMERIC NOT NULL,
    opened_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at TIMESTAMPTZ,
    -- No 'manual'/'timeout'/'rl_early' — this service has no operator-close or model-early-close
    -- path (CLAUDE.md request: "no update mechanic like the main system"). Only a live SL/TP touch
    -- ever closes a position here.
    close_reason TEXT CHECK (close_reason IN ('sl', 'tp')),
    close_px NUMERIC,
    realized_pnl NUMERIC
);

CREATE INDEX IF NOT EXISTS idx_tester_orders_version ON tester_orders (version_id);
CREATE INDEX IF NOT EXISTS idx_tester_orders_open ON tester_orders (inst_id) WHERE closed_at IS NULL;

-- Runtime-editable config for cmd/strategy-tester (panel's config section, 2026-08-30 request):
-- a single row, read at startup and on every reload. Kept in Postgres rather than the YAML file
-- so a panel edit doesn't need write access to the server's config.yaml or a redeploy — the
-- service reads this row's overrides at startup, falling back to config.yaml/env defaults for
-- anything NULL.
CREATE TABLE IF NOT EXISTS tester_config (
    id INT PRIMARY KEY DEFAULT 1 CHECK (id = 1), -- singleton row
    bar TEXT,
    notional_usd NUMERIC,
    leverage NUMERIC,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
