DROP TABLE IF EXISTS token_budgets;

DROP INDEX IF EXISTS idx_paper_orders_parent;

ALTER TABLE paper_orders
    DROP COLUMN IF EXISTS variant,
    DROP COLUMN IF EXISTS parent_order_id;
