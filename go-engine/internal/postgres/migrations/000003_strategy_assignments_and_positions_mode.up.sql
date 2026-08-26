-- Strategy parent/override model (CLAUDE.md §11.3): every strategy row descends from a locked
-- "origin" row; sub-strategies store only their own config override and are assigned to
-- token+timeframe pairs via strategy_assignments, loaded from Postgres on every process start so
-- assignment state survives a crash/restart instead of living only in cmd/paper-trader's main.go.
ALTER TABLE strategies
    ADD COLUMN IF NOT EXISTS is_origin BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS strategy_assignments (
    id BIGSERIAL PRIMARY KEY,
    strategy_id INTEGER NOT NULL REFERENCES strategies(id),
    inst_id TEXT NOT NULL,
    bar TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (strategy_id, inst_id, bar)
);

CREATE INDEX IF NOT EXISTS idx_strategy_assignments_inst_bar
    ON strategy_assignments (inst_id, bar) WHERE enabled;

-- Positions panel (CLAUDE.md §11.4): paper/demo/real share one shape. paper_orders becomes the
-- "paper" mode; demo/real orders land in the same table going forward so the panel/API query one
-- place regardless of mode, distinguishing only by the mode column.
ALTER TABLE paper_orders
    ADD COLUMN IF NOT EXISTS mode TEXT NOT NULL DEFAULT 'paper' CHECK (mode IN ('paper', 'demo', 'real'));

CREATE INDEX IF NOT EXISTS idx_paper_orders_mode ON paper_orders (mode);
