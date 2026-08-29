-- The positions panel (CLAUDE.md §11.4) needs the decision timeframe a paper order's signal fired
-- on, for display alongside the strategy that generated it. This can't be reliably reconstructed
-- from strategy_assignments after the fact — one strategy can be assigned to several bars for the
-- same instrument (CLAUDE.md §9), so the assignment table alone doesn't say which bar THIS order
-- came from. Captured at open time instead, same pattern as strategy_id.
ALTER TABLE paper_orders
    ADD COLUMN IF NOT EXISTS bar TEXT;
