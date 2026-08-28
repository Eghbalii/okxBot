-- Shared account equity + equity timeline (CLAUDE.md §15.6, revised 2026-08-28).
--
-- Replaces the per-token $10 sub-budgets (token_budgets, migration 000004) with ONE account
-- balance per trading mode. Per-token buckets were an explicit Phase A simplification that made
-- capital allocation a config constant rather than a decision; the revised design gives the RL
-- agent one shared pool to size trades against, bounded by Go-side caps rather than by pre-split
-- buckets. See §15.6 for why the original per-token reasoning no longer applies.
--
-- Scoped by mode (paper/demo/real) so all three can be tracked simultaneously without the paper
-- engine's auto-reset behavior ever touching a real-money balance (§15.7's real-mode carve-out).
CREATE TABLE IF NOT EXISTS account_equity (
    mode TEXT PRIMARY KEY CHECK (mode IN ('paper', 'demo', 'real')),
    -- The configured starting balance a reset returns to; also what equity_usd is seeded at.
    initial_usd NUMERIC NOT NULL,
    equity_usd NUMERIC NOT NULL,
    reset_count INTEGER NOT NULL DEFAULT 0,
    last_reset_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Equity timeline (explicit product requirement, 2026-08-28): the operator isn't watching the logs
-- when a drain/reset happens overnight, so every balance change is recorded as a durable row the
-- panel can chart after the fact. A reset is just another row with reason='reset', which is what
-- makes "it drained at 3am and reset twice" visible instead of being a log line nobody saw.
--
-- NUMERIC for the monetary columns, per CLAUDE.md §7 — note token_budgets (000004) used DOUBLE
-- PRECISION, which was inconsistent with that rule; this table does not repeat that.
CREATE TABLE IF NOT EXISTS account_equity_history (
    id BIGSERIAL PRIMARY KEY,
    mode TEXT NOT NULL CHECK (mode IN ('paper', 'demo', 'real')),
    -- Equity AFTER this change, so charting is a plain read with no running-sum reconstruction.
    equity_usd NUMERIC NOT NULL,
    -- The signed amount that produced this row: realized PnL for a trade close, or the top-up
    -- amount for a reset. Zero for a seed row.
    delta_usd NUMERIC NOT NULL DEFAULT 0,
    reason TEXT NOT NULL CHECK (reason IN ('trade', 'reset', 'seed')),
    -- The paper order whose close produced this row, when reason='trade'. Nullable: reset/seed
    -- rows have no originating order, and demo/real rows may not map onto a paper_orders id.
    order_id BIGINT REFERENCES paper_orders(id),
    inst_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The panel's chart reads one mode over a time range, newest-relevant-first.
CREATE INDEX IF NOT EXISTS idx_account_equity_history_mode_created
    ON account_equity_history (mode, created_at DESC);

-- token_budgets is superseded. Dropped rather than left in place: leaving a stale per-token budget
-- table around invites a future reader (or a half-migrated code path) to keep writing to it, and
-- the running balances in it are disposable paper-mode bookkeeping, not trade history — the real
-- record of what happened is paper_orders, which is untouched by this migration.
DROP TABLE IF EXISTS token_budgets;
