-- Exchange-order-ID fields for real trading (CLAUDE.md §27, real-order-lifecycle plan commit 3).
-- Nullable — only RealTrader's open path ever populates them; paper/demo rows have neither.
-- exchange_order_id is OKX's own ordId for the entry order itself; exchange_algo_order_id is the
-- ID OKX assigns the resting SL/TP algo/conditional order, needed later to amend or cancel it
-- (§27.3's "no fork" mechanic edits the real order in place via AmendAlgoOrder, not a shadow
-- fork). No index: display/audit fields, not queried by.
ALTER TABLE paper_orders
    ADD COLUMN IF NOT EXISTS exchange_order_id TEXT,
    ADD COLUMN IF NOT EXISTS exchange_algo_order_id TEXT;
