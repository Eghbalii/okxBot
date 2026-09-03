ALTER TABLE paper_orders
    DROP COLUMN IF EXISTS exchange_order_id,
    DROP COLUMN IF EXISTS exchange_algo_order_id;
