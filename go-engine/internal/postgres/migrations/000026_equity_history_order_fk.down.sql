-- Null out any order_id that does not exist in paper_orders (i.e. every real-mode row) before
-- restoring the constraint, so the down migration cannot fail on data the up migration made valid.
UPDATE account_equity_history h
SET order_id = NULL
WHERE order_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM paper_orders p WHERE p.id = h.order_id);

ALTER TABLE account_equity_history
    ADD CONSTRAINT account_equity_history_order_id_fkey FOREIGN KEY (order_id) REFERENCES paper_orders(id);
