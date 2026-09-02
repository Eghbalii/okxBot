-- Append-only in-trade SL/TP adjustment log (CLAUDE.md §15.4/§15.12 revision, 2026-09-02): the
-- RL SL/TP-adjust mechanic used to clone an order into a linked 'rl_adjusted' shadow fork rather
-- than edit it in place, intended as a same-entry A/B comparison. Fork volume grew large enough
-- to distort per-strategy stats and made it hard to tell a real adjustment from a duplicate. The
-- mechanic now edits the one real (baseline) order directly; this table is the audit trail that
-- replaces the fork — every SL/TP move gets its own row here instead of its own paper_orders row,
-- so clicking an order in the panel can still show exactly what changed and when.
CREATE TABLE IF NOT EXISTS paper_order_adjustments (
    id BIGSERIAL PRIMARY KEY,
    order_id BIGINT NOT NULL REFERENCES paper_orders(id),
    field TEXT NOT NULL CHECK (field IN ('sl', 'tp')),
    old_value NUMERIC,
    new_value NUMERIC,
    source TEXT NOT NULL DEFAULT 'model' CHECK (source IN ('model', 'optimizer', 'manual')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_paper_order_adjustments_order
    ON paper_order_adjustments (order_id, created_at);
