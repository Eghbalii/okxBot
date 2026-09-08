-- account_equity_history.order_id has referenced paper_orders(id) since it was added, from a time
-- when paper_orders held every mode's rows. Migration 000019 gave real trading its own real_orders
-- table, and a single column cannot carry a foreign key into two tables — so every real close has
-- been failing this constraint and silently writing NO equity-history row at all:
--
--   ERROR: insert or update on table "account_equity_history" violates foreign key constraint
--   DETAIL: Key (order_id)=(3) is not present in table "paper_orders".
--
-- Observed on real order 3 (2026-09-08): the position closed and its PnL was recorded on the order
-- itself, but the account timeline — the thing that exists specifically so an overnight
-- drain-and-reset is reviewable after the fact (CLAUDE.md §15.7) — recorded nothing for it.
--
-- The constraint is dropped rather than re-pointed: which table a row's order_id refers to is
-- already determined by its own mode column ('paper' -> paper_orders, 'real' -> real_orders), so
-- the reference stays meaningful, it just cannot be enforced by a single FK. Postgres has no
-- polymorphic foreign key, and the alternatives (two nullable columns, or a CHECK with a trigger)
-- buy enforcement at a cost in complexity this table does not justify — it is an append-only
-- audit log, not a place rows are joined back from in a hot path.
ALTER TABLE account_equity_history DROP CONSTRAINT account_equity_history_order_id_fkey;

COMMENT ON COLUMN account_equity_history.order_id IS
    'Order this balance change came from. Which table it refers to is given by this row''s mode: '
    'paper -> paper_orders(id), real -> real_orders(id). Not enforced by a foreign key, since one '
    'column cannot reference two tables.';
