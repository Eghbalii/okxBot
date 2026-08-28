ALTER TABLE paper_orders
    DROP COLUMN IF EXISTS pnl_max_pct,
    DROP COLUMN IF EXISTS pnl_min_pct;
