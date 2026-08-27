-- Parameter-change history (CLAUDE.md §16, strategy parameter optimizer): every time a
-- strategy's Config changes — either because cmd/strategy-optimizer just persisted a winning
-- tuned candidate (§16.3 step 5) or because an operator edited a sub-strategy's params by hand
-- in the panel (§11.3/§11.5) — a row is recorded here. This is what lets the panel draw the
-- "parameter changed here" marker line on a strategy's price chart (§16, point 6 of the
-- implementation session) without reconstructing history from strategies.updated_at (which only
-- has the current value, not what changed or why).
CREATE TABLE IF NOT EXISTS strategy_param_changes (
    id BIGSERIAL PRIMARY KEY,
    strategy_id BIGINT NOT NULL REFERENCES strategies(id),
    inst_id TEXT NOT NULL,
    old_config JSONB,
    new_config JSONB NOT NULL,
    source TEXT NOT NULL DEFAULT 'optimizer' CHECK (source IN ('optimizer', 'manual')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_strategy_param_changes_inst_created
    ON strategy_param_changes (inst_id, created_at);
