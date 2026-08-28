-- Peak and trough unrealized PnL reached while a paper order was open (CLAUDE.md §15.11).
--
-- These are model INPUT, not reporting: a trade that ran to 90% of its target and gave it all back
-- teaches something completely different from one that drifted sideways to the same current PnL,
-- and current PnL alone cannot express that difference. Without them the RL agent has no way to
-- learn "this position already showed me a profit I failed to take".
--
-- Persisted rather than tracked in memory because the paper-trader restarts: an in-memory high-water
-- mark would silently reset to the current PnL on every restart, quietly telling the model a
-- round-tripped trade had never been in profit at all.
--
-- NUMERIC per CLAUDE.md §7, like every other monetary/price column.
ALTER TABLE paper_orders
    ADD COLUMN IF NOT EXISTS pnl_max_pct NUMERIC NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS pnl_min_pct NUMERIC NOT NULL DEFAULT 0;
