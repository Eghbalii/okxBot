-- SL/TP shadow forks (CLAUDE.md §15.4): when the RL agent proposes an in-trade SL/TP adjustment on
-- an open paper order, the original order is never edited in place. Instead a linked "fork" row is
-- created carrying the ratcheted SL/TP, and both rows monitor independently against the live price
-- feed to completion — this is what lets the original (baseline) and RL-adjusted variant be
-- compared afterward (win rate, PnL) as a same-entry A/B, rather than the adjustment silently
-- overwriting ground truth.
ALTER TABLE paper_orders
    ADD COLUMN IF NOT EXISTS parent_order_id BIGINT REFERENCES paper_orders(id),
    ADD COLUMN IF NOT EXISTS variant TEXT NOT NULL DEFAULT 'baseline' CHECK (variant IN ('baseline', 'rl_adjusted'));

CREATE INDEX IF NOT EXISTS idx_paper_orders_parent ON paper_orders (parent_order_id) WHERE parent_order_id IS NOT NULL;

-- A fork is tracking-only (CLAUDE.md §15.4/§15.6/§15.7 decision): it must never be counted a
-- second time toward a token's budget/reward. Enforced at the query level (token_budgets updates
-- filter WHERE variant = 'baseline'), documented here since it's not expressible as a constraint.

-- Per-token budget tracking (CLAUDE.md §15.7): "give a drained token another chance" needs a
-- running balance the system can act on, distinct from summing paper_orders on every check.
CREATE TABLE IF NOT EXISTS token_budgets (
    inst_id TEXT PRIMARY KEY,
    budget_usd DOUBLE PRECISION NOT NULL,
    equity_usd DOUBLE PRECISION NOT NULL,
    reset_count INTEGER NOT NULL DEFAULT 0,
    last_reset_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
